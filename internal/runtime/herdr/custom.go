package herdr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

type paneRevStatus struct {
	rev    uint64
	at     time.Time
	status string
}

var (
	customWorkingRe       = regexp.MustCompile(`(?i)(esc to interrupt|streaming\s*·|task running|[✢✳✶✻✽✹⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏].*\(\d+s)`)
	customBottomWorkingRe = regexp.MustCompile(`(?im)^\s*[✢✳✶✻✽✹⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏◐◓◑◒◴◷◶◵]\s*(thinking|pondering|working|streaming|brewing|churning|running|executing|generating|connecting|retrying|waiting|printing|reading|writing|searching|editing|analyzing|思考中|处理中|执行中|生成中)`)
	// Exclude static idle prefix '✳' used by Claude Code / CodeBuddy window titles when idle.
	titleSpinnerRe     = regexp.MustCompile(`^[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏◐◓◑◒◴◷◶◵]`)
	customReadyRe      = regexp.MustCompile(`(?i)(codebuddy code|bypass permissions|do you trust|shift\+tab to cycle|^>\s*$)`)
	customIdleChromeRe = regexp.MustCompile(`(?i)(bypass permissions on|← for agents|↵\s*send|⇧tab to cycle|shift\+tab to cycle)`)
	customChoiceRe     = regexp.MustCompile(`(?m)^\s*([›>•❯▶]\s+.+|\d+[.)]\s+.+)$`)
	customAskHintRe    = regexp.MustCompile(`(?i)(do you trust|trust (this|the files|the contents)|do you want to|allow|proceed|confirm|esc to cancel|yes,?\s*(i trust|allow|proceed))`)
)

// SetCustomStore configures on-disk persistence for shell-fallback agents (e.g. codebuddy)
// so node/hub restarts or tunnel reconnects never lose track of running panes.
func (c *Client) SetCustomStore(path string) {
	if path == "" {
		return
	}
	var loaded map[string]model.Agent
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &loaded)
	}
	c.customMu.Lock()
	c.customPath = path
	if c.custom == nil {
		c.custom = map[string]model.Agent{}
	}
	for k, v := range loaded {
		if _, exists := c.custom[k]; !exists {
			c.custom[k] = v
		}
	}
	c.customMu.Unlock()
}

// InheritCustom copies tracked shell-fallback panes from a replaced remote tunnel client.
func (c *Client) InheritCustom(other *Client) {
	if other == nil {
		return
	}
	other.customMu.Lock()
	cp := make(map[string]model.Agent, len(other.custom))
	for k, v := range other.custom {
		cp[k] = v
	}
	other.customMu.Unlock()
	if len(cp) == 0 {
		return
	}
	c.customMu.Lock()
	if c.custom == nil {
		c.custom = map[string]model.Agent{}
	}
	for k, v := range cp {
		if _, exists := c.custom[k]; !exists {
			c.custom[k] = v
		}
	}
	path := c.customPath
	snap := copyCustomMap(c.custom)
	c.customMu.Unlock()
	persistCustomFile(path, snap)
}

func (c *Client) saveCustomSnapshot() {
	c.customMu.Lock()
	path := c.customPath
	snap := copyCustomMap(c.custom)
	c.customMu.Unlock()
	persistCustomFile(path, snap)
}

func copyCustomMap(m map[string]model.Agent) map[string]model.Agent {
	out := make(map[string]model.Agent, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func persistCustomFile(path string, data map[string]model.Agent) {
	if path == "" {
		return
	}
	if len(data) == 0 {
		_ = os.Remove(path)
		return
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, append(b, '\n'), 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// inferCustomStatus inspects a shell-fallback pane's live screen with a short 1.2s cache TTL
// (since herdr does not bump pane.revision on raw shell output), accurately classifying
// CodeBuddy into "blocked", "working", or "idle".
func (c *Client) inferCustomStatus(ctx context.Context, paneID, termTitle string, rev uint64) string {
	now := time.Now()
	var prevStatus string
	c.customMu.Lock()
	if c.revCache != nil {
		if prev, ok := c.revCache[paneID]; ok {
			prevStatus = prev.status
			if prev.rev == rev && now.Sub(prev.at) < 1200*time.Millisecond && prev.status != "" {
				c.customMu.Unlock()
				return prev.status
			}
		}
	}
	c.customMu.Unlock()

	status := "idle"
	if rr, err := c.Read(ctx, paneID, "detection", "text", 0); err == nil {
		status = classifyCustomScreen(rr.Text)
	} else if prevStatus != "" {
		// Preserve last known status across transient pane.read failures instead of flashing idle.
		status = prevStatus
	}
	if status == "idle" && titleSpinnerRe.MatchString(strings.TrimSpace(termTitle)) {
		status = "working"
	}

	c.customMu.Lock()
	if c.revCache == nil {
		c.revCache = map[string]paneRevStatus{}
	}
	c.revCache[paneID] = paneRevStatus{rev: rev, at: now, status: status}
	c.customMu.Unlock()
	return status
}

// waitCustomBoot waits up to 10s for a newly launched shell-fallback TUI (local or remote)
// to draw its prompt or trust dialog before prompts are pasted into the pane.
func (c *Client) waitCustomBoot(ctx context.Context, paneID string) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rr, err := c.Read(ctx, paneID, "detection", "text", 0)
		if err != nil || strings.TrimSpace(rr.Text) == "" {
			return
		}
		if customReadyRe.MatchString(rr.Text) || classifyCustomScreen(rr.Text) != "idle" {
			select {
			case <-ctx.Done():
			case <-time.After(250 * time.Millisecond):
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(450 * time.Millisecond):
		}
	}
}

func classifyCustomScreen(screen string) string {
	lines := strings.Split(screen, "\n")
	if len(lines) > 26 {
		lines = lines[len(lines)-26:]
	}
	tail := strings.Join(lines, "\n")
	if customWorkingRe.MatchString(tail) {
		return "working"
	}
	// If the live CLI input chrome is visible at the bottom (without an active cancel hint),
	// any numbered list in scrollback above it is normal history, not a blocking dialog.
	var nonEmpty []string
	for _, l := range lines {
		if t := strings.TrimSpace(l); t != "" {
			nonEmpty = append(nonEmpty, t)
		}
	}
	bottom := nonEmpty
	if len(bottom) > 8 {
		bottom = bottom[len(bottom)-8:]
	}
	bottomJoined := strings.Join(bottom, "\n")
	if customBottomWorkingRe.MatchString(bottomJoined) {
		return "working"
	}
	chromeLines := nonEmpty
	if len(chromeLines) > 6 {
		chromeLines = chromeLines[len(chromeLines)-6:]
	}
	chromeJoined := strings.Join(chromeLines, "\n")
	if customIdleChromeRe.MatchString(chromeJoined) && !strings.Contains(strings.ToLower(chromeJoined), "esc to cancel") {
		return "idle"
	}
	if customAskHintRe.MatchString(tail) && len(customChoiceRe.FindAllString(tail, 4)) >= 2 {
		return "blocked"
	}
	return "idle"
}

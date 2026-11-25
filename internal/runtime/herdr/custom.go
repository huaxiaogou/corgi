package herdr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
)

type paneRevStatus struct {
	rev    uint64
	status string
}

var (
	customWorkingRe = regexp.MustCompile(`(?i)(esc to interrupt|streaming\s*·|task running|[✢✳✶✻✽✹⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏].*\(\d+s)`)
	customChoiceRe  = regexp.MustCompile(`(?m)^\s*([›>•❯▶]\s+.+|\d+[.)]\s+.+)$`)
	customAskHintRe = regexp.MustCompile(`(?i)(do you trust|trust (this|the files|the contents)|do you want to|allow|proceed|confirm|esc to cancel|yes,?\s*(i trust|allow|proceed))`)
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

// inferCustomStatus inspects a shell-fallback pane's screen only when its terminal revision advances,
// accurately classifying CodeBuddy into "blocked", "working", or "idle".
func (c *Client) inferCustomStatus(ctx context.Context, paneID string, rev uint64) string {
	c.customMu.Lock()
	if c.revCache != nil {
		if prev, ok := c.revCache[paneID]; ok && prev.rev == rev && prev.status != "" {
			c.customMu.Unlock()
			return prev.status
		}
	}
	c.customMu.Unlock()

	status := "idle"
	if rr, err := c.Read(ctx, paneID, "detection", "text", 0); err == nil {
		status = classifyCustomScreen(rr.Text)
	}

	c.customMu.Lock()
	if c.revCache == nil {
		c.revCache = map[string]paneRevStatus{}
	}
	c.revCache[paneID] = paneRevStatus{rev: rev, status: status}
	c.customMu.Unlock()
	return status
}

func classifyCustomScreen(screen string) string {
	lines := strings.Split(screen, "\n")
	if len(lines) > 26 {
		lines = lines[len(lines)-26:]
	}
	tail := strings.Join(lines, "\n")
	if customAskHintRe.MatchString(tail) && len(customChoiceRe.FindAllString(tail, 4)) >= 2 {
		return "blocked"
	}
	if customWorkingRe.MatchString(tail) {
		return "working"
	}
	return "idle"
}

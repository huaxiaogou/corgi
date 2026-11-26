package herdr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	defaultStartRetryInterval = 150 * time.Millisecond
	defaultStartRetryTimeout  = 10 * time.Second
)

type shellPaneState int

const (
	shellPaneUnknown shellPaneState = iota
	shellPaneReady
	shellPaneBusy
	shellPaneUnavailable
)

// PaneProcess describes one foreground process reported by herdr's pane.process_info.
type PaneProcess struct {
	PID     uint32   `json:"pid"`
	Name    string   `json:"name"`
	Argv0   *string  `json:"argv0"`
	Argv    []string `json:"argv"`
	Cmdline *string  `json:"cmdline"`
}

// PaneProcessInfo describes a pane's shell and active foreground processes.
type PaneProcessInfo struct {
	PaneID                   string        `json:"pane_id"`
	ShellPID                 *uint32       `json:"shell_pid"`
	ForegroundProcessGroupID *uint32       `json:"foreground_process_group_id"`
	ForegroundProcesses      []PaneProcess `json:"foreground_processes"`
}

// IsAtShellPrompt reports whether the pane's foreground process is its idle interactive shell.
func (p *PaneProcessInfo) IsAtShellPrompt() bool {
	return classifyShellProcessInfo(p) == shellPaneReady
}

type paneProcess = PaneProcess
type paneProcessInfo = PaneProcessInfo

// ProcessInfo queries herdr's pane.process_info for paneID.
func (c *Client) ProcessInfo(ctx context.Context, paneID string) (*PaneProcessInfo, error) {
	var r struct {
		ProcessInfo *PaneProcessInfo `json:"process_info"`
	}
	if err := c.Call(ctx, "pane.process_info", map[string]any{"pane_id": paneID}, &r); err != nil {
		return nil, err
	}
	return r.ProcessInfo, nil
}

// StartAgent launches an agent inside paneID.
// When a freshly created workspace's login shell is still running startup rc files
// (e.g. .zprofile/.zshrc), herdr briefly rejects agent.start with agent_pane_busy;
// StartAgent retries within a bounded window while failing fast on panes that are
// already occupied by an agent or are not backed by an interactive shell.
// If herdr does not natively support kind (e.g. codebuddy), it verifies the target
// pane is an available shell before falling back to SendText and tracking it locally.
func (c *Client) StartAgent(ctx context.Context, paneID, kind, name string, args []string) error {
	if strings.TrimSpace(paneID) == "" {
		return &Error{Code: "agent_pane_not_found", Message: "agent target pane id is required"}
	}
	p := map[string]any{
		"name":       name,
		"kind":       kind,
		"pane_id":    paneID,
		"timeout_ms": 60000,
	}
	if len(args) > 0 {
		p["args"] = args
	}

	deadline := time.Now().Add(c.retryTimeout())
	var err error
	for {
		err = c.Call(ctx, "agent.start", p, nil)
		if err == nil {
			c.clearCustomPane(paneID)
			return nil
		}
		if !isPaneBusyErr(err) {
			break
		}
		if occupied, occErr := c.paneOccupiedByAgent(ctx, paneID); occErr != nil && isNotFoundErr(occErr) {
			return occErr
		} else if occupied {
			return err
		}
		if state, pErr := c.inspectShellPane(ctx, paneID); pErr != nil {
			if isNotFoundErr(pErr) {
				return pErr
			}
		} else if state == shellPaneUnavailable {
			return err
		}
		if waitErr := sleepRetry(ctx, deadline, c.retryInterval()); waitErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}

	if !isUnsupportedKindErr(err) {
		return err
	}
	return c.startCustomAgent(ctx, paneID, kind, name, args, err)
}

func (c *Client) startCustomAgent(ctx context.Context, paneID, kind, name string, args []string, origErr error) error {
	bin := kind
	if c.home != "" {
		resolved := FindBin(kind, c.home)
		if resolved == "" {
			return origErr
		}
		bin = resolved
	}
	// Herdr validates agent kind before pane availability, so unsupported kinds still
	// require verifying paneID is an available shell before injecting any command.
	if err := c.waitForAvailableShell(ctx, paneID); err != nil {
		return err
	}
	cmdLine := shellJoin(append([]string{bin}, args...))
	if sendErr := c.SendText(ctx, paneID, cmdLine, true); sendErr != nil {
		return sendErr
	}
	c.waitCustomBoot(ctx, paneID)
	n := name
	k := kind
	c.customMu.Lock()
	if c.custom == nil {
		c.custom = map[string]model.Agent{}
	}
	c.custom[paneID] = model.Agent{
		PaneID:       paneID,
		Agent:        &k,
		DisplayAgent: &k,
		Name:         &n,
		AgentStatus:  "idle",
		Ready:        true,
	}
	c.customMu.Unlock()
	c.saveCustomSnapshot()
	return nil
}

func (c *Client) waitForAvailableShell(ctx context.Context, paneID string) error {
	busyErr := &Error{
		Code:    "agent_pane_busy",
		Message: fmt.Sprintf("agent target pane %s is not an available shell", paneID),
	}
	deadline := time.Now().Add(c.retryTimeout())
	for {
		if occupied, occErr := c.paneOccupiedByAgent(ctx, paneID); occErr != nil && isNotFoundErr(occErr) {
			return occErr
		} else if occupied {
			return busyErr
		}
		state, pErr := c.inspectShellPane(ctx, paneID)
		if pErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isNotFoundErr(pErr) {
				return pErr
			}
			return c.verifyPaneFallback(ctx, paneID, busyErr)
		}
		switch state {
		case shellPaneReady:
			return nil
		case shellPaneUnknown:
			return c.verifyPaneFallback(ctx, paneID, busyErr)
		case shellPaneUnavailable:
			return busyErr
		case shellPaneBusy:
			if waitErr := sleepRetry(ctx, deadline, c.retryInterval()); waitErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return busyErr
			}
		}
	}
}

func (c *Client) verifyPaneFallback(ctx context.Context, paneID string, busyErr *Error) error {
	var r struct {
		Panes []model.Pane `json:"panes"`
	}
	if err := c.Call(ctx, "pane.list", nil, &r); err != nil || r.Panes == nil {
		return nil
	}
	for _, p := range r.Panes {
		if p.ID != paneID {
			continue
		}
		if p.Agent != nil && *p.Agent != "" {
			return busyErr
		}
		return nil
	}
	return &Error{Code: "agent_pane_not_found", Message: fmt.Sprintf("agent target pane %s not found", paneID)}
}

func (c *Client) paneOccupiedByAgent(ctx context.Context, paneID string) (bool, error) {
	c.customMu.Lock()
	_, inCustom := c.custom[paneID]
	c.customMu.Unlock()
	if inCustom {
		return true, nil
	}
	var ar struct {
		Agents []model.Agent `json:"agents"`
	}
	if err := c.Call(ctx, "agent.list", nil, &ar); err == nil {
		for _, a := range ar.Agents {
			if a.PaneID == paneID && (a.LaunchPending || (a.Agent != nil && *a.Agent != "") || (a.Name != nil && *a.Name != "")) {
				return true, nil
			}
		}
	}
	var pr struct {
		Pane *struct {
			ID           string  `json:"pane_id"`
			Agent        *string `json:"agent"`
			RestoreError *string `json:"restore_error"`
		} `json:"pane"`
	}
	if err := c.Call(ctx, "pane.get", map[string]any{"pane_id": paneID}, &pr); err != nil {
		if isNotFoundErr(err) {
			return false, err
		}
	} else if pr.Pane != nil {
		if (pr.Pane.Agent != nil && *pr.Pane.Agent != "") || (pr.Pane.RestoreError != nil && *pr.Pane.RestoreError != "") {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) inspectShellPane(ctx context.Context, paneID string) (shellPaneState, error) {
	var r struct {
		ProcessInfo *paneProcessInfo `json:"process_info"`
	}
	if err := c.Call(ctx, "pane.process_info", map[string]any{"pane_id": paneID}, &r); err != nil {
		return shellPaneUnknown, err
	}
	if r.ProcessInfo == nil {
		return shellPaneUnknown, nil
	}
	return classifyShellProcessInfo(r.ProcessInfo), nil
}

func classifyShellProcessInfo(info *paneProcessInfo) shellPaneState {
	if info == nil {
		return shellPaneUnknown
	}
	if info.ShellPID == nil || *info.ShellPID == 0 {
		return shellPaneUnavailable
	}
	if info.ForegroundProcessGroupID == nil || *info.ForegroundProcessGroupID != *info.ShellPID {
		return shellPaneBusy
	}
	if len(info.ForegroundProcesses) != 1 {
		return shellPaneBusy
	}
	fg := info.ForegroundProcesses[0]
	if fg.PID != *info.ShellPID {
		return shellPaneBusy
	}
	name := fg.Name
	if name == "" && fg.Argv0 != nil {
		name = *fg.Argv0
	}
	if !isKnownShellName(name) {
		return shellPaneUnavailable
	}
	return shellPaneReady
}

func (c *Client) clearCustomPane(paneID string) {
	c.customMu.Lock()
	_, had := c.custom[paneID]
	delete(c.custom, paneID)
	delete(c.revCache, paneID)
	c.customMu.Unlock()
	if had {
		c.saveCustomSnapshot()
	}
}

func (c *Client) retryInterval() time.Duration {
	if c.startRetryInterval > 0 {
		return c.startRetryInterval
	}
	return defaultStartRetryInterval
}

func (c *Client) retryTimeout() time.Duration {
	if c.startRetryTimeout > 0 {
		return c.startRetryTimeout
	}
	return defaultStartRetryTimeout
}

func sleepRetry(ctx context.Context, deadline time.Time, interval time.Duration) error {
	rem := time.Until(deadline)
	if rem <= 0 {
		return context.DeadlineExceeded
	}
	wait := interval
	if rem < wait {
		wait = rem
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func isPaneBusyErr(err error) bool {
	var he *Error
	return errors.As(err, &he) && he.Code == "agent_pane_busy"
}

func isNotFoundErr(err error) bool {
	var he *Error
	return errors.As(err, &he) && strings.Contains(he.Code, "not_found")
}

func isUnsupportedKindErr(err error) bool {
	var he *Error
	if !errors.As(err, &he) {
		return false
	}
	if he.Code == "agent_not_ready" {
		return false
	}
	low := strings.ToLower(he.Code + " " + he.Message)
	return strings.Contains(low, "unsupported") ||
		strings.Contains(low, "unknown agent") ||
		strings.Contains(low, "unknown kind") ||
		strings.Contains(low, "invalid kind")
}

func isKnownShellName(raw string) bool {
	s := strings.TrimSpace(raw)
	s = strings.TrimLeft(s, "-")
	if idx := strings.LastIndexAny(s, "/\\"); idx >= 0 {
		s = s[idx+1:]
	}
	s = strings.ToLower(strings.TrimSuffix(s, ".exe"))
	switch s {
	case "sh", "bash", "zsh", "fish", "dash", "ash", "ksh", "ksh93", "mksh", "pdksh",
		"csh", "tcsh", "nu", "nushell", "elvish", "xonsh", "pwsh", "powershell", "cmd",
		"oil", "osh", "ysh", "ion", "murex":
		return true
	default:
		return false
	}
}

func shellJoin(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		if a == "" {
			parts = append(parts, "''")
			continue
		}
		safe := true
		for _, r := range a {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
				r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == '=' || r == '@') {
				safe = false
				break
			}
		}
		if safe {
			parts = append(parts, a)
		} else {
			parts = append(parts, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
		}
	}
	return strings.Join(parts, " ")
}

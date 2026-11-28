package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

// Error represents a structured error returned by herdr's JSON protocol.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return e.Code + ": " + e.Message
}

// DialFunc opens a connection to a local or tunneled herdr socket.
type DialFunc func(ctx context.Context) (net.Conn, error)

// Client speaks herdr's newline-delimited JSON socket API.
type Client struct {
	dial               DialFunc
	home               string
	seq                atomic.Uint64
	readRetryDelay     time.Duration
	startRetryInterval time.Duration
	startRetryTimeout  time.Duration
	customMu           sync.Mutex
	customPath         string
	custom             map[string]model.Agent // fallback-launched agents (e.g. codebuddy when herdr lacks native kind)
	revCache           map[string]paneRevStatus
}

// NewClient wraps an arbitrary dialer (used for remote yamux tunnel streams).
func NewClient(dial DialFunc) *Client {
	return &Client{dial: dial, readRetryDelay: defaultHerdrReadRetryDelay}
}

const (
	defaultHerdrBootWait       = 10 * time.Second
	defaultHerdrReconnectWait  = 4 * time.Second
	defaultHerdrStartCooldown  = 30 * time.Second
	defaultHerdrPollInterval   = 150 * time.Millisecond
	defaultHerdrReadRetryDelay = 150 * time.Millisecond
)

// UnixDialer coordinates local herdr Unix socket connections, shared boot-wait windows,
// daemon restart grace periods, and auto-start fallback without package-level mutable state.
type UnixDialer struct {
	socketPath    string
	home          string
	logger        *slog.Logger
	bootWait      time.Duration
	reconnectWait time.Duration
	startCooldown time.Duration
	pollInterval  time.Duration
	startDaemonFn func(home string, logger *slog.Logger) bool

	mu            sync.Mutex
	everConnected bool
	bootUntil     time.Time
	lastStartAt   time.Time
}

// NewUnixDialer creates an isolated local Unix socket dialer for one Hub/Node instance.
func NewUnixDialer(socketPath, home string, logger *slog.Logger) *UnixDialer {
	return &UnixDialer{
		socketPath:    filepath.Clean(socketPath),
		home:          home,
		logger:        logger,
		bootWait:      defaultHerdrBootWait,
		reconnectWait: defaultHerdrReconnectWait,
		startCooldown: defaultHerdrStartCooldown,
		pollInterval:  defaultHerdrPollInterval,
		startDaemonFn: startHerdrDaemon,
	}
}

// NewUnixClient connects to a local herdr Unix socket and automatically starts
// `herdr server` in the background if the socket is unavailable.
func NewUnixClient(socketPath, home string, logger *slog.Logger) *Client {
	ud := NewUnixDialer(socketPath, home, logger)
	return &Client{
		home:           home,
		readRetryDelay: defaultHerdrReadRetryDelay,
		dial:           ud.Dial,
	}
}

// Dial connects to the configured local herdr Unix socket with shared boot-wait
// coordination, brief reconnect waiting across daemon restarts, and auto-start fallback.
func (u *UnixDialer) Dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer

	if c, err := d.DialContext(ctx, "unix", u.socketPath); err == nil {
		u.markConnected()
		return c, nil
	} else {
		firstErr := err
		// If this socket was previously connected, allow a short grace window for an in-flight
		// daemon restart before attempting to spawn a new herdr server.
		if u.wasConnected() {
			if c2, ok := u.pollUnixDial(ctx, &d, time.Now().Add(u.effectiveReconnectWait())); ok {
				u.markConnected()
				return c2, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}

		// Share any active boot-wait window across concurrent callers, or initiate a fresh start.
		bootUntil, started := u.ensureBootWindow()
		if !bootUntil.IsZero() && time.Now().Before(bootUntil) {
			if c2, ok := u.pollUnixDial(ctx, &d, bootUntil); ok {
				u.markConnected()
				return c2, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if started {
				return nil, fmt.Errorf("herdr server did not become ready at %s: %w", u.socketPath, firstErr)
			}
		}
		return nil, fmt.Errorf("herdr socket unavailable (%s): %w", u.socketPath, firstErr)
	}
}

func (u *UnixDialer) effectiveBootWait() time.Duration {
	if u.bootWait > 0 {
		return u.bootWait
	}
	return defaultHerdrBootWait
}

func (u *UnixDialer) effectiveReconnectWait() time.Duration {
	if u.reconnectWait > 0 {
		return u.reconnectWait
	}
	return defaultHerdrReconnectWait
}

func (u *UnixDialer) effectiveStartCooldown() time.Duration {
	if u.startCooldown > 0 {
		return u.startCooldown
	}
	return defaultHerdrStartCooldown
}

func (u *UnixDialer) pollUnixDial(ctx context.Context, d *net.Dialer, until time.Time) (net.Conn, bool) {
	poll := u.pollInterval
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	for {
		if c, err := d.DialContext(ctx, "unix", u.socketPath); err == nil {
			return c, true
		}
		rem := time.Until(until)
		if rem <= 0 {
			return nil, false
		}
		wait := poll
		if rem < wait {
			wait = rem
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, false
		case <-timer.C:
		}
	}
}

func (u *UnixDialer) markConnected() {
	u.mu.Lock()
	u.everConnected = true
	u.bootUntil = time.Time{}
	u.lastStartAt = time.Time{} // reset cooldown once healthy so a later restart recovers immediately
	u.mu.Unlock()
}

func (u *UnixDialer) wasConnected() bool {
	u.mu.Lock()
	ok := u.everConnected
	u.mu.Unlock()
	return ok
}

func (u *UnixDialer) ensureBootWindow() (bootUntil time.Time, started bool) {
	now := time.Now()
	u.mu.Lock()
	if !u.bootUntil.IsZero() && now.Before(u.bootUntil) {
		until := u.bootUntil
		u.mu.Unlock()
		return until, false
	}
	if !u.lastStartAt.IsZero() && now.Sub(u.lastStartAt) < u.effectiveStartCooldown() {
		u.mu.Unlock()
		return time.Time{}, false
	}
	u.lastStartAt = now
	u.bootUntil = now.Add(u.effectiveBootWait())
	until := u.bootUntil
	startFn := u.startDaemonFn
	u.mu.Unlock()

	if startFn == nil {
		startFn = startHerdrDaemon
	}
	if !startFn(u.home, u.logger) {
		u.mu.Lock()
		if u.bootUntil.Equal(until) {
			u.bootUntil = time.Time{}
		}
		u.mu.Unlock()
		return time.Time{}, false
	}
	return until, true
}

func startHerdrDaemon(home string, logger *slog.Logger) bool {
	bin := FindBin("herdr", home)
	if bin == "" {
		return false
	}
	cmd := exec.Command(bin, "server")
	cmd.Dir = home
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		if logger != nil {
			logger.Warn("failed to auto-start herdr server", "err", err)
		}
		return false
	}
	if logger != nil {
		logger.Info("auto-started herdr server", "pid", cmd.Process.Pid)
	}
	go func() { _ = cmd.Wait() }()
	return true
}

// FindBin locates a CLI binary on PATH or common user installation directories.
func FindBin(name, home string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	candidates := []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, ".cargo", "bin", name),
		filepath.Join(home, "go", "bin", name),
		filepath.Join(home, ".npm-global", "bin", name),
		"/opt/homebrew/bin/" + name,
		"/usr/local/bin/" + name,
	}
	for _, pat := range []string{
		"/opt/homebrew/opt/node*/bin/" + name,
		filepath.Join(home, ".nvm", "versions", "node", "*", "bin", name),
		filepath.Join(home, ".volta", "bin", name),
	} {
		if matches, _ := filepath.Glob(pat); len(matches) > 0 {
			// Reverse glob order so newer Node versions (e.g. v24 before v20) win.
			for i := len(matches) - 1; i >= 0; i-- {
				candidates = append(candidates, matches[i])
			}
		}
	}
	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// Call executes one NDJSON RPC method against herdr.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	raw, err := c.CallRaw(ctx, method, params)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode herdr %s response: %w", method, err)
	}
	return nil
}

// CallRaw executes one NDJSON RPC method and returns the raw JSON result.
// Idempotent read RPCs retry once on mid-flight transport disconnects (EOF/reset);
// mutating RPCs never retry automatically.
func (c *Client) CallRaw(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c == nil || c.dial == nil {
		return nil, fmt.Errorf("herdr client unavailable")
	}
	if params == nil {
		params = map[string]any{}
	}
	raw, err := c.callRawOnce(ctx, method, params)
	if err == nil || !isIdempotentReadMethod(method) || !isRetryableTransportErr(err) || ctx.Err() != nil {
		return raw, err
	}
	delay := c.readRetryDelay
	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, err
		case <-timer.C:
		}
	}
	return c.callRawOnce(ctx, method, params)
}

func isIdempotentReadMethod(method string) bool {
	switch method {
	case "workspace.list", "pane.list", "agent.list", "agent.get", "pane.get", "pane.read", "pane.layout":
		return true
	default:
		return false
	}
}

func isRetryableTransportErr(err error) bool {
	if err == nil {
		return false
	}
	var he *Error
	if errors.As(err, &he) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && !ne.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "eof") || strings.Contains(msg, "connection reset") || strings.Contains(msg, "broken pipe")
}

func (c *Client) callRawOnce(ctx context.Context, method string, params any) (json.RawMessage, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	id := fmt.Sprintf("cg-%d", c.seq.Add(1))
	req, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, fmt.Errorf("encode herdr %s request: %w", method, err)
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, fmt.Errorf("write herdr %s: %w", method, err)
	}

	rd := bufio.NewReaderSize(conn, 256*1024)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("read herdr %s: %w", method, err)
		}
		var env struct {
			ID     string          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *Error          `json:"error"`
		}
		if json.Unmarshal(line, &env) != nil || env.ID != id {
			continue
		}
		if env.Error != nil {
			return nil, env.Error
		}
		return env.Result, nil
	}
}

// Workspaces lists herdr workspaces.
func (c *Client) Workspaces(ctx context.Context) ([]model.Workspace, error) {
	var r struct {
		Workspaces []model.Workspace `json:"workspaces"`
	}
	err := c.Call(ctx, "workspace.list", nil, &r)
	return r.Workspaces, err
}

// Panes lists herdr terminal panes.
func (c *Client) Panes(ctx context.Context) ([]model.Pane, error) {
	var r struct {
		Panes []model.Pane `json:"panes"`
	}
	err := c.Call(ctx, "pane.list", nil, &r)
	return r.Panes, err
}

// Agents lists managed agents known to herdr, merged with any shell-fallback agents still alive.
func (c *Client) Agents(ctx context.Context) ([]model.Agent, error) {
	var r struct {
		Agents []model.Agent `json:"agents"`
	}
	err := c.Call(ctx, "agent.list", nil, &r)
	if err != nil {
		return nil, err
	}

	c.customMu.Lock()
	if len(c.custom) == 0 {
		c.customMu.Unlock()
		return r.Agents, nil
	}
	snap := make(map[string]model.Agent, len(c.custom))
	for k, v := range c.custom {
		snap[k] = v
	}
	c.customMu.Unlock()

	present := make(map[string]bool, len(r.Agents))
	for _, a := range r.Agents {
		present[a.PaneID] = true
	}
	panes, pErr := c.Panes(ctx)
	livePanes := make(map[string]model.Pane, len(panes))
	for _, p := range panes {
		livePanes[p.ID] = p
	}

	var stale []string
	for pid, ca := range snap {
		if present[pid] {
			stale = append(stale, pid)
			continue
		}
		if pErr == nil {
			lp, ok := livePanes[pid]
			if !ok {
				stale = append(stale, pid)
				continue
			}
			ca.WorkspaceID = lp.WorkspaceID
			ca.Cwd = lp.Cwd
			if lp.Title != nil {
				ca.TerminalTitle = *lp.Title
			}
			ca.AgentStatus = c.inferCustomStatus(ctx, pid, ca.TerminalTitle, lp.Revision)
		} else {
			c.customMu.Lock()
			if prev, ok := c.revCache[pid]; ok && prev.status != "" {
				ca.AgentStatus = prev.status
			}
			c.customMu.Unlock()
		}
		r.Agents = append(r.Agents, ca)
	}
	if len(stale) > 0 {
		c.customMu.Lock()
		for _, pid := range stale {
			delete(c.custom, pid)
			delete(c.revCache, pid)
		}
		c.customMu.Unlock()
		c.saveCustomSnapshot()
	}
	return r.Agents, nil
}

// Read reads lines from a pane with source ("recent", "recent_unwrapped", "detection", "visible")
// and format ("ansi", "text").
func (c *Client) Read(ctx context.Context, paneID, source, format string, lines int) (model.ReadResult, error) {
	p := map[string]any{
		"pane_id":    paneID,
		"source":     source,
		"format":     format,
		"strip_ansi": format != "ansi",
	}
	if lines > 0 {
		p["lines"] = lines
	}
	var r struct {
		Read model.ReadResult `json:"read"`
	}
	err := c.Call(ctx, "pane.read", p, &r)
	return r.Read, err
}

// PaneSize returns width/height of a pane.
func (c *Client) PaneSize(ctx context.Context, paneID string) (model.Rect, error) {
	var r struct {
		Layout struct {
			Panes []struct {
				ID   string     `json:"pane_id"`
				Rect model.Rect `json:"rect"`
			} `json:"panes"`
		} `json:"layout"`
	}
	if err := c.Call(ctx, "pane.layout", map[string]any{"pane_id": paneID}, &r); err != nil {
		return model.Rect{}, err
	}
	for _, p := range r.Layout.Panes {
		if p.ID == paneID {
			return p.Rect, nil
		}
	}
	return model.Rect{Width: 120, Height: 40}, nil
}

// CreateWorkspace creates an unfocused herdr workspace and returns (workspaceID, paneID).
func (c *Client) CreateWorkspace(ctx context.Context, cwd, label string) (string, string, error) {
	p := map[string]any{"label": label, "focus": false}
	if cwd != "" {
		p["cwd"] = cwd
	}
	var r struct {
		Workspace struct {
			ID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane struct {
			ID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	err := c.Call(ctx, "workspace.create", p, &r)
	if err != nil {
		return "", "", err
	}
	if r.Workspace.ID == "" || r.RootPane.ID == "" {
		return "", "", fmt.Errorf("herdr workspace.create returned empty workspace or pane id")
	}
	return r.Workspace.ID, r.RootPane.ID, nil
}

// CloseWorkspace closes a herdr workspace.
func (c *Client) CloseWorkspace(ctx context.Context, workspaceID string) error {
	return c.Call(ctx, "workspace.close", map[string]any{"workspace_id": workspaceID}, nil)
}

// Prompt sends a prompt to an agent by target (name or pane ID).
// Falls back to pane.send_text + enter for shell-fallback agents or when herdr
// marks an already-idle agent as launch_pending / not_ready.
func (c *Client) Prompt(ctx context.Context, target, text string) error {
	if pid := c.lookupCustomPane(target); pid != "" {
		if rr, rErr := c.Read(ctx, pid, "detection", "text", 0); rErr == nil && classifyCustomScreen(rr.Text) == "blocked" {
			return &Error{Code: "agent_blocked", Message: "agent is waiting on a dialog"}
		}
		payload := text
		if strings.Contains(text, "\n") {
			payload = "\x1b[200~" + text + "\x1b[201~"
		}
		if err := c.Call(ctx, "pane.send_text", map[string]any{"pane_id": pid, "text": payload}, nil); err != nil {
			return err
		}
		if strings.Contains(text, "\n") {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		return c.SendKeys(ctx, pid, "enter")
	}
	err := c.Call(ctx, "agent.prompt", map[string]any{"target": target, "text": text}, nil)
	if err == nil {
		return nil
	}
	var he *Error
	if !errors.As(err, &he) || (he.Code != "agent_not_found" && he.Code != "agent_not_ready" && he.Code != "not_found") {
		return err
	}
	pane := target
	if !strings.Contains(pane, ":") {
		if agents, aErr := c.Agents(ctx); aErr == nil {
			for _, a := range agents {
				if a.Name != nil && *a.Name == target {
					pane = a.PaneID
					break
				}
			}
		}
	}
	if !strings.Contains(pane, ":") {
		return err
	}
	var info struct {
		Agent struct {
			Status string `json:"agent_status"`
		} `json:"agent"`
	}
	if c.Call(ctx, "agent.get", map[string]any{"target": pane}, &info) != nil {
		return err
	}
	switch info.Agent.Status {
	case "idle", "done", "working":
	case "blocked":
		return &Error{Code: "agent_blocked", Message: "agent is " + info.Agent.Status}
	default:
		return err
	}
	if rr, rErr := c.Read(ctx, pane, "detection", "text", 0); rErr == nil && classifyCustomScreen(rr.Text) == "blocked" {
		return &Error{Code: "agent_blocked", Message: "agent is waiting on a dialog"}
	}
	return c.SendText(ctx, pane, "\x1b[200~"+text+"\x1b[201~", true)
}

func (c *Client) lookupCustomPane(target string) string {
	c.customMu.Lock()
	defer c.customMu.Unlock()
	if _, ok := c.custom[target]; ok {
		return target
	}
	for pid, a := range c.custom {
		if a.Name != nil && *a.Name == target {
			return pid
		}
	}
	return ""
}

// SendText pastes text into a pane, optionally pressing enter.
func (c *Client) SendText(ctx context.Context, paneID, text string, enter bool) error {
	if err := c.Call(ctx, "pane.send_text", map[string]any{"pane_id": paneID, "text": text}, nil); err != nil {
		return err
	}
	if enter {
		if c.home != "" && len(text) > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(150 * time.Millisecond):
			}
		}
		return c.SendKeys(ctx, paneID, "enter")
	}
	return nil
}

// SendKeys sends key sequences (e.g. "enter", "esc", "ctrl+c", "down") to a pane.
func (c *Client) SendKeys(ctx context.Context, paneID string, keys ...string) error {
	return c.Call(ctx, "pane.send_keys", map[string]any{"pane_id": paneID, "keys": keys}, nil)
}

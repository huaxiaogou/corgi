package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	dial     DialFunc
	home     string
	seq      atomic.Uint64
	customMu sync.Mutex
	custom   map[string]model.Agent // fallback-launched agents (e.g. codebuddy when herdr lacks native kind)
}

// NewClient wraps an arbitrary dialer (used for remote yamux tunnel streams).
func NewClient(dial DialFunc) *Client {
	return &Client{dial: dial}
}

// NewUnixClient connects to a local herdr Unix socket and automatically starts
// `herdr server` in the background if the socket is unavailable.
func NewUnixClient(socketPath, home string, logger *slog.Logger) *Client {
	starter := &serverStarter{home: home, logger: logger}
	return &Client{
		home: home,
		dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			c, err := d.DialContext(ctx, "unix", socketPath)
			if err == nil {
				return c, nil
			}
			if !starter.tryStart() {
				return nil, fmt.Errorf("herdr socket unavailable (%s): %w", socketPath, err)
			}
			for i := 0; i < 40; i++ {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(250 * time.Millisecond):
				}
				if c2, err2 := d.DialContext(ctx, "unix", socketPath); err2 == nil {
					return c2, nil
				}
			}
			return nil, fmt.Errorf("herdr server did not become ready at %s: %w", socketPath, err)
		},
	}
}

type serverStarter struct {
	home   string
	logger *slog.Logger
	mu     sync.Mutex
	lastAt time.Time
}

func (s *serverStarter) tryStart() bool {
	s.mu.Lock()
	if time.Since(s.lastAt) < 30*time.Second {
		s.mu.Unlock()
		return false
	}
	s.lastAt = time.Now()
	s.mu.Unlock()

	bin := FindBin("herdr", s.home)
	if bin == "" {
		return false
	}
	cmd := exec.Command(bin, "server")
	cmd.Dir = s.home
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		if s.logger != nil {
			s.logger.Warn("failed to auto-start herdr server", "err", err)
		}
		return false
	}
	if s.logger != nil {
		s.logger.Info("auto-started herdr server", "pid", cmd.Process.Pid)
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
			candidates = append(candidates, matches...)
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
func (c *Client) CallRaw(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
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
		}
		r.Agents = append(r.Agents, ca)
	}
	if len(stale) > 0 {
		c.customMu.Lock()
		for _, pid := range stale {
			delete(c.custom, pid)
		}
		c.customMu.Unlock()
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
	return r.Workspace.ID, r.RootPane.ID, err
}

// CloseWorkspace closes a herdr workspace.
func (c *Client) CloseWorkspace(ctx context.Context, workspaceID string) error {
	return c.Call(ctx, "workspace.close", map[string]any{"workspace_id": workspaceID}, nil)
}

// StartAgent launches an agent inside paneID.
// If herdr does not natively support kind (e.g. codebuddy on older herdr versions),
// it falls back to launching the CLI command inside the pane and tracking it locally.
func (c *Client) StartAgent(ctx context.Context, paneID, kind, name string, args []string) error {
	p := map[string]any{
		"name":       name,
		"kind":       kind,
		"pane_id":    paneID,
		"timeout_ms": 60000,
	}
	if len(args) > 0 {
		p["args"] = args
	}
	err := c.Call(ctx, "agent.start", p, nil)
	if err == nil {
		c.customMu.Lock()
		delete(c.custom, paneID)
		c.customMu.Unlock()
		return nil
	}
	if !isUnsupportedKindErr(err) {
		return err
	}
	bin := kind
	if c.home != "" {
		resolved := FindBin(kind, c.home)
		if resolved == "" {
			return err
		}
		bin = resolved
	}
	cmdLine := shellJoin(append([]string{bin}, args...))
	if sendErr := c.SendText(ctx, paneID, cmdLine, true); sendErr != nil {
		return err
	}
	if c.home != "" {
		// Give shell-launched TUIs (e.g. codebuddy) a brief moment to boot before accepting prompts.
		select {
		case <-ctx.Done():
		case <-time.After(3500 * time.Millisecond):
		}
	}
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
	return nil
}

// Prompt sends a prompt to an agent by target (name or pane ID).
// Falls back to pane.send_text + enter for shell-fallback agents not registered in herdr.
func (c *Client) Prompt(ctx context.Context, target, text string) error {
	err := c.Call(ctx, "agent.prompt", map[string]any{"target": target, "prompt": text}, nil)
	if err == nil {
		return nil
	}
	if pid := c.lookupCustomPane(target); pid != "" {
		return c.SendText(ctx, pid, text, true)
	}
	return err
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

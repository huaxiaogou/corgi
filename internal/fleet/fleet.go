package fleet

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

// RemoteMachine represents one node dialed in over a yamux tunnel.
type RemoteMachine struct {
	Info       model.Machine
	Herdr      *herdr.Client
	HTTPClient *http.Client
	CloseFn    func()
}

// Fleet multiplexes herdr operations across the local hub host and all connected remote nodes.
// Remote IDs carry "@<machine>" suffixes so callers address panes/workspaces transparently.
type Fleet struct {
	mu       sync.RWMutex
	corgiDir string
	local    model.Machine
	localH   *herdr.Client
	remotes  map[string]*RemoteMachine
	names    map[string]string    // agent name -> suffixed pane ID
	runs     map[string][2]string // pane ID -> [model, effort]
	seenAt   map[string]time.Time
	doneAt   map[string]time.Time
	herdrOK  map[string]bool // machine ID -> whether last herdr list succeeded
	onChange func()
	subs     map[chan struct{}]struct{}
}

// NewFleet creates a Fleet rooted at the hub's local herdr client.
func NewFleet(localHerdr *herdr.Client, hostname, home, version string, localAgents []string) *Fleet {
	return &Fleet{
		local: model.Machine{
			ID:      "",
			Name:    hostname,
			OS:      runtime.GOOS + "/" + runtime.GOARCH,
			Version: version,
			Agents:  localAgents,
			Home:    home,
			Local:   true,
			Online:  true,
			Since:   time.Now(),
		},
		localH:  localHerdr,
		remotes: map[string]*RemoteMachine{},
		names:   map[string]string{},
		runs:    map[string][2]string{},
		seenAt:  map[string]time.Time{},
		doneAt:  map[string]time.Time{},
		herdrOK: map[string]bool{},
		subs:    map[chan struct{}]struct{}{},
	}
}

// MachineHerdrOK reports whether machine's herdr answered its most recent list call
// (default true until proven unreachable so transient restarts don't wipe metadata).
func (f *Fleet) MachineHerdrOK(machine string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if ok, seen := f.herdrOK[machine]; seen {
		return ok
	}
	return true
}

func (f *Fleet) setMachineHerdrOK(machine string, ok bool) {
	f.mu.Lock()
	if f.herdrOK == nil {
		f.herdrOK = map[string]bool{}
	}
	f.herdrOK[machine] = ok
	f.mu.Unlock()
}

// SetCorgiDir configures on-disk custom-agent persistence for both local and remote herdr clients.
func (f *Fleet) SetCorgiDir(dir string) {
	f.mu.Lock()
	f.corgiDir = dir
	lh := f.localH
	f.mu.Unlock()
	if lh != nil && dir != "" {
		lh.SetCustomStore(filepath.Join(dir, "custom_agents.json"))
	}
}

// SetOnChange registers a callback invoked whenever nodes attach or detach.
func (f *Fleet) SetOnChange(fn func()) {
	f.mu.Lock()
	f.onChange = fn
	f.mu.Unlock()
}

// SubscribeChanges returns a buffered notification channel signaled on every Attach/Detach
// together with an unsubscribe function.
func (f *Fleet) SubscribeChanges() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	if f.subs == nil {
		f.subs = map[chan struct{}]struct{}{}
	}
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		delete(f.subs, ch)
		f.mu.Unlock()
	}
}

func (f *Fleet) notifySubscribersLocked() {
	for ch := range f.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// UpdateLocalAgents refreshes the detected installed agent kinds on the hub host.
func (f *Fleet) UpdateLocalAgents(agents []string) {
	f.mu.Lock()
	f.local.Agents = append([]string(nil), agents...)
	f.mu.Unlock()
}

// SplitID turns "w1:p1@macbook" into ("w1:p1", "macbook").
func SplitID(id string) (localID, machine string) {
	if i := strings.LastIndexByte(id, '@'); i >= 0 {
		return id[:i], id[i+1:]
	}
	return id, ""
}

// JoinID turns ("w1:p1", "macbook") into "w1:p1@macbook" (bare for local machine "").
func JoinID(localID, machine string) string {
	if machine == "" || localID == "" {
		return localID
	}
	return localID + "@" + machine
}

// Attach registers (or replaces) a live remote node.
func (f *Fleet) Attach(rm *RemoteMachine) {
	f.mu.Lock()
	old := f.remotes[rm.Info.ID]
	dir := f.corgiDir
	rm.Info.Online = true
	rm.Info.Since = time.Now()
	f.remotes[rm.Info.ID] = rm
	f.notifySubscribersLocked()
	cb := f.onChange
	f.mu.Unlock()

	if rm.Herdr != nil {
		if dir != "" && rm.Info.ID != "" {
			rm.Herdr.SetCustomStore(filepath.Join(dir, "custom_agents_"+rm.Info.ID+".json"))
		}
		if old != nil && old.Herdr != nil {
			rm.Herdr.InheritCustom(old.Herdr)
		}
	}
	if old != nil && old != rm && old.CloseFn != nil {
		old.CloseFn()
	}
	if cb != nil {
		cb()
	}
}

// Detach disconnects a remote node if it matches current (or unconditionally if match == nil).
func (f *Fleet) Detach(id string, match *RemoteMachine) {
	f.mu.Lock()
	cur := f.remotes[id]
	if cur == nil || (match != nil && cur != match) {
		f.mu.Unlock()
		return
	}
	delete(f.remotes, id)
	f.notifySubscribersLocked()
	cb := f.onChange
	f.mu.Unlock()

	if cur.CloseFn != nil {
		cur.CloseFn()
	}
	if cb != nil {
		cb()
	}
}

// Machines returns the hub host first, followed by sorted remote nodes.
func (f *Fleet) Machines() []model.Machine {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := []model.Machine{f.local}
	var remotes []model.Machine
	for _, r := range f.remotes {
		remotes = append(remotes, r.Info)
	}
	sort.Slice(remotes, func(i, j int) bool { return remotes[i].Name < remotes[j].Name })
	return append(out, remotes...)
}

// ResolveMachine maps a machine ID or display name ("" / "hub" / "local" -> "") to its ID.
func (f *Fleet) ResolveMachine(nameOrID string) string {
	n := strings.TrimSpace(nameOrID)
	if n == "" || strings.EqualFold(n, "hub") || strings.EqualFold(n, "local") {
		return ""
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if strings.EqualFold(n, f.local.Name) {
		return ""
	}
	for id, r := range f.remotes {
		if id == n || strings.EqualFold(r.Info.Name, n) {
			return id
		}
	}
	return n
}

// Machine looks up a connected machine ("" / "hub" / "local" for hub host, or remote node ID/name).
func (f *Fleet) Machine(nameOrID string) (model.Machine, bool) {
	id := f.ResolveMachine(nameOrID)
	f.mu.RLock()
	defer f.mu.RUnlock()
	if id == "" {
		return f.local, true
	}
	if r, ok := f.remotes[id]; ok && r.Info.Online {
		return r.Info, true
	}
	return model.Machine{}, false
}

// HTTPClient returns the tunneled HTTP client for a remote node.
func (f *Fleet) HTTPClient(machine string) (*http.Client, error) {
	f.mu.RLock()
	rm := f.remotes[machine]
	f.mu.RUnlock()
	if rm == nil || rm.HTTPClient == nil {
		return nil, fmt.Errorf("machine %q is offline", machine)
	}
	return rm.HTTPClient, nil
}

// RemoteMachineByID looks up a connected remote node strictly by exact device ID.
// Unlike ResolveMachine, it never maps "hub", "local", or a hostname to the local Hub.
func (f *Fleet) RemoteMachineByID(id string) (model.Machine, *http.Client, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return model.Machine{}, nil, false
	}
	f.mu.RLock()
	rm := f.remotes[id]
	f.mu.RUnlock()
	if rm == nil || rm.HTTPClient == nil || !rm.Info.Online {
		return model.Machine{}, nil, false
	}
	return rm.Info, rm.HTTPClient, true
}

// LocalWorkspaces returns workspaces strictly from the Hub's local herdr daemon.
func (f *Fleet) LocalWorkspaces(ctx context.Context) ([]model.Workspace, error) {
	f.mu.RLock()
	lh := f.localH
	f.mu.RUnlock()
	if lh == nil {
		return nil, fmt.Errorf("local herdr unavailable")
	}
	return lh.Workspaces(ctx)
}

func (f *Fleet) clientFor(machine string) (*herdr.Client, error) {
	if machine == "" {
		return f.localH, nil
	}
	f.mu.RLock()
	rm := f.remotes[machine]
	f.mu.RUnlock()
	if rm == nil || rm.Herdr == nil {
		return nil, fmt.Errorf("machine %q is offline", machine)
	}
	return rm.Herdr, nil
}

type targetClient struct {
	machine string
	client  *herdr.Client
}

func (f *Fleet) allClients() []targetClient {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var out []targetClient
	if f.localH != nil {
		out = append(out, targetClient{machine: "", client: f.localH})
	}
	for id, r := range f.remotes {
		if r.Herdr != nil {
			out = append(out, targetClient{machine: id, client: r.Herdr})
		}
	}
	return out
}

// Workspaces fans out workspace.list across all online machines.
func (f *Fleet) Workspaces(ctx context.Context) ([]model.Workspace, error) {
	clients := f.allClients()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		merged   []model.Workspace
		localErr error
	)
	for _, tc := range clients {
		wg.Add(1)
		go func(tc targetClient) {
			defer wg.Done()
			ws, err := tc.client.Workspaces(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if tc.machine == "" {
					localErr = err
				}
				return
			}
			for i := range ws {
				ws[i].ID = JoinID(ws[i].ID, tc.machine)
			}
			merged = append(merged, ws...)
		}(tc)
	}
	wg.Wait()
	return merged, localErr
}

// PanesOnMachine queries pane.list strictly on the single specified machine ("" for local Hub)
// and returns any transport/RPC error directly instead of swallowing it.
func (f *Fleet) PanesOnMachine(ctx context.Context, machine string) ([]model.Pane, error) {
	cl, err := f.clientFor(machine)
	if err != nil {
		return nil, err
	}
	if cl == nil {
		return nil, fmt.Errorf("herdr client unavailable for machine %q", machine)
	}
	ps, err := cl.Panes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ps {
		ps[i].ID = JoinID(ps[i].ID, machine)
		ps[i].WorkspaceID = JoinID(ps[i].WorkspaceID, machine)
	}
	return ps, nil
}

// Panes fans out pane.list across all online machines.
func (f *Fleet) Panes(ctx context.Context) ([]model.Pane, error) {
	clients := f.allClients()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		merged   []model.Pane
		localErr error
	)
	for _, tc := range clients {
		wg.Add(1)
		go func(tc targetClient) {
			defer wg.Done()
			ps, err := tc.client.Panes(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if tc.machine == "" {
					localErr = err
				}
				return
			}
			for i := range ps {
				ps[i].ID = JoinID(ps[i].ID, tc.machine)
				ps[i].WorkspaceID = JoinID(ps[i].WorkspaceID, tc.machine)
			}
			merged = append(merged, ps...)
		}(tc)
	}
	wg.Wait()
	return merged, localErr
}

// Agents fans out agent.list across all online machines and updates the name->pane map.
func (f *Fleet) Agents(ctx context.Context) ([]model.Agent, error) {
	ags, _, err := f.AgentsWithScanned(ctx)
	return ags, err
}

// AgentsWithScanned fans out agent.list across all online machines and also returns
// the set of machine IDs ("" for Hub) whose scan succeeded on this call.
func (f *Fleet) AgentsWithScanned(ctx context.Context) ([]model.Agent, map[string]bool, error) {
	clients := f.allClients()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		merged   []model.Agent
		scanned  = make(map[string]bool, len(clients))
		localErr error
	)
	for _, tc := range clients {
		wg.Add(1)
		go func(tc targetClient) {
			defer wg.Done()
			ags, err := tc.client.Agents(ctx)
			f.setMachineHerdrOK(tc.machine, err == nil)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if tc.machine == "" {
					localErr = err
				}
				return
			}
			scanned[tc.machine] = true
			for i := range ags {
				ags[i].PaneID = JoinID(ags[i].PaneID, tc.machine)
				ags[i].WorkspaceID = JoinID(ags[i].WorkspaceID, tc.machine)
			}
			merged = append(merged, ags...)
		}(tc)
	}
	wg.Wait()

	f.mu.Lock()
	for _, a := range merged {
		if a.Name != nil && *a.Name != "" {
			name := *a.Name
			// Router and Dream are strictly Hub-local system sessions: never let an identically
			// named pane on a remote Node (e.g. a leftover local dev Hub) hijack Hub routing.
			if (name == model.RouterName || name == model.DreamName) && strings.Contains(a.PaneID, "@") {
				continue
			}
			f.names[name] = a.PaneID
		}
	}
	f.mu.Unlock()
	return merged, scanned, localErr
}

// RememberName remembers the pane ID of a named agent.
func (f *Fleet) RememberName(paneID, name string) {
	if paneID == "" || name == "" {
		return
	}
	f.mu.Lock()
	f.names[name] = paneID
	f.mu.Unlock()
}

// RememberRun records the model and effort used when launching paneID.
func (f *Fleet) RememberRun(paneID, modelName, effort string) {
	if paneID == "" {
		return
	}
	f.mu.Lock()
	f.runs[paneID] = [2]string{modelName, effort}
	f.mu.Unlock()
}

// Run returns the recorded launch [model, effort] for paneID.
func (f *Fleet) Run(paneID string) (*[2]string, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	r, ok := f.runs[paneID]
	if !ok {
		return nil, false
	}
	return &r, true
}

// MarkSeen records that the user viewed paneID so "done" reads as "idle".
func (f *Fleet) MarkSeen(paneID string) {
	f.mu.Lock()
	f.seenAt[paneID] = time.Now()
	f.mu.Unlock()
}

// EffectiveStatus adjusts "done" -> "idle" when the pane was already viewed.
func (f *Fleet) EffectiveStatus(paneID, status string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if status != "done" {
		delete(f.doneAt, paneID)
		return status
	}
	since, ok := f.doneAt[paneID]
	if !ok {
		since = time.Now()
		f.doneAt[paneID] = since
	}
	if seen := f.seenAt[paneID]; !seen.IsZero() && !seen.Before(since) {
		return "idle"
	}
	return status
}

func (f *Fleet) resolveTarget(target string) (localTarget, machine string) {
	if strings.Contains(target, ":") || strings.Contains(target, "@") {
		return SplitID(target)
	}
	f.mu.RLock()
	pane, ok := f.names[target]
	f.mu.RUnlock()
	if ok {
		return SplitID(pane)
	}
	return target, ""
}

// CreateWorkspace opens a workspace on machine and returns suffixed (workspaceID, paneID).
func (f *Fleet) CreateWorkspace(ctx context.Context, machine, cwd, label string) (string, string, error) {
	cl, err := f.clientFor(machine)
	if err != nil {
		return "", "", err
	}
	wsID, paneID, err := cl.CreateWorkspace(ctx, cwd, label)
	return JoinID(wsID, machine), JoinID(paneID, machine), err
}

// CloseWorkspace closes a suffixed workspaceID.
func (f *Fleet) CloseWorkspace(ctx context.Context, workspaceID string) error {
	localID, machine := SplitID(workspaceID)
	cl, err := f.clientFor(machine)
	if err != nil {
		return err
	}
	return cl.CloseWorkspace(ctx, localID)
}

// StartAgent starts an agent inside suffixed paneID.
func (f *Fleet) StartAgent(ctx context.Context, paneID, kind, name string, args []string) error {
	localID, machine := SplitID(paneID)
	cl, err := f.clientFor(machine)
	if err != nil {
		return err
	}
	return cl.StartAgent(ctx, localID, kind, name, args)
}

// Prompt sends a prompt to target (agent name or suffixed pane ID).
// If the target pane is currently showing an interactive startup/confirmation dialog,
// Prompt returns agent_blocked instead of letting the dialog swallow the message.
func (f *Fleet) Prompt(ctx context.Context, target, text string) error {
	localTarget, machine := f.resolveTarget(target)
	cl, err := f.clientFor(machine)
	if err != nil {
		return err
	}
	if strings.Contains(localTarget, ":") {
		if rr, rErr := cl.Read(ctx, localTarget, "detection", "text", 0); rErr == nil && agent.ScreenAsks(rr.Text) {
			return &herdr.Error{Code: "agent_blocked", Message: "agent is showing a confirmation dialog"}
		}
	}
	return cl.Prompt(ctx, localTarget, text)
}

// Read reads pane output for suffixed paneID.
func (f *Fleet) Read(ctx context.Context, paneID, source, format string, lines int) (model.ReadResult, error) {
	localID, machine := SplitID(paneID)
	cl, err := f.clientFor(machine)
	if err != nil {
		return model.ReadResult{}, err
	}
	return cl.Read(ctx, localID, source, format, lines)
}

// PaneSize returns terminal dimensions for suffixed paneID.
func (f *Fleet) PaneSize(ctx context.Context, paneID string) (model.Rect, error) {
	localID, machine := SplitID(paneID)
	cl, err := f.clientFor(machine)
	if err != nil {
		return model.Rect{}, err
	}
	return cl.PaneSize(ctx, localID)
}

// SendText types text into suffixed paneID.
func (f *Fleet) SendText(ctx context.Context, paneID, text string, enter bool) error {
	localID, machine := SplitID(paneID)
	cl, err := f.clientFor(machine)
	if err != nil {
		return err
	}
	return cl.SendText(ctx, localID, text, enter)
}

// SendKeys sends key sequences to suffixed paneID.
func (f *Fleet) SendKeys(ctx context.Context, paneID string, keys ...string) error {
	localID, machine := SplitID(paneID)
	cl, err := f.clientFor(machine)
	if err != nil {
		return err
	}
	return cl.SendKeys(ctx, localID, keys...)
}

// ProcessInfo queries foreground process argv/cmdline for suffixed paneID.
func (f *Fleet) ProcessInfo(ctx context.Context, paneID string) (*herdr.PaneProcessInfo, error) {
	localID, machine := SplitID(paneID)
	cl, err := f.clientFor(machine)
	if err != nil {
		return nil, err
	}
	return cl.ProcessInfo(ctx, localID)
}

package core

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

// DirectTurnResult delivers a Worker's direct reply back to an interactive channel (e.g. WeCom)
// while Hub simultaneously snapshots long reports and records background context.
type DirectTurnResult struct {
	WorkerName       string
	Machine          string
	MachineLabel     string
	Kind             string
	FullText         string
	ReportArtifactID string
	PreviewURL       string
	QuotaError       string
}

// DirectWorkerRequest dispatches a user message straight to a live or resumable Worker.
type DirectWorkerRequest struct {
	Name       string
	Machine    string
	Session    string
	Kind       string
	Cwd        string
	WorkItemID string
	Text       string
	OnDone     func(ctx context.Context, res DirectTurnResult)
}

// WorkerPickItem is a concise summary of a live or resumable Worker for channel pickers.
type WorkerPickItem struct {
	Name       string
	Machine    string
	Kind       string
	Status     string
	Session    string
	WorkItemID string
	Ended      bool
}

type pendingDirectTurn struct {
	prompt   string
	at       time.Time
	callback func(ctx context.Context, res DirectTurnResult)
}

// Fleet exposes the underlying multi-machine Fleet for channel streaming helpers.
func (e *Engine) Fleet() *fleet.Fleet { return e.fleet }

// GetKV reads a persisted key-value entry from SQLite.
func (e *Engine) GetKV(ctx context.Context, key string) (string, bool, error) {
	if e.store == nil {
		return "", false, nil
	}
	return e.store.GetKV(ctx, key)
}

// SetKV writes a persisted key-value entry to SQLite.
func (e *Engine) SetKV(ctx context.Context, key, val string) error {
	if e.store == nil {
		return nil
	}
	return e.store.SetKV(ctx, key, val)
}

// DeleteKV removes a persisted key-value entry from SQLite.
func (e *Engine) DeleteKV(ctx context.Context, key string) error {
	if e.store == nil {
		return nil
	}
	return e.store.DeleteKV(ctx, key)
}

// BuildArtifactShareURL returns the public preview URL for artID when public_url is configured.
func (e *Engine) BuildArtifactShareURL(ctx context.Context, artID string) string {
	artID = strings.TrimSpace(artID)
	if artID == "" {
		return ""
	}
	return e.BuildPreviewURL(ctx, artID)
}

// MachineDisplayName maps a machine ID ("" for Hub) to a short human-readable label.
func (e *Engine) MachineDisplayName(machineID string) string {
	machineID = strings.TrimSpace(machineID)
	if machineID == "" || strings.EqualFold(machineID, "hub") {
		return "Hub"
	}
	if e.fleet != nil {
		for _, m := range e.fleet.Machines() {
			if m.ID == machineID {
				if strings.TrimSpace(m.Name) != "" {
					return m.Name
				}
				break
			}
		}
	}
	return machineID
}

// HasQueuedRouterTriggers reports whether any queued triggers are still waiting for Router.
func (e *Engine) HasQueuedRouterTriggers(ctx context.Context) bool {
	if e.store == nil {
		return false
	}
	queued, err := e.store.ListQueuedTriggers(ctx)
	if err != nil {
		return false
	}
	for _, t := range queued {
		if len(t.Targets) == 0 || slices.Contains(t.Targets, model.RouterName) {
			return true
		}
	}
	return false
}

// ListSelectableWorkers returns live workers first, followed by recent ended sessions that can
// be resumed, deduplicated by name and capped to limit.
func (e *Engine) ListSelectableWorkers(ctx context.Context, limit int) []WorkerPickItem {
	if limit <= 0 {
		limit = 6
	}
	if e.store == nil {
		return nil
	}
	seen := map[string]bool{
		model.RouterName: true,
		model.DreamName:  true,
	}
	var liveItems []WorkerPickItem
	if workers, err := e.store.ListWorkers(ctx); err == nil {
		sort.Slice(workers, func(i, j int) bool { return workers[i].CreatedAt.After(workers[j].CreatedAt) })
		for _, w := range workers {
			if w.Ended || w.PaneID == "" || seen[w.Name] {
				continue
			}
			seen[w.Name] = true
			liveItems = append(liveItems, WorkerPickItem{
				Name:       w.Name,
				Machine:    e.MachineDisplayName(workerMachineID(w)),
				Kind:       w.Kind,
				Status:     w.Status,
				Session:    w.Session,
				WorkItemID: w.WorkItemID,
				Ended:      false,
			})
		}
	}
	out := append([]WorkerPickItem(nil), liveItems...)
	if len(out) >= limit {
		return out[:limit]
	}
	if ended, err := e.store.ListEndedSessions(ctx, 30); err == nil {
		for _, es := range ended {
			name := strings.TrimSpace(es.Name)
			if name == "" || es.Session == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, WorkerPickItem{
				Name:       name,
				Machine:    e.MachineDisplayName(es.Machine),
				Kind:       es.Kind,
				Status:     "ended",
				Session:    es.Session,
				WorkItemID: es.WorkItemID,
				Ended:      true,
			})
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

// RouterTranscriptInfo locates the Hub Router's live JSONL transcript path and adapter.
func (e *Engine) RouterTranscriptInfo(ctx context.Context) (string, agent.Adapter) {
	if e.agents == nil || e.memory == nil {
		return "", nil
	}
	kind := agent.NormalizeKind(e.Settings().RouterKind)
	sess := ""
	e.mu.Lock()
	launchAt := e.routerLaunchAt
	if e.routerLastKind != "" {
		kind = agent.NormalizeKind(e.routerLastKind)
	}
	e.mu.Unlock()

	if e.fleet != nil {
		if ags, err := e.fleet.Agents(ctx); err == nil {
			for _, a := range ags {
				if a.Name != nil && *a.Name == model.RouterName {
					if a.Agent != nil && *a.Agent != "" {
						kind = agent.NormalizeKind(*a.Agent)
					}
					if a.Session != nil {
						sess = extractSessionID(a.Session.Kind, a.Session.Value)
					}
					break
				}
			}
		}
	}
	if kind == "" {
		kind = "claude"
	}
	ad, err := e.agents.Get(kind)
	if err != nil {
		return "", nil
	}
	p := ad.LocateTranscript(e.home, e.memory.RouterDir(), sess, launchAt)
	return p, ad
}

// WorkerTranscriptSource returns either a local Hub transcript path + adapter or the remote
// machine ID + pane ID so channel watchers can consume live thinking/tool progress.
func (e *Engine) WorkerTranscriptSource(ctx context.Context, workerName string) (paneID, machine, localPath string, adapter agent.Adapter) {
	if e.store == nil {
		return "", "", "", nil
	}
	w, err := e.store.GetWorker(ctx, strings.TrimSpace(workerName))
	if err != nil || w == nil || w.Ended || w.PaneID == "" {
		return "", "", "", nil
	}
	machine = workerMachineID(w)
	paneID = w.PaneID
	if machine != "" {
		return paneID, machine, "", nil
	}
	if e.agents == nil {
		return paneID, "", "", nil
	}
	ad, err := e.agents.Get(w.Kind)
	if err != nil {
		return paneID, "", "", nil
	}
	sess := w.Session
	if sess == "" && e.fleet != nil {
		if ags, err := e.fleet.Agents(ctx); err == nil {
			for _, a := range ags {
				if a.PaneID == w.PaneID && a.Session != nil {
					sess = extractSessionID(a.Session.Kind, a.Session.Value)
					break
				}
			}
		}
	}
	p := ad.LocateTranscript(e.home, w.Cwd, sess, w.CreatedAt)
	return paneID, "", p, ad
}

// SendDirectWorkerMessage resolves or resumes the target Worker, queues or sends the prompt
// via SubmitPanePrompt, and registers an optional callback for delivering the Worker's direct reply.
func (e *Engine) SendDirectWorkerMessage(ctx context.Context, req DirectWorkerRequest) (*model.Worker, bool, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, false, fmt.Errorf("消息内容不能为空")
	}
	w, resumed, err := e.ResumeWorkerSession(ctx, ResumeWorkerRequest{
		Name:       req.Name,
		Machine:    req.Machine,
		Session:    req.Session,
		Kind:       req.Kind,
		Cwd:        req.Cwd,
		WorkItemID: req.WorkItemID,
	})
	if err != nil {
		return nil, false, err
	}

	var slot *pendingDirectTurn
	if req.OnDone != nil {
		slot = &pendingDirectTurn{
			prompt:   text,
			at:       time.Now(),
			callback: req.OnDone,
		}
		e.mu.Lock()
		if e.directTurns == nil {
			e.directTurns = map[string][]*pendingDirectTurn{}
		}
		e.directTurns[w.Name] = append(e.directTurns[w.Name], slot)
		e.mu.Unlock()
	}

	if _, _, err := e.SubmitPanePrompt(ctx, w.PaneID, text, "auto"); err != nil {
		if slot != nil {
			e.removeDirectTurn(w.Name, slot)
		}
		return w, resumed, err
	}
	return w, resumed, nil
}

func (e *Engine) hasDirectTurn(workerName string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.directTurns[workerName]) > 0
}

func (e *Engine) popDirectTurn(workerName string) *pendingDirectTurn {
	e.mu.Lock()
	defer e.mu.Unlock()
	q := e.directTurns[workerName]
	if len(q) == 0 {
		return nil
	}
	dt := q[0]
	if len(q) == 1 {
		delete(e.directTurns, workerName)
	} else {
		e.directTurns[workerName] = q[1:]
	}
	return dt
}

func (e *Engine) removeDirectTurn(workerName string, target *pendingDirectTurn) {
	e.mu.Lock()
	defer e.mu.Unlock()
	q := e.directTurns[workerName]
	for i, it := range q {
		if it == target {
			q = append(q[:i], q[i+1:]...)
			break
		}
	}
	if len(q) == 0 {
		delete(e.directTurns, workerName)
	} else {
		e.directTurns[workerName] = q
	}
}

func (e *Engine) appendDirectBgNote(note string) {
	note = strings.TrimSpace(note)
	if note == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.directBgNotes = append(e.directBgNotes, note)
	if len(e.directBgNotes) > 8 {
		e.directBgNotes = e.directBgNotes[len(e.directBgNotes)-8:]
	}
}

func (e *Engine) drainDirectBgNotes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.directBgNotes) == 0 {
		return nil
	}
	out := append([]string(nil), e.directBgNotes...)
	e.directBgNotes = nil
	return out
}

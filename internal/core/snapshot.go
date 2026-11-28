package core

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
)

// AddTimer creates and persists a new timer.
func (e *Engine) AddTimer(ctx context.Context, spec, text, thread, target, owner string) (*model.Timer, error) {
	next, err := ParseTimerSpec(spec, time.Now())
	if err != nil {
		return nil, err
	}
	id, _, err := e.store.NextID(ctx, "tm")
	if err != nil {
		return nil, err
	}
	tm := &model.Timer{
		ID:      id,
		Spec:    strings.TrimSpace(spec),
		Thread:  orStr(thread, model.MainThread),
		Target:  strings.TrimSpace(target),
		Text:    strings.TrimSpace(text),
		NextRun: next,
		Enabled: true,
		Owner:   owner,
	}
	if err := e.store.UpsertTimer(ctx, tm); err != nil {
		return nil, err
	}
	e.notifyChanged()
	return tm, nil
}

// Show expands any ID (trigger, entry, action, thread, or worker) for `corgi show` (Chain of Recall).
func (e *Engine) Show(ctx context.Context, id string) (any, error) {
	if t, err := e.store.GetTrigger(ctx, id); err == nil {
		return t, nil
	}
	if en, err := e.store.GetEntry(ctx, id); err == nil {
		return en, nil
	}
	if act, err := e.store.GetAction(ctx, id); err == nil {
		return act, nil
	}
	if wi, err := e.store.GetWorkItem(ctx, id); err == nil {
		return wi, nil
	}
	if w, err := e.store.GetWorker(ctx, id); err == nil {
		trigs, _ := e.store.ListWorkerTriggers(ctx, w.Name, 15)
		out := map[string]any{"worker": w, "triggers": trigs}
		if !w.Ended && w.PaneID != "" {
			out["last_message"] = e.lastWorkerSummary(ctx, w)
		}
		return out, nil
	}
	if th, err := e.store.GetThread(ctx, id); err == nil {
		trigs, _ := e.store.ListThreadTriggers(ctx, id, 30)
		return map[string]any{"thread": th, "triggers": trigs}, nil
	}
	return nil, store.ErrNotFound
}

// MemorySnapshot returns the memory files, dream journals, budget metrics, and dream schedule.
func (e *Engine) MemorySnapshot(ctx context.Context, displayDir string) MemorySnapshot {
	snap := MemorySnapshot{
		Dir:         displayDir,
		Files:       e.memory.ListFiles(),
		Dreams:      e.memory.ListDreams(),
		UpFrontSize: e.memory.UpFrontSize(),
		Budget:      UpFrontBudgetBytes,
		Dreaming:    e.Dreaming(),
	}
	if last := e.LastDream(ctx); !last.IsZero() {
		snap.LastDream = &last
	}
	if next := e.NextDream(ctx, time.Now()); !next.IsZero() {
		snap.NextDream = &next
	}
	return snap
}

// ResolvedActionsForEntries loads non-pending Actions referenced by the given chat entries
// so the Web UI can render quiet decision receipt cards with original question context.
func (e *Engine) ResolvedActionsForEntries(ctx context.Context, entries []*model.Entry) map[string]*model.Action {
	ids := make([]string, 0, len(entries))
	for _, en := range entries {
		if en != nil && en.ActionID != "" {
			ids = append(ids, en.ActionID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	loaded, err := e.store.ListActionsByIDs(ctx, ids)
	if err != nil || len(loaded) == 0 {
		return nil
	}
	out := make(map[string]*model.Action, len(loaded))
	for id, act := range loaded {
		if act != nil && act.Status != model.ActionPending {
			out[id] = act
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Snapshot builds the SuperView pushed to Web/PWA clients.
func (e *Engine) Snapshot(ctx context.Context, thread string) model.SuperView {
	_, _ = e.store.ExpireStalePendingActions(ctx, model.PendingActionMaxAge)
	_ = e.expireStaleConfigConfirms(ctx)
	entries, earlier, _ := e.store.ListEntries(ctx, thread, 200)
	e.HydrateEntriesArtifacts(ctx, entries)
	actions, _ := e.store.ListPendingActions(ctx)
	activityNotices, _ := e.store.ListRecentFYIActions(ctx, thread, time.Now().Add(-fyiActivityWindow), fyiActivityLimit)
	resolvedActions := e.ResolvedActionsForEntries(ctx, entries)
	snoozed, _ := e.store.ListSnoozedActions(ctx)
	pinned, _ := e.store.ListPinnedEntries(ctx, 50)
	e.HydrateEntriesArtifacts(ctx, pinned)
	workers, _ := e.store.ListWorkers(ctx)
	workItems, _ := e.store.ListWorkItems(ctx, false)
	if workItems == nil {
		workItems = []*model.WorkItem{}
	}
	e.HydrateWorkItemsSnapshots(ctx, workItems)
	closedWorkCount, _ := e.store.CountClosedWorkItems(ctx, thread)
	threads, _ := e.store.ListThreads(ctx, 100)
	queued, _ := e.store.ListQueuedTriggers(ctx)

	var activeWorkers []*model.Worker
	for _, w := range workers {
		if !w.Ended {
			activeWorkers = append(activeWorkers, w)
			continue
		}
		if time.Since(w.CreatedAt) < 12*time.Hour {
			// Slim ended workers in Snapshot/SSE while preserving lossless store records & show/{id}.
			slim := *w
			slim.Task = clipMultiline(slim.Task, 400)
			slim.LastReport = clipMultiline(slim.LastReport, 240)
			activeWorkers = append(activeWorkers, &slim)
		}
	}
	sort.Slice(activeWorkers, func(i, j int) bool { return activeWorkers[i].CreatedAt.After(activeWorkers[j].CreatedAt) })

	qRouter := 0
	for _, t := range queued {
		if len(t.Targets) == 0 || slices.Contains(t.Targets, model.RouterName) {
			qRouter++
		}
	}

	e.mu.Lock()
	st := e.settings
	apiTurn := e.router.turn
	apiAuthPaused := e.router.authPaused
	rv := model.RouterView{
		Kind:     "api",
		Model:    orStr(st.RouterAPIModel, TokenHubDefaultModel),
		Effort:   st.RouterAPIEffort,
		Status:   "asleep",
		Dreaming: e.dream != nil,
	}
	if apiTurn != nil {
		rv.Alive = true
		rv.Status = "working"
		rv.Replied = apiTurn.hasFlag(turnFlagReplied)
	} else if e.dream != nil {
		rv.Alive = true
		rv.Status = "dreaming"
	}
	sink := e.sink
	e.mu.Unlock()

	if rv.Status != "working" && rv.Status != "dreaming" {
		if e.hasConfiguredRouterAPIKey(ctx, st) && !apiAuthPaused {
			rv.Alive = true
			rv.Status = "idle"
		} else {
			rv.Alive = false
			rv.Status = "asleep"
		}
	}

	var follow *model.FollowView
	if timers, err := e.store.ListTimers(ctx); err == nil {
		for _, tm := range timers {
			if tm.Enabled && !tm.NextRun.IsZero() && (strings.HasPrefix(tm.Spec, "in ") || strings.HasPrefix(tm.Spec, "at ")) && tm.Owner != "you" {
				follow = &model.FollowView{Next: tm.NextRun, NextText: oneLineClip(tm.Text, 80)}
				break
			}
		}
	}

	queuedChat, chatQueuePaused := e.ChatQueueSnapshot(ctx, thread)

	return model.SuperView{
		Router:           rv,
		DispatchDefaults: buildDispatchDefaultsView(st),
		Entries:          entries,
		Earlier:          earlier,
		Actions:          actions,
		ActivityNotices:  activityNotices,
		ResolvedActions:  resolvedActions,
		SnoozedActions:   len(snoozed),
		Pinned:           pinned,
		Workers:          activeWorkers,
		WorkItems:        workItems,
		ClosedWorkCount:  closedWorkCount,
		Threads:          threads,
		Queued:           len(queued),
		QueuedRouter:     qRouter,
		QueuedChat:       queuedChat,
		ChatQueuePaused:  chatQueuePaused,
		Paused:           st.Paused,
		WeComOnline:      sink.WeComOnline(),
		WeComFocus:       e.ActiveWeComFocus(ctx, thread),
		Follow:           follow,
	}
}

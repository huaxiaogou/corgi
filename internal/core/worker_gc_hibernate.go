package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
)

func (e *Engine) collectWorkerSubscribers(ctx context.Context, workerName string) []string {
	if workerName == "" || e.store == nil {
		return nil
	}
	threads, err := e.store.ListThreads(ctx, 200)
	if err != nil {
		return nil
	}
	var subs []string
	for _, th := range threads {
		if th != nil && th.ID != "" && th.ID != model.MainThread && slices.Contains(th.Subscribers, workerName) {
			subs = append(subs, th.ID)
		}
	}
	return subs
}

func (e *Engine) buildWorkerEndedSession(ctx context.Context, wk *model.Worker, histCwd string, subs []string, now time.Time) *model.EndedSession {
	if wk == nil || wk.Session == "" || e.store == nil {
		return nil
	}
	eid, _, err := e.store.NextID(ctx, "h")
	if err != nil {
		return nil
	}
	return &model.EndedSession{
		ID:                  eid,
		Kind:                wk.Kind,
		Machine:             wk.Machine,
		Cwd:                 orStr(histCwd, wk.Cwd),
		Name:                wk.Name,
		Title:               orStr(wk.Title, wk.Name),
		Session:             wk.Session,
		WorkItemID:          wk.WorkItemID,
		Model:               wk.Model,
		Effort:              wk.Effort,
		Constraint:          wk.Constraint,
		Mode:                wk.Mode,
		ReadonlyEnforcement: wk.ReadonlyEnforcement,
		CreatedBy:           wk.CreatedBy,
		Thread:              wk.Thread,
		Keep:                wk.Keep,
		RepoRoot:            wk.RepoRoot,
		RepoCommonDir:       wk.RepoCommonDir,
		WorktreePath:        wk.WorktreePath,
		WorktreeBranch:      wk.WorktreeBranch,
		WorktreeBase:        wk.WorktreeBase,
		Subscribers:         append([]string(nil), subs...),
		EndedAt:             now,
	}
}

func (e *Engine) hibernateWorker(ctx context.Context, cand workerGCCandidate) {
	if e.fleet == nil || e.store == nil || cand.name == "" {
		return
	}

	// Atomically claim exclusive GC closing slot when zero active workerOps exist.
	e.gc.mu.Lock()
	if e.gc.closing[cand.name] || e.gc.workerOps[cand.name] > 0 {
		e.gc.mu.Unlock()
		return
	}
	if e.gc.closing == nil {
		e.gc.closing = map[string]bool{}
	}
	if e.gc.closingPane == nil {
		e.gc.closingPane = map[string]string{}
	}
	e.gc.closing[cand.name] = true
	if cand.paneID != "" {
		e.gc.closingPane[cand.paneID] = cand.name
	}
	e.gc.mu.Unlock()

	defer func() {
		e.gc.mu.Lock()
		delete(e.gc.closing, cand.name)
		if cand.paneID != "" && e.gc.closingPane[cand.paneID] == cand.name {
			delete(e.gc.closingPane, cand.paneID)
		}
		e.gc.mu.Unlock()
	}()

	now := time.Now()
	st := e.Settings()
	if st.EffectiveWorkerIdleGCMinutes() <= 0 || st.Paused || e.isHubUpdating() {
		return
	}

	// Re-read fresh worker state and re-verify all safety vetoes before touching herdr.
	wk, err := e.store.GetWorker(ctx, cand.name)
	if err != nil || wk == nil || wk.Ended || wk.Hibernating || wk.Keep || !isCorgiOwnedWorker(wk) {
		return
	}
	if wk.PaneID == "" || wk.PaneID != cand.paneID || wk.WorkspaceID == "" || wk.WorkspaceID != cand.workspaceID {
		return
	}
	if e.isHealingBypassWorker(wk.Name, wk.PaneID) ||
		e.hasDirectTurn(wk.Name) || e.hasWorkerPendingCheck(wk.Name, now) || e.hasWorkerStartupCheck(wk.Name) {
		return
	}
	if actCooldown := e.getWorkerGCActivityCooldown(); actCooldown > 0 {
		if lastAct := e.lastWorkerActivity(wk.Name); !lastAct.IsZero() && now.Sub(lastAct) < actCooldown {
			return
		}
	}
	if wk.SnoozeUntil != nil && wk.SnoozeUntil.After(now) {
		return
	}
	if qTrigs, qErr := e.store.ListQueuedTriggers(ctx); qErr == nil {
		for _, t := range qTrigs {
			if slices.Contains(t.Targets, wk.Name) {
				return
			}
		}
	}
	if qPrompts, qErr := e.store.ListQueuedPrompts(ctx, wk.PaneID); qErr == nil && len(qPrompts) > 0 {
		return
	}
	if pending, pErr := e.store.ListPendingActions(ctx); pErr == nil {
		for _, a := range pending {
			if a.TargetWorker == wk.Name || (wk.WorkItemID != "" && a.WorkItemID == wk.WorkItemID) {
				return
			}
		}
	}
	if snoozed, sErr := e.store.ListSnoozedActions(ctx); sErr == nil {
		for _, a := range snoozed {
			if (a.SnoozeUntil == nil || a.SnoozeUntil.After(now)) &&
				(a.TargetWorker == wk.Name || (wk.WorkItemID != "" && a.WorkItemID == wk.WorkItemID)) {
				return
			}
		}
	}
	for _, fv := range e.ListActiveWeComFocuses(ctx) {
		if fv != nil && fv.Worker == wk.Name {
			return
		}
	}

	// Re-verify rule preconditions on fresh worker state.
	if cand.rule == "P2" {
		if !isWorkerFaulted(wk) || !wk.StartupFailed {
			return
		}
	} else {
		if isWorkerFaulted(wk) {
			return
		}
		effIdle, ok := e.effectiveWorkerIdle(wk, now)
		if !ok {
			return
		}
		switch cand.rule {
		case "P1":
			if wk.WorkItemID == "" {
				return
			}
			wi, wiErr := e.store.GetWorkItem(ctx, wk.WorkItemID)
			if wiErr != nil || wi == nil || wi.ClosedAt == nil || effIdle < defaultWorkerGCClosedIdle {
				return
			}
		case "P3":
			if effIdle < time.Duration(st.EffectiveWorkerIdleGCMinutes())*time.Minute {
				return
			}
		case "P4":
			if effIdle < defaultWorkerGCCapIdleCooldown {
				return
			}
		}
	}

	// Screen check BEFORE final pane check: if screen read fails or background task is active, abort conservatively.
	rr, rErr := e.fleet.Read(ctx, wk.PaneID, "recent", "text", 20)
	if rErr != nil {
		e.logger.Warn("worker gc: screen read failed, skipping hibernation", "worker", wk.Name, "pane", wk.PaneID, "err", rErr)
		return
	}
	if agent.HasBackgroundTask(rr.Text) {
		return
	}

	// Strictly use pane-bound session ID (never cwd-guessed lookupRemoteSession).
	if cand.session != "" && wk.Session == "" {
		wk.Session = cand.session
	}
	if (cand.rule == "P3" || cand.rule == "P4") && wk.Session == "" {
		return
	}

	// Query ONLY target machine's pane.list immediately before closing to verify:
	// (a) wk.WorkspaceID contains strictly 1 pane matching wk.PaneID (never kill sibling user panes),
	// (b) for P1/P3/P4 the pane is still idle/done (never interrupt a newly started foreground turn).
	machine := workerMachineID(wk)
	panes, pErr := e.fleet.PanesOnMachine(ctx, machine)
	if pErr != nil {
		e.logger.Warn("worker gc: pane.list failed, aborting hibernation", "worker", wk.Name, "machine", machine, "err", pErr)
		e.recordWorkerGCFailBackoff(wk.Name, now)
		return
	}
	wsPaneCount := 0
	var targetPane *model.Pane
	for i := range panes {
		if panes[i].WorkspaceID == wk.WorkspaceID {
			wsPaneCount++
			if panes[i].ID == wk.PaneID {
				targetPane = &panes[i]
			}
		}
	}
	if wsPaneCount != 1 || targetPane == nil {
		e.logger.Info("worker gc: skipped non-exclusive workspace", "worker", wk.Name, "workspace", wk.WorkspaceID, "pane_count", wsPaneCount)
		return
	}
	if cand.rule == "P2" {
		if targetPane.AgentStatus == "working" {
			return
		}
	} else {
		if targetPane.AgentStatus != "idle" && targetPane.AgentStatus != "done" {
			return
		}
	}

	// Re-read fresh worker right before persisting Hibernating=true so a concurrent Keep=true
	// toggle during the screen/pane RPC is never clobbered.
	freshWk, fErr := e.store.GetWorker(ctx, cand.name)
	if fErr != nil || freshWk == nil || freshWk.Ended || freshWk.Hibernating || freshWk.Keep ||
		!isCorgiOwnedWorker(freshWk) || freshWk.PaneID != cand.paneID || freshWk.WorkspaceID != cand.workspaceID {
		return
	}
	if cand.session != "" && freshWk.Session == "" {
		freshWk.Session = cand.session
	}
	if (cand.rule == "P3" || cand.rule == "P4") && freshWk.Session == "" {
		return
	}
	wk = freshWk

	// Persist Hibernating=true BEFORE issuing CloseWorkspace so any crash or lost RPC reply
	// reconciles quietly in syncWorkers instead of marking the worker "gone" and alerting Router.
	wk.Hibernating = true
	if err := e.store.UpsertWorker(ctx, wk); err != nil {
		return
	}

	// Close workspace immediately (no stopCodexTurn since GC only closes already-idle/faulted panes).
	closeErr := e.fleet.CloseWorkspace(ctx, wk.WorkspaceID)
	if closeErr != nil && !isHerdrNotFoundErr(closeErr) {
		var he *herdr.Error
		if errors.As(closeErr, &he) {
			// Structured herdr rejection: workspace was NOT closed; revert Hibernating cleanly.
			e.logger.Warn("worker gc: close workspace rejected, keeping worker live", "worker", wk.Name, "workspace", wk.WorkspaceID, "err", closeErr)
			wk.Hibernating = false
			_ = e.store.UpsertWorker(ctx, wk)
			e.recordWorkerGCFailBackoff(wk.Name, time.Now())
			return
		}
		// Ambiguous transport/timeout error (workspace may already have closed on the Node):
		// leave Hibernating=true so syncWorkers reconciles cleanly on the next confirmed machine scan.
		e.logger.Warn("worker gc: close workspace transport error, deferring to sync reconciliation", "worker", wk.Name, "workspace", wk.WorkspaceID, "err", closeErr)
		e.recordWorkerGCFailBackoff(wk.Name, time.Now())
		return
	}

	if wk.PaneID != "" && e.agents != nil {
		if lp, m := fleet.SplitID(wk.PaneID); m == "" && lp != "" {
			e.agents.ReleasePane(lp)
		}
	}
	e.finalizeHibernatedWorker(ctx, wk, cand.reason, time.Now())
}

func isHerdrNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	var he *herdr.Error
	if errors.As(err, &he) {
		return strings.Contains(strings.ToLower(he.Code), "not_found")
	}
	return false
}

// finalizeHibernatedWorker archives the worker's session (preserving worktree path and metadata),
// marks the worker Ended=true, cleans up live subscriptions/dialogs/turn watermarks, and emits
// one quiet info event. Never deletes worktree directories.
func (e *Engine) finalizeHibernatedWorker(ctx context.Context, wk *model.Worker, reason string, now time.Time) {
	if wk == nil || e.store == nil {
		return
	}
	if wk.PaneID != "" {
		e.recordRecentlyClosedPane(wk.PaneID, now)
	}
	subs := e.collectWorkerSubscribers(ctx, wk.Name)
	if wk.WorkItemID != "" && wk.WorktreeBranch != "" {
		_ = e.store.RecordWorkItemBranch(ctx, wk.WorkItemID, wk.WorktreeBranch)
	}
	// Save rich EndedSession BEFORE clearing Hibernating/marking Ended so concurrent WatchEndings
	// sees the completed archive atomically.
	if es := e.buildWorkerEndedSession(ctx, wk, wk.Cwd, subs, now); es != nil {
		_ = e.store.SaveEndedSession(ctx, es, store.EndedSessionRetentionLimit)
	}

	wk.Ended = true
	wk.Hibernating = false
	wk.Status = "done"
	wk.FaultError = ""
	wk.FaultPaneID = ""
	wk.FaultAt = nil
	wk.StartupFailed = false
	_ = e.store.UpsertWorker(ctx, wk)

	if len(subs) > 0 {
		if threads, tErr := e.store.ListThreads(ctx, 200); tErr == nil {
			for _, th := range threads {
				if slices.Contains(th.Subscribers, wk.Name) {
					th.Subscribers = removeStr(th.Subscribers, wk.Name)
					_ = e.store.UpsertThread(ctx, th)
				}
			}
		}
	}
	_ = e.store.ExpireWorkerActions(ctx, wk.Name)
	e.ClearWeComFocusForWorker(ctx, wk.Name)
	if wk.PaneID != "" {
		_ = e.store.DeleteQueuedPromptsByScope(ctx, wk.PaneID)
		e.setQueuePaused(wk.PaneID, false)
		e.clearConstraintDelivered(wk.PaneID)
	}
	e.clearWorkerTurnState(wk.Name)

	note := "上下文与目录已保留，可随时续接"
	if wk.Session == "" {
		note = "进程已回收"
	}
	if reason != "" {
		e.addEvent(ctx, wk.Thread, fmt.Sprintf("已自动休眠 %s（%s，%s）", wk.Name, reason, note), wk.Name, "info")
	} else {
		e.addEvent(ctx, wk.Thread, fmt.Sprintf("已自动休眠 %s（%s）", wk.Name, note), wk.Name, "info")
	}
	e.notifyChanged()
}

// reconcileHibernatingWorker resolves a worker left in Hibernating=true after a Hub crash
// or lost CloseWorkspace reply once a fresh scan of its host machine completes.
func (e *Engine) reconcileHibernatingWorker(ctx context.Context, w *model.Worker, alive bool, scanned map[string]bool, now time.Time) {
	if w == nil || !w.Hibernating || e.isWorkerGCClosing(w.Name) {
		return
	}
	wkMachine := workerMachineID(w)
	if scanned != nil && !scanned[wkMachine] {
		return
	}
	if !alive {
		e.finalizeHibernatedWorker(ctx, w, "休眠对账完成", now)
		return
	}
	// Pane is still alive: clear Hibernating flag and back off so we don't thrash.
	w.Hibernating = false
	_ = e.store.UpsertWorker(ctx, w)
	e.recordWorkerGCFailBackoff(w.Name, now)
	e.notifyChanged()
}

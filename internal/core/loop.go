package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

// Run drives the periodic Super-Agent scheduling loop until ctx is canceled.
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()
	e.runCtx = ctx
	e.mu.Unlock()
	e.reconcileAPIRouterInflight(ctx)
	e.EmitPendingAutoUpdateNotice(ctx)
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	defer e.llm.CloseIdleConnections()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.wakeCh:
			e.tick(ctx)
		case <-tick.C:
			e.tick(ctx)
		}
	}
}

type liveInfo struct {
	pane    string
	status  string
	ready   bool
	kind    string
	session string
}

func isHubSystemName(name string) bool {
	return name == model.RouterName || name == model.DreamName
}

func isHubLocalPane(paneID string) bool {
	return paneID != "" && !strings.Contains(paneID, "@")
}

func (e *Engine) tick(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()

	// 1. Fire due timers and periodically expire >7d stale pending actions.
	now := time.Now()
	e.mu.Lock()
	needExpire := now.Sub(e.lastPendingExpire) >= 10*time.Minute
	if needExpire {
		e.lastPendingExpire = now
	}
	e.mu.Unlock()
	if needExpire {
		if n, err := e.store.ExpireStalePendingActions(ctx, model.PendingActionMaxAge); err == nil && n > 0 {
			e.notifyChanged()
		}
	}
	if e.expireStaleConfigConfirms(ctx) > 0 {
		e.notifyChanged()
	}
	if timers, err := e.store.ListTimers(ctx); err == nil {
		for _, tm := range timers {
			if tm.Enabled && !tm.NextRun.After(now) {
				fired := now
				tm.LastRun = &fired
				AdvanceTimer(tm, now)
				_ = e.store.UpsertTimer(ctx, tm)
				_, _ = e.Ingest(ctx, "timer", tm.Thread, tm.ID, tm.Text, "", tm.Target, tm.ID)
			}
		}
	}

	var (
		rawAgents []model.Agent
		scanned   map[string]bool
		fleetOK   bool
	)
	if e.fleet != nil {
		// Cap fleet scan so a stalled local herdr never exhausts the tick deadline
		// needed by API Dream finalization and API Router SQLite delivery.
		scanCtx, scanCancel := context.WithTimeout(ctx, 8*time.Second)
		var fErr error
		rawAgents, scanned, fErr = e.fleet.AgentsWithScanned(scanCtx)
		scanCancel()
		if fErr == nil || len(scanned) > 0 {
			fleetOK = true
		}
	}
	live := map[string]liveInfo{}
	if fleetOK {
		for i := range rawAgents {
			a := &rawAgents[i]
			if (a.Name == nil || *a.Name == "") && a.PaneID != "" && a.Agent != nil && *a.Agent != "" {
				fallback := deriveFallbackWorkerName(*a.Agent, a.PaneID)
				a.Name = &fallback
				e.fleet.RememberName(a.PaneID, fallback)
			}
			if a.Name == nil || *a.Name == "" {
				continue
			}
			name := *a.Name
			if isHubSystemName(name) && !isHubLocalPane(a.PaneID) {
				continue
			}
			li := liveInfo{pane: a.PaneID, status: a.AgentStatus, ready: a.Ready}
			if a.Agent != nil {
				li.kind = *a.Agent
			}
			if a.Session != nil {
				li.session = extractSessionID(a.Session.Kind, a.Session.Value)
			}
			live[name] = li
		}

		// Auto-adopt any live non-system agent session into Corgi's managed workers table
		// so no orphan unmanaged sessions ever exist.
		e.adoptUnmanagedAgents(ctx, rawAgents)

		// 2. Sync worker states, detect bare-shell failures, create Actions on blocked dialogs or quota errors.
		e.syncWorkers(ctx, live, scanned, now)
	}

	queued, _ := e.store.ListQueuedTriggers(ctx)
	hasRouterQueued := false
	for _, t := range queued {
		if len(t.Targets) == 0 || slices.Contains(t.Targets, model.RouterName) {
			hasRouterQueued = true
			break
		}
	}

	// 3. Drive or trigger nightly memory consolidation (Dream).
	e.checkDream(ctx, hasRouterQueued)

	if e.Settings().Paused || e.isHubUpdating() {
		return
	}

	// 4. Drain idle main-chat queue, route/deliver triggers, and drain idle single-agent queues.
	e.drainRouterPrompts(ctx)
	e.deliverQueuedWithFleet(ctx, live, scanned, fleetOK)
	if fleetOK {
		e.drainPanePrompts(ctx, rawAgents)
	}

	// 5. Watchdog for stalled/thrashing workers, zero-token gated idle patrol / morning digest,
	//    and lossless background hibernation GC for idle Corgi-dispatched workers.
	if fleetOK {
		e.checkStalls(ctx, live, now)
		e.checkWorkerThrash(ctx, live, now)
		e.checkIdlePatrol(ctx, live, now)
		e.checkWorkerGC(ctx, rawAgents, live, scanned, now)
	}

	// 6. Optional overnight idle auto-update after tonight's scheduled Dream succeeds/skips.
	e.checkAutoUpdate(ctx, hasRouterQueued, now)
}

func deriveFallbackWorkerName(rawKind, paneID string) string {
	k := agent.NormalizeKind(rawKind)
	if k == "" {
		k = "agent"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(paneID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if r == '@' || r == '-' || r == '_' {
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 16 {
		slug = slug[:16]
	}
	if slug == "" {
		return k + "-live"
	}
	return k + "-" + slug
}

// adoptUnmanagedAgents ensures every live non-system coding agent across all connected machines
// is tracked in Corgi's managed workers table so it participates in status counts, dialog cards,
// and end-of-turn reporting.
func (e *Engine) adoptUnmanagedAgents(ctx context.Context, rawAgents []model.Agent) {
	workers, err := e.store.ListWorkers(ctx)
	if err != nil {
		return
	}
	byName := map[string]*model.Worker{}
	byPane := map[string]*model.Worker{}
	for _, w := range workers {
		byName[w.Name] = w
		if !w.Ended && w.PaneID != "" {
			byPane[w.PaneID] = w
		}
	}

	now := time.Now()
	changed := false
	for _, a := range rawAgents {
		if a.Name == nil || *a.Name == "" || a.PaneID == "" || a.Agent == nil {
			continue
		}
		name := *a.Name
		if name == model.RouterName || name == model.DreamName {
			continue
		}
		if e.isWorkerGCClosing(name) || e.isPaneGCClosing(a.PaneID) || e.isRecentlyClosedPane(a.PaneID, now) {
			continue
		}
		if byPane[a.PaneID] != nil {
			continue
		}
		if prev := byName[name]; prev != nil {
			if !prev.Ended || prev.Hibernating {
				continue
			}
			// Leave "gone" workers with matching PaneID to syncWorkers' own restoration branch.
			if prev.Status == "gone" && prev.PaneID == a.PaneID {
				continue
			}
		}

		kind := agent.NormalizeKind(*a.Agent)
		if kind == "" {
			continue
		}
		_, machine := fleet.SplitID(a.PaneID)
		cwd := ""
		if a.Cwd != nil {
			cwd = *a.Cwd
		}
		sess := ""
		if a.Session != nil {
			sess = extractSessionID(a.Session.Kind, a.Session.Value)
		}
		taskDesc := strings.TrimSpace(a.TerminalTitle)
		if taskDesc == "" {
			taskDesc = "项目会话 · " + orStr(cwd, name)
		}
		st := a.AgentStatus
		if st == "" {
			st = "idle"
		}
		wk := &model.Worker{
			Name:        name,
			Kind:        kind,
			Machine:     machine,
			Cwd:         cwd,
			Task:        oneLineClip(taskDesc, 200),
			Thread:      model.MainThread,
			PaneID:      a.PaneID,
			WorkspaceID: a.WorkspaceID,
			CreatedBy:   "you",
			CreatedAt:   time.Now(),
			Status:      st,
			Session:     sess,
		}
		if e.fleet != nil {
			if run, ok := e.fleet.Run(a.PaneID); ok {
				wk.Run = run
				wk.Model, wk.Effort = run[0], run[1]
			}
		}
		if uErr := e.store.UpsertWorker(ctx, wk); uErr == nil {
			byName[name] = wk
			byPane[a.PaneID] = wk
			changed = true
		}
	}
	if changed {
		e.notifyChanged()
	}
}

func (e *Engine) syncWorkers(ctx context.Context, live map[string]liveInfo, scanned map[string]bool, now time.Time) {
	workers, err := e.store.ListWorkers(ctx)
	if err != nil {
		return
	}
	for _, w := range workers {
		e.syncOneWorker(ctx, w, live, scanned, now)
	}
}

func (e *Engine) syncOneWorker(ctx context.Context, w *model.Worker, live map[string]liveInfo, scanned map[string]bool, now time.Time) {
	if w == nil {
		return
	}
	// Acquire non-activity worker op reservation so GC hibernation cannot claim and close w
	// mid-iteration while syncOneWorker holds a stale snapshot of w.
	if !e.beginWorkerOpWithoutActivity(w.Name) {
		return
	}
	defer e.endWorkerOpWithoutActivity(w.Name)

	if w.Ended {
		// Restore worker if herdr brought its exact pane back after a daemon restart.
		if li, ok := live[w.Name]; ok && w.Status == "gone" && w.PaneID != "" && li.pane == w.PaneID {
			w.Ended = false
			w.Status = li.status
			_ = e.store.UpsertWorker(ctx, w)
			e.invalidateBypassCheck(w.PaneID)
			_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
				fmt.Sprintf("worker %s is back (herdr restored its pane; status %s).", w.Name, li.status), "", model.RouterName, w.Name)
			e.addEvent(ctx, w.Thread, w.Name+" 回来了（herdr 恢复了其窗格）", w.Name, "info")
			e.notifyChanged()
		}
		return
	}
	li, alive := live[w.Name]
	if w.Hibernating {
		e.reconcileHibernatingWorker(ctx, w, alive, scanned, now)
		return
	}
	if !alive {
		if e.isHealingBypassWorker(w.Name, w.PaneID) {
			return
		}
		wkMachine := workerMachineID(w)
		if scanned != nil && !scanned[wkMachine] {
			// Machine did not complete a scan on this tick (e.g. Node restarting/reconnecting);
			// preserve worker state and pending approvals until its machine is observed again.
			return
		}
		if w.Status != "starting" && time.Since(w.CreatedAt) > 20*time.Second {
			w.Ended = true
			w.Status = "gone"
			w.FaultError = ""
			w.FaultPaneID = ""
			w.FaultAt = nil
			w.StartupFailed = false
			_ = e.store.UpsertWorker(ctx, w)
			_ = e.store.ExpireWorkerActions(ctx, w.Name)
			e.ClearWeComFocusForWorker(ctx, w.Name)
			_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
				fmt.Sprintf("worker %s exited (pane gone).", w.Name), "", model.RouterName, w.Name)
			e.addEvent(ctx, w.Thread, w.Name+" 已退出", w.Name, "info")
			e.notifyChanged()
		}
		return
	}
	if e.isHealingBypassWorker(w.Name, li.pane) {
		return
	}
	if e.checkWorkerBareShell(ctx, w, li, now) {
		return
	}
	sessionChanged := false
	if li.session != "" && li.session != w.Session {
		w.Session = li.session
		if w.StartupFailed {
			w.StartupFailed = false
		}
		sessionChanged = true
	}
	// herdr reports "idle" while an agent sits on a first-run wizard or folder trust prompt.
	if li.status == "idle" && e.shouldCheckIdleDialog(li.pane, now) {
		if rr, rErr := e.fleet.Read(ctx, li.pane, "detection", "text", 0); rErr == nil && agent.ScreenAsks(rr.Text) {
			li.status = "blocked"
			live[w.Name] = li
		}
	}
	prev := w.Status
	if prev == "working" && (li.status == "done" || li.status == "idle") {
		e.invalidateBypassCheck(li.pane)
	}
	if li.status == "idle" || li.status == "done" {
		e.maybeHealWorkerBypass(ctx, w, li, now)
	}
	idleChanged := false
	if li.status == "idle" || li.status == "done" {
		if w.IdleSince == nil {
			t := now
			if (prev == "idle" || prev == "done") && !w.ReportAt.IsZero() && w.ReportAt.Before(now) {
				t = w.ReportAt
			}
			if lastAct := e.lastWorkerActivity(w.Name); !lastAct.IsZero() && lastAct.After(t) {
				t = lastAct
			}
			if t.After(now) {
				t = now
			}
			w.IdleSince = &t
			idleChanged = true
		}
	} else if w.IdleSince != nil || w.IdlePatrolCount > 0 || w.SnoozeUntil != nil {
		resetWorkerIdle(w)
		idleChanged = true
	}
	if li.status == "working" {
		if !w.HadTurn {
			w.HadTurn = true
			sessionChanged = true
		}
	}
	if li.status == "working" || li.status == "blocked" || (w.FaultPaneID != "" && li.pane != "" && li.pane != w.FaultPaneID) {
		e.disarmWorkerStartupCheck(w.Name)
		if w.FaultError != "" || w.FaultPaneID != "" || w.FaultAt != nil || w.StartupFailed {
			w.FaultError = ""
			w.FaultPaneID = ""
			w.FaultAt = nil
			w.StartupFailed = false
			sessionChanged = true
		}
	}
	effectiveStatus := li.status
	if isWorkerFaulted(w) && (li.status == "idle" || li.status == "done") {
		effectiveStatus = "failed"
	}
	if effectiveStatus == prev && li.pane == w.PaneID {
		if idleChanged || sessionChanged {
			_ = e.store.UpsertWorker(ctx, w)
		}
		// Inspect freshly-dispatched idle/done workers for immediate fatal executor errors
		// or silent non-starts even when herdr never observed a working transition.
		if (li.status == "done" || li.status == "idle") && e.hasWorkerStartupCheck(w.Name) {
			e.checkWorkerStartupHealth(ctx, w, li, now)
		}
		// Re-check steady idle/done workers when waiting for a delayed transcript flush
		// (direct WeCom turns OR Router-managed turns that went idle with empty interim text).
		if (li.status == "done" || li.status == "idle") && (e.hasDirectTurn(w.Name) || e.hasWorkerPendingCheck(w.Name, now)) {
			e.handleWorkerTurnDone(ctx, w)
		}
		return
	}
	if li.status == "working" {
		e.clearWorkerPendingCheck(w.Name)
	}
	w.PaneID = li.pane
	w.Status = effectiveStatus
	_ = e.store.UpsertWorker(ctx, w)
	e.notifyChanged()

	switch {
	case li.status == "blocked":
		e.mu.Lock()
		recentAns := now.Sub(e.lastAnswer[li.pane]) < 2*time.Second
		e.mu.Unlock()
		if !recentAns {
			e.handleWorkerBlocked(ctx, w)
		}
	case prev == "working" && (li.status == "done" || li.status == "idle"):
		_ = e.store.ExpireWorkerActions(ctx, w.Name)
		e.handleWorkerTurnDone(ctx, w)
	case (li.status == "done" || li.status == "idle") && e.hasWorkerStartupCheck(w.Name):
		e.checkWorkerStartupHealth(ctx, w, li, now)
	}
}

func (e *Engine) shouldCheckIdleDialog(paneID string, now time.Time) bool {
	if paneID == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if now.Sub(e.idleCheckAt[paneID]) < 4500*time.Millisecond {
		return false
	}
	e.idleCheckAt[paneID] = now
	return true
}

func (e *Engine) handleWorkerBlocked(ctx context.Context, w *model.Worker) {
	rr, err := e.fleet.Read(ctx, w.PaneID, "detection", "text", 0)
	if err != nil {
		return
	}
	ap := agent.ParseApproval(rr.Text)
	if len(ap.Choices) == 0 {
		return
	}
	if e.answerWorkerTrust(ctx, w, ap) {
		return
	}
	if e.answerRestoredAgyEditDialog(ctx, w, ap) {
		return
	}
	if pending, err := e.store.ListPendingActions(ctx); err == nil {
		for _, p := range pending {
			if p.Kind == model.ActionKindWorkerDialog && p.TargetWorker == w.Name && p.Title == ap.Title {
				return
			}
		}
	}
	aid, _, err := e.store.NextID(ctx, "a")
	if err != nil {
		return
	}
	act := &model.Action{
		ID:           aid,
		Kind:         model.ActionKindWorkerDialog,
		Category:     "blocked",
		Thread:       w.Thread,
		WorkItemID:   w.WorkItemID,
		TargetWorker: w.Name,
		PaneID:       w.PaneID,
		Title:        ap.Title,
		Summary:      ap.Detail,
		Command:      ap.Command,
		Choices:      approvalToChoices(ap),
		Status:       model.ActionPending,
		CreatedAt:    time.Now(),
	}
	if err := e.store.InsertAction(ctx, act); err != nil {
		return
	}
	subject := e.formatWorkItemSubject(ctx, w.WorkItemID, w.Name, w.Name)
	e.addEvent(ctx, w.Thread, subject+" 等你确认："+ap.Title, w.Name, "blocked")

	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnActionCreated(ctx, act)
	sink.OnStateChanged()
}

func (e *Engine) handleWorkerTurnDone(ctx context.Context, w *model.Worker) {
	snap := e.lastWorkerTurnSnapshot(ctx, w)
	fullText, fromTranscript, afterLastUser := snap.Text, snap.FromTranscript, snap.AfterLastUser
	summary := fullText
	if summary != "(no output captured)" {
		summary = clipMultiline(fullText, 1200)
	}
	screen := ""
	if rr, err := e.fleet.Read(ctx, w.PaneID, "recent", "text", 40); err == nil {
		screen = rr.Text
	}
	checkAgainst := summary
	if w.LastReport != "" {
		checkAgainst += "\n" + w.LastReport
	}
	qErr := agent.ExtractQuotaError(screen, checkAgainst)
	if peek := e.peekDirectTurn(w.Name); peek != nil && qErr == "" {
		// Never leak a stale assistant reply from a previous turn while the Worker has not yet
		// written an assistant response after the new direct prompt.
		if fromTranscript && !afterLastUser {
			return
		}
		if peek.prevHash != "" && hashTurnText(fullText) == peek.prevHash && time.Since(peek.at) < 30*time.Second {
			return
		}
	}
	dt := e.popDirectTurn(w.Name)
	if qErr != "" {
		e.clearWorkerPendingCheck(w.Name)
		e.disarmWorkerStartupCheck(w.Name)
		if agent.ExtractFatalExecutorError(screen, checkAgainst) != "" {
			e.markWorkerFaulted(ctx, w, w.PaneID, qErr, false)
		}
		if dt != nil && dt.callback != nil {
			res := DirectTurnResult{
				WorkerName:   w.Name,
				Machine:      workerMachineID(w),
				MachineLabel: e.MachineDisplayName(workerMachineID(w)),
				Kind:         w.Kind,
				FullText:     fullText,
				QuotaError:   qErr,
			}
			go dt.callback(context.WithoutCancel(ctx), res)
		}
		wiNote := e.formatWorkerWorkItemContext(ctx, w)
		_, _ = e.IngestWithMeta(ctx, "worker", w.Thread, w.Name,
			fmt.Sprintf("worker %s stopped on an agent/provider error (not finished): %s\nLast message before it:\n%s%s\nHand the job to another kind of agent or computer and inform the human clearly by work item title.", w.Name, qErr, summary, wiNote),
			"", model.RouterName, w.Name, TriggerMeta{WorkItemID: w.WorkItemID})
		subject := e.formatWorkItemSubject(ctx, w.WorkItemID, w.Name, w.Name)
		e.addEvent(ctx, w.Thread, subject+" 出错了："+qErr, w.Name, "error")
		return
	}
	if dt == nil {
		// Global causal turn gate for Router-managed workers: suppress empty/pre-user flickers
		// (retaining pendingCheck for delayed flushes) and already-notified assistant offsets.
		if !e.shouldEmitWorkerTurnSummary(w, snap, time.Now()) {
			return
		}
	} else {
		e.markWorkerAssistantNotified(w, snap)
	}
	artID := ""
	artNote := ""
	if fromTranscript {
		if id := e.captureWorkerReportSnapshot(ctx, w, fullText, 800); id != "" {
			artID = id
			artNote = e.formatReportArtifactNote(id, w.Name)
		}
	}
	if dt != nil {
		if dt.callback != nil {
			res := DirectTurnResult{
				WorkerName:       w.Name,
				Machine:          workerMachineID(w),
				MachineLabel:     e.MachineDisplayName(workerMachineID(w)),
				Kind:             w.Kind,
				FullText:         fullText,
				ReportArtifactID: artID,
				PreviewURL:       e.BuildArtifactShareURL(ctx, artID),
			}
			go dt.callback(context.WithoutCancel(ctx), res)
		}
		note := fmt.Sprintf("worker %s (%s) completed a direct WeCom Q&A. User asked: %s | Reply summary: %s",
			w.Name, e.MachineDisplayName(workerMachineID(w)), oneLineClip(dt.prompt, 160), oneLineClip(summary, 320))
		if artID != "" {
			note += fmt.Sprintf(" [report_artifact=%s]", artID)
		}
		e.appendDirectBgNote(note)
		e.addEvent(ctx, w.Thread, w.Name+" 完成了一轮直聊", w.Name, "done")
		e.notifyChanged()
		return
	}
	turnNote := e.formatWorkerTurnStatusNote(ctx, w, snap)
	wiNote := e.formatWorkerWorkItemContext(ctx, w)
	_, _ = e.IngestWithMeta(ctx, "worker", w.Thread, w.Name,
		fmt.Sprintf("worker %s produced a new update (verify before treating as final acceptance). Last message:\n%s%s%s%s", w.Name, summary, artNote, turnNote, wiNote),
		"", model.RouterName, w.Name, TriggerMeta{WorkItemID: w.WorkItemID, ArtifactID: artID})
	e.addEvent(ctx, w.Thread, w.Name+" 完成了一轮", w.Name, "done")
}

func workerMachineID(w *model.Worker) string {
	if w == nil {
		return ""
	}
	if w.PaneID != "" {
		if _, m := fleet.SplitID(w.PaneID); m != "" {
			return m
		}
	}
	return strings.TrimSpace(w.Machine)
}

func (e *Engine) deliverQueued(ctx context.Context, live map[string]liveInfo, scanned map[string]bool) {
	e.deliverQueuedWithFleet(ctx, live, scanned, true)
}

func (e *Engine) deliverQueuedWithFleet(ctx context.Context, live map[string]liveInfo, scanned map[string]bool, fleetOK bool) {
	queued, err := e.store.ListQueuedTriggers(ctx)
	if err != nil || len(queued) == 0 {
		return
	}
	batches := map[string][]*model.Trigger{}
	for _, t := range queued {
		if len(t.Targets) == 0 {
			if t.Source != "worker" && t.Source != "chat" {
				if th, err := e.store.GetThread(ctx, t.Thread); err == nil {
					for _, sub := range th.Subscribers {
						if w, err := e.store.GetWorker(ctx, sub); err == nil && !w.Ended && sub != t.From {
							t.Targets = append(t.Targets, sub)
						}
					}
				}
			}
			if len(t.Targets) == 0 {
				t.Targets = []string{model.RouterName}
			}
			_ = e.store.ReplaceTriggerTargets(ctx, t.ID, t.Targets)
		}
		for _, target := range t.Targets {
			batches[target] = append(batches[target], t)
		}
	}

	for target, list := range batches {
		if target == model.RouterName {
			e.deliverAPIRouter(ctx, list)
			continue
		}
		if !fleetOK {
			continue
		}
		li, alive := live[target]
		if !alive {
			e.rerouteDeadWorkerTriggers(ctx, target, list, scanned)
			continue
		}
		if li.status == "blocked" || (!li.ready && li.status == "unknown") || e.isHealingBypassWorker(target, li.pane) {
			continue
		}
		if wk, wErr := e.store.GetWorker(ctx, target); wErr == nil && isWorkerFaulted(wk) && (wk.FaultPaneID == "" || wk.FaultPaneID == li.pane) {
			e.rerouteFaultedWorkerTriggers(ctx, wk, list)
			continue
		}
		if li.status == "working" {
			list = filterWorkerSteeringTriggers(list)
			if len(list) == 0 {
				continue
			}
		}
		if e.isDeliveringWorker(target) {
			continue
		}
		if triggersHaveHumanMedia(list) {
			if e.tryBeginWorkerDelivery(target) {
				batchCopy := append([]*model.Trigger(nil), list...)
				if rootCtx := e.backgroundRunCtx(); rootCtx != nil {
					go e.deliverBatchAsync(rootCtx, target, li.pane, batchCopy)
				} else {
					e.deliverBatchAsync(ctx, target, li.pane, batchCopy)
				}
			}
			continue
		}
		e.deliverBatch(ctx, target, li.pane, list)
	}
}

func triggersHaveHumanMedia(list []*model.Trigger) bool {
	for _, t := range list {
		if t != nil && len(t.Media) > 0 && !strings.EqualFold(strings.TrimSpace(t.From), "machine") {
			return true
		}
	}
	return false
}

func (e *Engine) isDeliveringWorker(target string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deliveringWorkers[target]
}

func (e *Engine) tryBeginWorkerDelivery(target string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.deliveringWorkers == nil {
		e.deliveringWorkers = map[string]bool{}
	}
	if e.deliveringWorkers[target] {
		return false
	}
	e.deliveringWorkers[target] = true
	return true
}

func (e *Engine) endWorkerDelivery(target string) {
	e.mu.Lock()
	delete(e.deliveringWorkers, target)
	e.mu.Unlock()
}

func (e *Engine) backgroundRunCtx() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runCtx
}

func (e *Engine) deliverBatchAsync(parent context.Context, target, paneID string, list []*model.Trigger) {
	defer e.endWorkerDelivery(target)
	dctx, cancel := context.WithTimeout(parent, stageBatchTimeout+20*time.Second)
	defer cancel()
	e.deliverBatch(dctx, target, paneID, list)
}

func filterWorkerSteeringTriggers(list []*model.Trigger) []*model.Trigger {
	// While a Worker is busy, queue routine Router instructions on Hub so multiple rapid
	// follow-ups coalesce cleanly by Seq on the next idle boundary (instead of sitting unseen
	// in a CLI PTY input buffer). Only explicit Steer triggers bypass the busy queue.
	var out []*model.Trigger
	for _, t := range list {
		if t.Steer {
			out = append(out, t)
		}
	}
	return out
}

func (e *Engine) rerouteDeadWorkerTriggers(ctx context.Context, target string, list []*model.Trigger, scanned map[string]bool) {
	if w, err := e.store.GetWorker(ctx, target); err == nil && !w.Ended {
		if time.Since(w.CreatedAt) < 2*time.Minute {
			return // still booting
		}
		if scanned != nil && !scanned[workerMachineID(w)] {
			return // worker's machine was not scanned on this tick; keep trigger targeting this worker
		}
	}
	for _, t := range list {
		_ = e.store.RerouteTriggerTarget(ctx, t.ID, target, model.RouterName)
	}
}

func (e *Engine) deliverBatch(ctx context.Context, target, paneID string, list []*model.Trigger) {
	if !e.beginWorkerOp(target) {
		return
	}
	defer e.endWorkerOp(target)
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	if len(list) > 20 {
		list = list[:20]
	}

	var b strings.Builder
	var targetWorker *model.Worker
	var workerConstraint string
	if wk, wErr := e.store.GetWorker(ctx, target); wErr == nil && wk != nil {
		targetWorker = wk
		if strings.TrimSpace(wk.Constraint) != "" {
			workerConstraint = strings.TrimSpace(wk.Constraint)
			b.WriteString(FormatConstraintHeader(workerConstraint))
			b.WriteString("\n\n")
		}
	}
	if len(list) == 1 {
		b.WriteString("[corgi] 1 new event\n")
	} else {
		fmt.Fprintf(&b, "[corgi] %d new events (batched)\n", len(list))
	}
	for _, t := range list {
		fmt.Fprintf(&b, "\n#%s · %s · %s", t.ID, t.Thread, t.Source)
		if t.From != "" {
			fmt.Fprintf(&b, " · from %s", t.From)
		}
		if t.Policy == "candidate" {
			b.WriteString(" · candidate (act only if a rule or memory covers it)")
		}
		fmt.Fprintf(&b, " · %s\n", t.At.Format("15:04"))
		body := strings.TrimSpace(t.Text)
		if r := []rune(body); len(r) > 700 { // Inline & Spill
			body = string(r[:700]) + fmt.Sprintf(" … [spilled; run `corgi show %s`]", t.ID)
		}
		if targetWorker != nil && len(t.Media) > 0 && !strings.EqualFold(strings.TrimSpace(t.From), "machine") {
			stagedBody, _, sErr := e.stageMediaForDirectText(ctx, workerMachineID(targetWorker), body, t.Media)
			if sErr != nil {
				e.logger.Warn("stage trigger media for worker failed", "target", target, "trigger", t.ID, "err", sErr)
				return
			}
			body = stagedBody
		}
		b.WriteString("  " + strings.ReplaceAll(body, "\n", "\n  ") + "\n")
	}

	promptTarget := target
	if paneID != "" {
		promptTarget = paneID
	}
	var preScreen string
	var preSnap workerTurnSnapshot
	if targetWorker != nil {
		if rr, rErr := e.fleet.Read(ctx, promptTarget, "recent", "text", 30); rErr == nil {
			preScreen = rr.Text
		}
		preSnap = e.lastWorkerTurnSnapshot(ctx, targetWorker)
	}
	promptBody := b.String()
	if err := e.fleet.Prompt(ctx, promptTarget, promptBody); err != nil {
		var he *herdr.Error
		if errors.As(err, &he) && (he.Code == "agent_blocked" || he.Code == "agent_not_ready") {
			return
		}
		e.logger.Warn("deliver trigger batch failed", "target", target, "err", err)
		return
	}

	if workerConstraint != "" && paneID != "" {
		e.markConstraintDelivered(paneID)
	}
	e.recordWorkerDelivered(target, paneID, list)
	e.resetWorkerIdleByName(ctx, target)
	if targetWorker != nil {
		e.armWorkerStartupCheck(targetWorker, paneID, list, promptBody, preScreen, preSnap, time.Now())
	}

	for _, t := range list {
		_ = e.store.AckTriggerTarget(ctx, t.ID, target)
	}
	e.notifyChanged()
}

// RestartRouter cancels any in-flight native API Router turn and resets backoff/auth state.
func (e *Engine) RestartRouter(ctx context.Context) error {
	e.cancelAndSettleAPIRouterTurn(ctx, "", true)
	e.mu.Lock()
	e.router.resetFailuresLocked(false)
	e.mu.Unlock()
	e.wake()
	e.notifyChanged()
	return nil
}

func (e *Engine) checkStalls(ctx context.Context, live map[string]liveInfo, now time.Time) {
	stallDur := time.Duration(e.Settings().StallMinutes) * time.Minute
	if stallDur <= 0 {
		return
	}
	workers, err := e.store.ListWorkers(ctx)
	if err != nil {
		return
	}
	for _, w := range workers {
		if w.Ended {
			continue
		}
		li, ok := live[w.Name]
		if !ok || li.status != "working" || e.hasDirectTurn(w.Name) {
			e.mu.Lock()
			delete(e.lastScreen, w.Name)
			e.mu.Unlock()
			continue
		}
		rr, err := e.fleet.Read(ctx, li.pane, "detection", "text", 0)
		if err != nil {
			continue
		}
		h := agent.ProgressHash(rr.Text)

		e.mu.Lock()
		mk, seen := e.lastScreen[w.Name]
		if !seen || mk.hash != h {
			e.lastScreen[w.Name] = screenMark{hash: h, since: now}
			e.mu.Unlock()
			continue
		}
		stalled := !mk.told && now.Sub(mk.since) >= stallDur
		if stalled {
			mk.told = true
			e.lastScreen[w.Name] = mk
		}
		e.mu.Unlock()

		if stalled {
			_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
				fmt.Sprintf("watchdog: worker %s screen has not changed for %d minutes. Inspect it, nudge it, or switch approach.", w.Name, int(stallDur.Minutes())),
				"", model.RouterName, w.Name)
		}
	}
}

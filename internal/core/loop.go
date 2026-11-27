package core

import (
	"context"
	"encoding/json"
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
	e.EmitPendingAutoUpdateNotice(ctx)
	if changed, err := e.memory.WriteRouterDocsChanged(e.binPath, e.Settings()); err != nil {
		e.logger.Warn("write router docs", "err", err)
	} else if changed {
		e.queueRouterDocReread(ctx)
	}
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
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

	rawAgents, scanned, err := e.fleet.AgentsWithScanned(ctx)
	if err != nil {
		return // herdr temporarily unreachable; preserve queued triggers
	}
	live := map[string]liveInfo{}
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
		li := liveInfo{pane: a.PaneID, status: a.AgentStatus, ready: a.Ready}
		if a.Agent != nil {
			li.kind = *a.Agent
		}
		if a.Session != nil {
			li.session = extractSessionID(a.Session.Kind, a.Session.Value)
		}
		if *a.Name == model.RouterName {
			li.status = e.stabilizeRouterStatus(a.PaneID, li.kind, li.status, now)
		}
		live[*a.Name] = li
	}
	if _, alive := live[model.RouterName]; !alive {
		e.clearRouterLive()
	}

	// Auto-adopt any live non-system agent session into Corgi's managed workers table
	// so no orphan unmanaged sessions ever exist.
	e.adoptUnmanagedAgents(ctx, rawAgents)

	// 2. Sync worker states, detect bare-shell failures, create Actions on blocked dialogs or quota errors.
	e.syncWorkers(ctx, live, scanned, now)

	// 3. Guard Router against trust blocks, transient API errors, and stalled turns.
	e.handleRouterLifecycle(ctx, live, now)

	queued, _ := e.store.ListQueuedTriggers(ctx)
	hasRouterQueued := false
	for _, t := range queued {
		if len(t.Targets) == 0 || slices.Contains(t.Targets, model.RouterName) {
			hasRouterQueued = true
			break
		}
	}

	// 4. Drive or trigger nightly memory consolidation (Dream) and context handover.
	e.checkDream(ctx, live, hasRouterQueued)
	e.checkHandover(ctx, live, hasRouterQueued)

	if e.Settings().Paused || e.Dreaming() || e.handingOver() || e.isHubUpdating() {
		return
	}

	// 5. Drain idle main-chat queue, route/deliver triggers, and drain idle single-agent queues.
	e.drainRouterPrompts(ctx, live)
	e.deliverQueued(ctx, live, scanned)
	e.drainPanePrompts(ctx, rawAgents)

	// 6. Watchdog for stalled/thrashing workers and zero-token gated idle patrol / morning digest.
	e.checkStalls(ctx, live, now)
	e.checkWorkerThrash(ctx, live, now)
	e.checkIdlePatrol(ctx, live, now)

	// 7. Optional overnight idle auto-update after tonight's scheduled Dream succeeds/skips.
	e.checkAutoUpdate(ctx, live, hasRouterQueued, now)
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

	changed := false
	for _, a := range rawAgents {
		if a.Name == nil || *a.Name == "" || a.PaneID == "" || a.Agent == nil {
			continue
		}
		name := *a.Name
		if name == model.RouterName || name == model.DreamName {
			continue
		}
		if byPane[a.PaneID] != nil {
			continue
		}
		if prev := byName[name]; prev != nil {
			if !prev.Ended {
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
			continue
		}
		li, alive := live[w.Name]
		if !alive {
			if e.isHealingBypassWorker(w.Name, w.PaneID) {
				continue
			}
			wkMachine := workerMachineID(w)
			if scanned != nil && !scanned[wkMachine] {
				// Machine did not complete a scan on this tick (e.g. Node restarting/reconnecting);
				// preserve worker state and pending approvals until its machine is observed again.
				continue
			}
			if w.Status != "starting" && time.Since(w.CreatedAt) > 20*time.Second {
				w.Ended = true
				w.Status = "gone"
				w.FaultError = ""
				w.FaultPaneID = ""
				_ = e.store.UpsertWorker(ctx, w)
				_ = e.store.ExpireWorkerActions(ctx, w.Name)
				_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
					fmt.Sprintf("worker %s exited (pane gone).", w.Name), "", model.RouterName, w.Name)
				e.addEvent(ctx, w.Thread, w.Name+" 已退出", w.Name, "info")
				e.notifyChanged()
			}
			continue
		}
		if e.isHealingBypassWorker(w.Name, li.pane) {
			continue
		}
		if e.checkWorkerBareShell(ctx, w, li, now) {
			continue
		}
		sessionChanged := false
		if li.session != "" && li.session != w.Session {
			w.Session = li.session
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
				w.IdleSince = &t
				idleChanged = true
			}
		} else if w.IdleSince != nil || w.IdlePatrolCount > 0 || w.SnoozeUntil != nil {
			resetWorkerIdle(w)
			idleChanged = true
		}
		if li.status == "working" || li.status == "blocked" || (w.FaultPaneID != "" && li.pane != "" && li.pane != w.FaultPaneID) {
			e.disarmWorkerStartupCheck(w.Name)
			if w.FaultError != "" || w.FaultPaneID != "" {
				w.FaultError = ""
				w.FaultPaneID = ""
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
			continue
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
			if e.hasDirectTurn(w.Name) || now.Sub(w.ReportAt) > 90*time.Second {
				e.handleWorkerTurnDone(ctx, w)
			}
		case (li.status == "done" || li.status == "idle") && e.hasWorkerStartupCheck(w.Name):
			e.checkWorkerStartupHealth(ctx, w, li, now)
		}
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
	summary := e.lastWorkerSummary(ctx, w)
	choices := formatChoicesText(ap)
	e.addEvent(ctx, w.Thread, w.Name+" 等你确认："+ap.Title, w.Name, "blocked")
	_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
		fmt.Sprintf("worker %s is BLOCKED on a terminal dialog (also shown as a card in chat).\nScreen:\n%s%s\nIf the answer follows from the task or is routine, answer it yourself: `%s answer %s <choice>`. Leave high-risk choices to the human.",
			w.Name, summary, choices, e.binPath, w.Name),
		"", model.RouterName, w.Name)

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
			e.markWorkerFaulted(ctx, w, w.PaneID, qErr)
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
		_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
			fmt.Sprintf("worker %s stopped on an agent/provider error (not finished): %s\nLast message before it:\n%s\nHand the job to another kind of agent or computer and inform the human in one line.", w.Name, qErr, summary),
			"", model.RouterName, w.Name)
		e.addEvent(ctx, w.Thread, w.Name+" 出错了："+qErr, w.Name, "error")
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
			artNote = fmt.Sprintf("\n[full_report_artifact: %s — full untruncated report snapshotted on Hub; pass --re <this event #tID> --worker %s on `corgi reply` (when not using --file) to attach this exact snapshot]", id, w.Name)
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
	trig, err := e.Ingest(ctx, "worker", w.Thread, w.Name,
		fmt.Sprintf("worker %s produced a new update (verify before treating as final acceptance). Last message:\n%s%s%s", w.Name, summary, artNote, turnNote), "", model.RouterName, w.Name)
	if err == nil && trig != nil && artID != "" {
		trig.ArtifactID = artID
		_ = e.store.UpdateTrigger(ctx, trig)
	}
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
		}
		for _, target := range t.Targets {
			batches[target] = append(batches[target], t)
		}
	}

	for target, list := range batches {
		li, alive := live[target]
		if !alive {
			if target == model.RouterName {
				e.ensureRouter(ctx)
			} else {
				e.rerouteDeadWorkerTriggers(ctx, target, list, scanned)
			}
			continue
		}
		if li.status == "blocked" || (!li.ready && li.status == "unknown") || e.isHealingBypassWorker(target, li.pane) {
			continue
		}
		if target != model.RouterName {
			if wk, wErr := e.store.GetWorker(ctx, target); wErr == nil && isWorkerFaulted(wk) && (wk.FaultPaneID == "" || wk.FaultPaneID == li.pane) {
				e.rerouteFaultedWorkerTriggers(ctx, wk, list)
				continue
			}
		}
		// Human chat/wecom messages interrupt Router mid-turn; system events wait for idle.
		if li.status == "working" {
			if target == model.RouterName {
				list = filterHumanTriggers(list)
			} else {
				list = filterWorkerSteeringTriggers(list)
			}
			if len(list) == 0 {
				continue
			}
		}
		e.deliverBatch(ctx, target, li.pane, list)
	}
}

func filterHumanTriggers(list []*model.Trigger) []*model.Trigger {
	var out []*model.Trigger
	for _, t := range list {
		if t.Steer || t.Source == "wecom" {
			out = append(out, t)
		}
	}
	return out
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
		t.Targets = removeStr(t.Targets, target)
		if !slices.Contains(t.Targets, model.RouterName) {
			t.Targets = append(t.Targets, model.RouterName)
		}
		_ = e.store.UpdateTrigger(ctx, t)
	}
}

func (e *Engine) deliverBatch(ctx context.Context, target, paneID string, list []*model.Trigger) {
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	if len(list) > 20 {
		list = list[:20]
	}

	e.mu.Lock()
	fresh := target == model.RouterName && e.routerFresh
	chars := e.settings.ReplyChars
	e.mu.Unlock()

	var b strings.Builder
	var targetWorker *model.Worker
	var workerConstraint string
	if target != model.RouterName {
		if wk, wErr := e.store.GetWorker(ctx, target); wErr == nil && wk != nil {
			targetWorker = wk
			if strings.TrimSpace(wk.Constraint) != "" {
				workerConstraint = strings.TrimSpace(wk.Constraint)
				b.WriteString(FormatConstraintHeader(workerConstraint))
				b.WriteString("\n\n")
			}
		}
	}
	if fresh {
		b.WriteString(e.memory.SessionStartPreamble())
		b.WriteString("\n")
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
			showBin := e.binPath
			if target != model.RouterName || showBin == "" {
				showBin = "corgi"
			}
			body = string(r[:700]) + fmt.Sprintf(" … [spilled; run `%s show %s`]", showBin, t.ID)
		}
		b.WriteString("  " + strings.ReplaceAll(body, "\n", "\n  ") + "\n")
	}
	if target == model.RouterName {
		injectWork := fresh
		if !injectWork {
			for _, t := range list {
				if t.Source == "chat" || t.Source == "wecom" {
					injectWork = true
					break
				}
			}
		}
		if injectWork {
			if openWork, wErr := e.store.ListWorkItems(ctx, false); wErr == nil && len(openWork) > 0 {
				b.WriteString(formatOpenWorkItemsSummary(openWork, e.binPath))
			}
		}
		if bgNotes := e.drainDirectBgNotes(); len(bgNotes) > 0 {
			b.WriteString("\n[background: completed direct worker chats (already delivered straight to user; remember for context, do NOT re-push)]\n")
			for _, n := range bgNotes {
				b.WriteString("- " + n + "\n")
			}
		}
		fmt.Fprintf(&b, "\nReply with `%s reply` (usually ≤%d Chinese chars / English words; for 2+ items use `- ` Markdown bullets via stdin heredoc `<<'EOF'`, never pack into one semicolon-separated line).", e.binPath, chars)
	}

	if target == model.RouterName {
		e.mu.Lock()
		if e.hubUpdating || e.dream != nil || e.handover != nil || e.routerDispatching {
			e.mu.Unlock()
			return
		}
		e.routerDispatching = true
		e.mu.Unlock()
		defer func() {
			e.mu.Lock()
			e.routerDispatching = false
			e.mu.Unlock()
		}()
	}

	promptTarget := target
	if paneID != "" {
		promptTarget = paneID
	}
	var preScreen string
	var preSnap workerTurnSnapshot
	if target != model.RouterName && targetWorker != nil {
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

	e.mu.Lock()
	if fresh {
		e.routerFresh = false
	}
	if target == model.RouterName {
		if e.routerAskedAt.IsZero() {
			e.routerAskedAt = time.Now()
		}
		e.routerRepliedAt = time.Time{}
		e.routerTrueIdleSince = time.Time{}
	}
	e.mu.Unlock()
	if workerConstraint != "" && paneID != "" {
		e.markConstraintDelivered(paneID)
	}
	if target != model.RouterName {
		e.recordWorkerDelivered(target, paneID, list)
		if targetWorker != nil {
			e.armWorkerStartupCheck(targetWorker, paneID, list, promptBody, preScreen, preSnap, time.Now())
		}
	}

	for _, t := range list {
		t.Targets = removeStr(t.Targets, target)
		if len(t.Targets) == 0 {
			t.Status = "delivered"
		}
		_ = e.store.UpdateTrigger(ctx, t)
	}
	e.notifyChanged()
}

// ensureRouter launches the resident Router session on the hub machine if absent.
func (e *Engine) ensureRouter(ctx context.Context) {
	if e.fleet == nil {
		return
	}
	e.mu.Lock()
	if e.routerLaunching || e.hubUpdating {
		e.mu.Unlock()
		return
	}
	e.routerLaunching = true
	st := e.settings
	e.mu.Unlock()

	go func() {
		defer func() {
			e.mu.Lock()
			e.routerLaunching = false
			e.mu.Unlock()
			e.notifyChanged()
		}()
		bg, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		kind := agent.NormalizeKind(st.RouterKind)
		if kind == "" {
			kind = "claude"
		}
		if e.agents.CheckInstalled(e.home, kind) != nil {
			installed := e.agents.DetectInstalled(e.home, true)
			if len(installed) == 0 {
				e.logger.Error("no agent CLI installed on hub for router", "configured", kind)
				return
			}
			e.logger.Warn("configured router agent not installed on hub, falling back", "configured", kind, "fallback", installed[0])
			kind = installed[0]
		}
		ad, err := e.agents.Get(kind)
		if err != nil {
			e.logger.Error("router adapter invalid", "kind", kind, "err", err)
			return
		}
		if err := e.memory.WriteRouterDocs(e.binPath, st); err != nil {
			e.logger.Warn("write router docs", "err", err)
		}
		e.memory.MarkRouterDocsRead()
		e.mu.Lock()
		rereadID := e.rereadTrigID
		e.rereadTrigID = ""
		e.mu.Unlock()
		if rereadID != "" {
			if tr, err := e.store.GetTrigger(bg, rereadID); err == nil && tr.Status == "queued" {
				tr.Status = "ignored"
				_ = e.store.UpdateTrigger(bg, tr)
			}
		}
		if !st.AskPermissions {
			_ = ad.PreTrustWorkspace(e.home, e.memory.RouterDir())
		}
		args := ad.LaunchArgs(st.RouterModel, st.RouterEffort, !st.AskPermissions)
		e.closeWorkspacesByLabel(bg, model.RouterLabel)
		wsID, paneID, err := e.fleet.CreateWorkspace(bg, "", e.memory.RouterDir(), model.RouterLabel)
		if err != nil {
			e.logger.Error("create router workspace failed", "err", err)
			return
		}
		if err := e.fleet.StartAgent(bg, paneID, kind, model.RouterName, args); err != nil {
			var he *herdr.Error
			if !(errors.As(err, &he) && he.Code == "agent_not_ready") {
				e.logger.Error("start router agent failed, closing workspace", "kind", kind, "err", err)
				_ = e.fleet.CloseWorkspace(bg, wsID)
				return
			}
		}
		e.fleet.RememberName(paneID, model.RouterName)
		e.fleet.RememberRun(paneID, st.RouterModel, st.RouterEffort)

		runPair := [2]string{st.RouterModel, st.RouterEffort}
		if b, err := json.Marshal(runPair); err == nil {
			_ = e.store.SetKV(bg, "router_run", string(b))
		}

		e.mu.Lock()
		e.routerFresh = true
		e.routerLaunchAt = time.Now()
		e.routerRun = &runPair
		e.routerStallAlerted = false
		e.routerRetryPending = false
		e.mu.Unlock()
	}()
}

// RestartRouter closes the current Router workspace so the next tick spawns a fresh session.
func (e *Engine) RestartRouter(ctx context.Context) error {
	if e.fleet == nil {
		return fmt.Errorf("fleet not configured")
	}
	ws, err := e.fleet.Workspaces(ctx)
	if err != nil {
		e.logger.Warn("restart router: list workspaces failed", "err", err)
		return fmt.Errorf("list workspaces: %w", err)
	}
	var closeErr error
	for _, w := range ws {
		if w.Label == model.RouterLabel {
			if cErr := e.fleet.CloseWorkspace(ctx, w.ID); cErr != nil {
				e.logger.Warn("restart router: close workspace failed", "workspace_id", w.ID, "err", cErr)
				if closeErr == nil {
					closeErr = fmt.Errorf("close router workspace %s: %w", w.ID, cErr)
				}
			}
		}
	}
	if closeErr != nil {
		return closeErr
	}
	_ = e.store.SetKV(ctx, "router_run", "")
	e.mu.Lock()
	e.routerFresh = true
	e.routerAskedAt = time.Time{}
	e.routerRepliedAt = time.Time{}
	e.routerRetries = 0
	e.routerRetryPending = false
	e.routerNudged = false
	e.routerStallAlerted = false
	e.routerRun = nil
	e.routerLastAlive = false
	e.routerLastPane = ""
	e.routerLastStatus = ""
	e.routerLastSeenAt = time.Time{}
	e.routerWorkingAt = time.Time{}
	e.routerIdleSince = time.Time{}
	e.mu.Unlock()
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

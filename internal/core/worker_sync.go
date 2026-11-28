package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

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

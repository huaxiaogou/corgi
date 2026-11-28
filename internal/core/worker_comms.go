package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

// SendToWorker queues a steering message for an active worker, or adopts a live
// manually-started agent matching workerName so the Router can delegate directly to it.
func (e *Engine) SendToWorker(ctx context.Context, workerName, thread, text string) (*model.Trigger, error) {
	trig, _, err := e.SendToWorkerWithWork(ctx, workerName, thread, text, "")
	return trig, err
}

// SendToWorkerWithWork steers an active or newly-adopted worker while ensuring durable work-item linkage:
// explicit workID attaches to that item; otherwise inherits the worker's open item, requires disambiguation
// when multiple open items reference the worker, or creates a new item if unassigned.
func (e *Engine) SendToWorkerWithWork(ctx context.Context, workerName, thread, text, workID string) (*model.Trigger, string, error) {
	return e.SendToWorkerWithWorkTitle(ctx, workerName, thread, text, workID, "")
}

// SendToWorkerWithWorkTitle steers an active or newly-adopted worker while ensuring durable
// work-item linkage, using explicit workTitle when creating a brand-new work item.
func (e *Engine) SendToWorkerWithWorkTitle(ctx context.Context, workerName, thread, text, workID, workTitle string) (*model.Trigger, string, error) {
	e.waitWorkerGCIdle(ctx, workerName, 3*time.Second)
	if !e.beginWorkerOp(workerName) {
		return nil, "", fmt.Errorf("worker %q is hibernating; retry or resume session", workerName)
	}
	defer e.endWorkerOp(workerName)

	wk, err := e.store.GetWorker(ctx, workerName)
	if err != nil || wk.Ended || wk.Hibernating {
		adopted, adoptErr := e.adoptLiveAgent(ctx, workerName, thread, text)
		if adoptErr != nil {
			return nil, "", fmt.Errorf("no running worker or live agent named %q", workerName)
		}
		wk = adopted
	}
	if isWorkerFaulted(wk) {
		return nil, "", fmt.Errorf("worker %q is paused due to fatal error (%s); call manage_work(action=\"stop_worker\", target=%q) and dispatch to a healthy worker/model", wk.Name, oneLineClip(wk.FaultError, 120), wk.Name)
	}
	if thread == "" {
		thread = wk.Thread
	}
	prevBranch := wk.WorktreeBranch
	if wk.IdleSince != nil || wk.IdlePatrolCount > 0 || wk.SnoozeUntil != nil {
		resetWorkerIdle(wk)
	}

	targetWorkID := strings.TrimSpace(workID)
	if targetWorkID == "" && wk.WorkItemID != "" {
		if existingWI, wiErr := e.store.GetWorkItem(ctx, wk.WorkItemID); wiErr == nil && existingWI.ClosedAt == nil {
			targetWorkID = existingWI.ID
		}
	}
	if targetWorkID == "" {
		matches, mErr := e.store.FindOpenWorkItemsByWorker(ctx, wk.Name)
		if mErr != nil {
			return nil, "", mErr
		}
		if len(matches) == 1 {
			targetWorkID = matches[0].ID
		} else if len(matches) > 1 {
			ids := make([]string, 0, len(matches))
			for _, m := range matches {
				ids = append(ids, m.ID)
			}
			return nil, "", fmt.Errorf("worker %q belongs to multiple open work items (%s); specify --work <id>", wk.Name, strings.Join(ids, ", "))
		}
	}
	var newWorkID string
	if targetWorkID == "" {
		var idErr error
		newWorkID, _, idErr = e.store.NextID(ctx, "wi")
		if idErr != nil {
			return nil, "", idErr
		}
	}
	wiTitle, isDraft := DeriveWorkItemTitle(workTitle, text, "")
	wi, err := e.store.AttachWorkerAndUpsert(ctx, wk, targetWorkID, newWorkID, wiTitle, "")
	if err != nil {
		return nil, "", err
	}
	if targetWorkID == "" && isDraft && wi != nil {
		_ = e.store.SetWorkItemTitleState(ctx, wi.ID, store.WorkItemTitleDraft)
	}
	if prevBranch != "" && wi != nil {
		_ = e.store.RecordWorkItemBranch(ctx, wi.ID, prevBranch)
	}
	trig, err := e.IngestWithMeta(ctx, "router", thread, model.RouterName, text, "", wk.Name, wk.Name, TriggerMeta{WorkItemID: wk.WorkItemID})
	if err != nil {
		return nil, "", err
	}
	if trig != nil {
		e.recordWorkerIssued(wk, trig.ID, trig.Seq)
	}
	return trig, wk.WorkItemID, nil
}

func (e *Engine) adoptLiveAgent(ctx context.Context, target, thread, task string) (*model.Worker, error) {
	if e.fleet == nil {
		return nil, fmt.Errorf("fleet unavailable")
	}
	agents, err := e.fleet.Agents(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		name := ""
		if a.Name != nil {
			name = *a.Name
		}
		if name == "" || name == model.RouterName || name == model.DreamName {
			continue
		}
		if name != target && a.PaneID != target {
			continue
		}
		_, machine := fleet.SplitID(a.PaneID)
		kind := ""
		if a.Agent != nil {
			kind = agent.NormalizeKind(*a.Agent)
		}
		cwd := ""
		if a.Cwd != nil {
			cwd = *a.Cwd
		}
		if thread == "" {
			thread = model.MainThread
		}
		wk := &model.Worker{
			Name:        name,
			Kind:        kind,
			Machine:     machine,
			Cwd:         cwd,
			Task:        oneLineClip(task, 200),
			Thread:      thread,
			PaneID:      a.PaneID,
			WorkspaceID: a.WorkspaceID,
			CreatedBy:   "adopted",
			CreatedAt:   time.Now(),
			Status:      orStr(a.AgentStatus, "idle"),
		}
		if err := e.store.UpsertWorker(ctx, wk); err != nil {
			return nil, err
		}
		e.fleet.RememberName(a.PaneID, name)
		e.addEvent(ctx, thread, fmt.Sprintf("接管已有会话 %s（%s%s）", name, kind, machineTag(machine, machine == "")), name, "spawn")
		e.notifyChanged()
		return wk, nil
	}
	return nil, fmt.Errorf("live agent %q not found", target)
}

// Report receives a worker's progress or completion report and routes it to the Router.
func (e *Engine) Report(ctx context.Context, workerName, paneID, text string) (*model.Trigger, error) {
	var wk *model.Worker
	if workerName != "" {
		wk, _ = e.store.GetWorker(ctx, workerName)
	}
	if wk == nil && paneID != "" {
		if all, err := e.store.ListWorkers(ctx); err == nil {
			for _, w := range all {
				local, _ := fleet.SplitID(w.PaneID)
				if !w.Ended && (w.PaneID == paneID || local == paneID) {
					wk = w
					break
				}
			}
		}
	}
	thread := model.MainThread
	from := orStr(workerName, "worker")
	artID := ""
	artNote := ""
	turnNote := ""
	if wk != nil {
		from = wk.Name
		thread = wk.Thread
		wk.LastReport = text
		wk.ReportAt = time.Now()
		wk.HadTurn = true
		wk.FaultError = ""
		wk.FaultPaneID = ""
		wk.FaultAt = nil
		wk.StartupFailed = false
		_ = e.store.UpsertWorker(ctx, wk)
		e.disarmWorkerStartupCheck(wk.Name)
		if id := e.captureWorkerReportSnapshot(ctx, wk, text, 500); id != "" {
			artID = id
			artNote = e.formatReportArtifactNote(id, wk.Name)
		}
		snap := e.lastWorkerTurnSnapshot(ctx, wk)
		turnNote = e.formatWorkerTurnStatusNote(ctx, wk, snap)
		e.recordWorkerExplicitReport(wk, snap, artID, isProgressReportText(text), wk.ReportAt)
	}
	workID := ""
	wiNote := ""
	if wk != nil {
		workID = wk.WorkItemID
		wiNote = e.formatWorkerWorkItemContext(ctx, wk)
	}
	subject := e.formatWorkItemSubject(ctx, workID, from, from)
	e.addEvent(ctx, thread, fmt.Sprintf("%s 汇报：%s", subject, oneLineClip(text, 120)), from, "done")
	trig, err := e.IngestWithMeta(ctx, "worker", thread, from, fmt.Sprintf("report from %s:\n%s%s%s%s", from, text, artNote, turnNote, wiNote), "", model.RouterName, from, TriggerMeta{
		WorkItemID: workID,
		ArtifactID: artID,
	})
	return trig, err
}

func (e *Engine) formatReportArtifactNote(artID, workerName string) string {
	return fmt.Sprintf("\n[full_report_artifact: %s — full untruncated report snapshotted on Hub; pass re_id=<this trigger_id> and worker=%q on reply_user (when not using files) to attach this exact snapshot]", artID, workerName)
}

// Subscribe subscribes or unsubscribes a worker to a thread's incoming events.
func (e *Engine) Subscribe(ctx context.Context, workerName, thread string, off bool) error {
	if thread == "" || thread == model.MainThread {
		return fmt.Errorf("chat:main always stays with Corgi router")
	}
	th, err := e.store.GetThread(ctx, thread)
	if err != nil {
		th = &model.Thread{ID: thread, Subscribers: []string{}, LastAt: time.Now()}
	}
	if off {
		th.Subscribers = removeStr(th.Subscribers, workerName)
	} else if !slices.Contains(th.Subscribers, workerName) {
		th.Subscribers = append(th.Subscribers, workerName)
	}
	th.LastAt = time.Now()
	return e.store.UpsertThread(ctx, th)
}

// StopWorker closes a worker's herdr workspace while preserving its session ID for --resume.
func (e *Engine) StopWorker(ctx context.Context, workerName string) error {
	e.waitWorkerGCIdle(ctx, workerName, 3*time.Second)
	if !e.beginWorkerOp(workerName) {
		return fmt.Errorf("worker %q is currently hibernating; retry shortly", workerName)
	}
	defer e.endWorkerOp(workerName)

	wk, err := e.store.GetWorker(ctx, workerName)
	if err != nil {
		return fmt.Errorf("worker %q not found", workerName)
	}
	if wk.Session == "" && wk.PaneID != "" {
		if sess := e.lookupRemoteSession(ctx, wk.PaneID); sess != "" {
			wk.Session = sess
		}
	}
	if agent.NormalizeKind(wk.Kind) == "codex" && wk.Session != "" {
		if err := e.stopCodexTurn(ctx, wk.Machine, wk.Session); err != nil {
			e.logger.Warn("interrupt codex daemon turn failed", "worker", wk.Name, "machine", wk.Machine, "err", err)
		}
	}
	if wk.PaneID != "" && e.fleet != nil {
		// Press Esc first so any running turn aborts cleanly before closing the workspace.
		_ = e.fleet.SendKeys(ctx, wk.PaneID, "esc")
		time.Sleep(600 * time.Millisecond)
	}
	if wk.WorkspaceID != "" && e.fleet != nil {
		_ = e.fleet.CloseWorkspace(ctx, wk.WorkspaceID)
	}
	if wk.PaneID != "" && e.agents != nil {
		if lp, m := fleet.SplitID(wk.PaneID); m == "" && lp != "" {
			e.agents.ReleasePane(lp)
		}
	}
	if wk.PaneID != "" {
		e.recordRecentlyClosedPane(wk.PaneID, time.Now())
	}
	subs := e.collectWorkerSubscribers(ctx, wk.Name)
	if wk.WorkItemID != "" && wk.WorktreeBranch != "" {
		_ = e.store.RecordWorkItemBranch(ctx, wk.WorkItemID, wk.WorktreeBranch)
	}
	wtNote := ""
	if wk.WorktreePath != "" && wk.RepoRoot != "" {
		if clRes, clErr := e.cleanupWorktreeOnMachine(ctx, wk.Machine, wk.RepoRoot, wk.WorktreePath, wk.WorktreeBranch, wk.WorktreeBase); clErr != nil {
			wtNote = "；Worktree 清理跳过：" + clErr.Error()
		} else {
			if clRes.RemovedDir {
				wk.WorktreePath = ""
				wk.Cwd = wk.RepoRoot
			}
			if clRes.DeletedBranch {
				wk.WorktreeBranch = ""
			}
			if clRes.Note != "" {
				wtNote = "；" + clRes.Note
			}
		}
	}
	if wk.Session != "" {
		histCwd := wk.Cwd
		if wk.WorktreePath != "" && wk.RepoRoot != "" {
			histCwd, _ = e.resolveResumeCwd(ctx, wk.Machine, wk.Cwd, wk.RepoRoot)
		}
		if es := e.buildWorkerEndedSession(ctx, wk, histCwd, subs, time.Now()); es != nil {
			_ = e.store.SaveEndedSession(ctx, es, store.EndedSessionRetentionLimit)
		}
	}
	wk.Ended = true
	wk.Hibernating = false
	wk.Status = "done"
	wk.FaultError = ""
	wk.FaultPaneID = ""
	wk.FaultAt = nil
	wk.StartupFailed = false
	if err := e.store.UpsertWorker(ctx, wk); err != nil {
		return err
	}
	if threads, tErr := e.store.ListThreads(ctx, 200); tErr == nil {
		for _, th := range threads {
			if slices.Contains(th.Subscribers, wk.Name) {
				th.Subscribers = removeStr(th.Subscribers, wk.Name)
				_ = e.store.UpsertThread(ctx, th)
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
	e.addEvent(ctx, wk.Thread, "已停止 "+wk.Name+"（会话已保留，可随时续接"+wtNote+"）", wk.Name, "info")
	e.notifyChanged()
	return nil
}

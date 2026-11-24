package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

func (e *Engine) Spawn(ctx context.Context, req SpawnRequest) (*model.Worker, error) {
	if strings.TrimSpace(req.Task) == "" {
		return nil, fmt.Errorf("task is required")
	}
	st := e.Settings()
	var sessionID string
	if req.Resume != "" {
		old, err := e.store.GetWorker(ctx, req.Resume)
		if err != nil {
			return nil, fmt.Errorf("cannot resume worker %q: %w", req.Resume, err)
		}
		if old.Session == "" {
			return nil, fmt.Errorf("worker %q has no saved session id to resume", req.Resume)
		}
		req.Kind, req.Cwd, req.Machine, sessionID = old.Kind, old.Cwd, old.Machine, old.Session
		if req.Name == "" {
			req.Name = old.Name
		}
		if req.Thread == "" {
			req.Thread = old.Thread
		}
	}
	kind := agent.NormalizeKind(req.Kind)
	if kind == "" {
		kind = agent.NormalizeKind(st.WorkerKind)
	}
	if kind == "" {
		kind = "claude"
	}
	ad, err := e.agents.Get(kind)
	if err != nil {
		return nil, err
	}
	machine := st.WorkerMachine
	if req.Machine != "" {
		machine = e.fleet.ResolveMachine(req.Machine)
	}
	cwd := strings.TrimSpace(req.Cwd)
	if cwd == "" {
		cwd = st.WorkerCwd
	}
	thread := strings.TrimSpace(req.Thread)
	if thread == "" {
		thread = model.MainThread
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		_, seq, _ := e.store.NextID(ctx, "w")
		name = fmt.Sprintf("%s-%d", kind, seq)
	}
	if !workerNameRe.MatchString(name) || name == model.RouterName {
		return nil, fmt.Errorf("invalid worker name %q", name)
	}
	modelName, effort := req.Model, req.Effort
	if modelName == "" && kind == agent.NormalizeKind(st.WorkerKind) {
		modelName, effort = st.WorkerModel, st.WorkerEffort
	}

	if machine == "" {
		if err := e.agents.CheckInstalled(e.home, kind); err != nil {
			return nil, err
		}
		if !st.AskPermissions {
			_ = ad.PreTrustWorkspace(e.home, cwd)
		}
	}

	args := ad.LaunchArgs(modelName, effort, !st.AskPermissions)
	if sessionID != "" {
		args = append(args, ad.ResumeArgs(sessionID)...)
	}

	wsID, paneID, err := e.fleet.CreateWorkspace(ctx, machine, cwd, "corgi · "+name)
	if err != nil {
		return nil, fmt.Errorf("create workspace: %w", err)
	}
	if err := e.fleet.StartAgent(ctx, paneID, kind, name, args); err != nil {
		var he *herdr.Error
		if !(errors.As(err, &he) && he.Code == "agent_not_ready") {
			return nil, fmt.Errorf("start agent %s: %w", name, err)
		}
	}
	e.fleet.RememberName(paneID, name)
	e.fleet.RememberRun(paneID, modelName, effort)

	wk := &model.Worker{
		Name:        name,
		Kind:        kind,
		Machine:     machine,
		Cwd:         cwd,
		Task:        req.Task,
		Thread:      thread,
		PaneID:      paneID,
		WorkspaceID: wsID,
		CreatedBy:   orStr(req.By, model.RouterName),
		CreatedAt:   time.Now(),
		Model:       modelName,
		Effort:      effort,
		Run:         &[2]string{modelName, effort},
		Status:      "starting",
		Session:     sessionID,
	}
	if err := e.store.UpsertWorker(ctx, wk); err != nil {
		return nil, err
	}
	sub := thread != model.MainThread
	if req.Subscribe != nil {
		sub = *req.Subscribe
	}
	if sub {
		_ = e.Subscribe(ctx, name, thread, false)
	}

	brief := fmt.Sprintf("[corgi task · worker=%s · thread=%s]\n%s\n\nWhen finished or blocked, report back with:\n  %s report \"summary or BLOCKED: question\"",
		name, thread, req.Task, e.binPath)
	_, _ = e.Ingest(ctx, "router", thread, model.RouterName, brief, "", name, name)
	e.addEvent(ctx, thread, fmt.Sprintf("派出 %s（%s%s）", name, kind, machineTag(machine)), name, "spawn")
	e.notifyChanged()
	return wk, nil
}

// SendToWorker queues a steering message for an active worker.
func (e *Engine) SendToWorker(ctx context.Context, workerName, thread, text string) (*model.Trigger, error) {
	wk, err := e.store.GetWorker(ctx, workerName)
	if err != nil || wk.Ended {
		return nil, fmt.Errorf("no running worker named %q", workerName)
	}
	if thread == "" {
		thread = wk.Thread
	}
	return e.Ingest(ctx, "router", thread, model.RouterName, text, "", wk.Name, wk.Name)
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
	if wk != nil {
		from = wk.Name
		thread = wk.Thread
		wk.LastReport = text
		wk.ReportAt = time.Now()
		_ = e.store.UpsertWorker(ctx, wk)
	}
	e.addEvent(ctx, thread, fmt.Sprintf("%s 汇报：%s", from, oneLineClip(text, 120)), from, "done")
	return e.Ingest(ctx, "worker", thread, from, fmt.Sprintf("report from %s:\n%s", from, text), "", model.RouterName, from)
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
	wk, err := e.store.GetWorker(ctx, workerName)
	if err != nil {
		return fmt.Errorf("worker %q not found", workerName)
	}
	if wk.WorkspaceID != "" {
		_ = e.fleet.CloseWorkspace(ctx, wk.WorkspaceID)
	}
	wk.Ended = true
	wk.Status = "done"
	if err := e.store.UpsertWorker(ctx, wk); err != nil {
		return err
	}
	_ = e.store.ExpireWorkerActions(ctx, wk.Name)
	if wk.Session != "" {
		eid, _, _ := e.store.NextID(ctx, "h")
		_ = e.store.SaveEndedSession(ctx, &model.EndedSession{
			ID: eid, Kind: wk.Kind, Machine: wk.Machine, Cwd: wk.Cwd,
			Name: wk.Name, Title: orStr(wk.Title, wk.Name), Session: wk.Session, EndedAt: time.Now(),
		}, 40)
	}
	e.addEvent(ctx, wk.Thread, "已停止 "+wk.Name+"（会话已保留，可随时续接）", wk.Name, "info")
	e.notifyChanged()
	return nil
}

// ResolveAction atomically resolves a pending Action from Web, WeCom, Router, or CLI.
func (e *Engine) ResolveAction(ctx context.Context, actionID, resolvedBy, choiceInput string) (*model.Action, error) {
	act, err := e.store.GetAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	chosenKey, chosenLabel, chosenCursor := matchActionChoice(act, choiceInput)
	resolved, err := e.store.ResolveAction(ctx, actionID, resolvedBy, chosenKey, chosenLabel)
	if err != nil {
		return nil, err
	}

	switch resolved.Kind {
	case model.ActionKindWorkerDialog:
		if err := e.sendDialogAnswer(ctx, resolved.PaneID, chosenKey, chosenCursor); err != nil {
			e.logger.Warn("failed sending dialog answer to worker", "worker", resolved.TargetWorker, "err", err)
		}
		e.mu.Lock()
		e.lastAnswer[resolved.PaneID] = time.Now()
		e.mu.Unlock()
		e.addEvent(ctx, resolved.Thread, fmt.Sprintf("已为 %s 选择：%s（via %s）", resolved.TargetWorker, chosenLabel, resolvedBy), resolved.TargetWorker, "info")
	case model.ActionKindRouterChoice, model.ActionKindRouterCheck:
		_, _, _ = e.Say(ctx, SayRequest{
			Thread:    resolved.Thread,
			Channel:   resolvedBy,
			From:      resolvedBy,
			Text:      chosenLabel,
			ReEntryID: resolved.EntryID,
		})
	}

	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnActionResolved(ctx, resolved)
	sink.OnStateChanged()
	return resolved, nil
}

// DismissAction atomically dismisses a pending reminder/choice without sending a reply to Router.
func (e *Engine) DismissAction(ctx context.Context, actionID string) (*model.Action, error) {
	act, err := e.store.DismissAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnActionResolved(ctx, act)
	sink.OnStateChanged()
	return act, nil
}

// AnswerWorker answers a blocked worker's live terminal dialog by choice key, label words, or "esc".
func (e *Engine) AnswerWorker(ctx context.Context, workerName, choiceInput, by string) (string, error) {
	wk, err := e.store.GetWorker(ctx, workerName)
	if err != nil || wk.Ended || wk.PaneID == "" {
		return "", fmt.Errorf("no running worker %q", workerName)
	}
	if pending, err := e.store.ListPendingActions(ctx); err == nil {
		for _, a := range pending {
			if a.Kind == model.ActionKindWorkerDialog && a.TargetWorker == workerName {
				res, err := e.ResolveAction(ctx, a.ID, by, choiceInput)
				if err != nil {
					return "", err
				}
				return res.ChosenLabel, nil
			}
		}
	}
	rr, err := e.fleet.Read(ctx, wk.PaneID, "detection", "text", 0)
	if err != nil {
		return "", err
	}
	ap := agent.ParseApproval(rr.Text)
	if len(ap.Choices) == 0 && !strings.EqualFold(strings.TrimSpace(choiceInput), "esc") {
		return "", fmt.Errorf("worker %s is not showing a dialog", workerName)
	}
	tmp := &model.Action{Choices: approvalToChoices(ap)}
	key, label, cursor := matchActionChoice(tmp, choiceInput)
	if err := e.sendDialogAnswer(ctx, wk.PaneID, key, cursor); err != nil {
		return "", err
	}
	e.mu.Lock()
	e.lastAnswer[wk.PaneID] = time.Now()
	e.mu.Unlock()
	e.addEvent(ctx, wk.Thread, fmt.Sprintf("替 %s 选了：%s", workerName, label), workerName, "info")
	return label, nil
}

func (e *Engine) sendDialogAnswer(ctx context.Context, paneID, key string, cursor int) error {
	if paneID == "" {
		return nil
	}
	if strings.EqualFold(key, "esc") {
		return e.fleet.SendKeys(ctx, paneID, "esc")
	}
	if cursor > 0 && key == "" {
		keys := make([]string, 0, cursor)
		for i := 1; i < cursor; i++ {
			keys = append(keys, "down")
		}
		keys = append(keys, "enter")
		return e.fleet.SendKeys(ctx, paneID, keys...)
	}
	return e.fleet.SendText(ctx, paneID, key, true)
}

func matchActionChoice(a *model.Action, raw string) (key, label string, cursor int) {
	q := strings.TrimSpace(raw)
	if strings.EqualFold(q, "esc") {
		return "esc", "取消 (Esc)", 0
	}
	low := strings.ToLower(q)
	for i, c := range a.Choices {
		if strings.EqualFold(c.Key, q) || strings.EqualFold(c.Label, q) {
			return c.Key, c.Label, i + 1
		}
	}
	for i, c := range a.Choices {
		if low != "" && strings.Contains(strings.ToLower(c.Label), low) {
			return c.Key, c.Label, i + 1
		}
	}
	return q, q, 0
}

func approvalToChoices(ap model.Approval) []model.ActionChoice {
	out := make([]model.ActionChoice, 0, len(ap.Choices))
	for _, c := range ap.Choices {
		key := c.Key
		if ap.Style == "cursor" {
			key = ""
		}
		out = append(out, model.ActionChoice{Key: key, Label: c.Label, Value: strconv.Itoa(c.Cursor)})
	}
	return out
}

// AddTimer creates and persists a new timer.

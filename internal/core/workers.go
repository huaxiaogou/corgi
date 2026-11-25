package core

import (
	"context"
	"errors"
	"fmt"
	"os"
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
		if !old.Ended {
			return nil, fmt.Errorf("worker %q is still running; use `send` instead", req.Resume)
		}
		if old.Session == "" {
			return nil, fmt.Errorf("worker %q has no saved session id to resume; spawn a fresh worker", req.Resume)
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
	mc, ok := e.fleet.Machine(machine)
	if !ok {
		return nil, fmt.Errorf("machine %q is not connected", orStr(req.Machine, machine))
	}
	machine = mc.ID

	// Pre-flight check that the target computer has the requested agent CLI installed.
	have := append([]string(nil), mc.Agents...)
	if mc.Local {
		for _, k := range e.agents.DetectInstalled(e.home, false) {
			if !slices.Contains(have, k) {
				have = append(have, k)
			}
		}
		if len(have) == 0 {
			if chkErr := e.agents.CheckInstalled(e.home, kind); chkErr != nil {
				return nil, chkErr
			}
		}
	}
	if len(have) > 0 && !slices.Contains(have, kind) {
		return nil, fmt.Errorf("%s has no %s installed (available: %s); pick another computer or agent kind",
			mc.Name, kind, strings.Join(have, ", "))
	}

	cwd := strings.TrimSpace(req.Cwd)
	if cwd == "" && (mc.ID == st.WorkerMachine || (mc.Local && st.WorkerMachine == "")) {
		cwd = st.WorkerCwd
	}
	targetHome := mc.Home
	if mc.Local && targetHome == "" {
		targetHome = e.home
	}
	cwd = expandInHome(targetHome, cwd)
	if mc.Local && cwd != "" {
		if fi, statErr := os.Stat(cwd); statErr != nil || !fi.IsDir() {
			return nil, fmt.Errorf("directory not found on %s: %s", mc.Name, cwd)
		}
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
	if existing, gErr := e.store.GetWorker(ctx, name); gErr == nil && !existing.Ended {
		return nil, fmt.Errorf("worker %s is already running", name)
	}

	modelName, effort := req.Model, req.Effort
	if kind == agent.NormalizeKind(st.WorkerKind) {
		if modelName == "" {
			modelName = st.WorkerModel
		}
		if effort == "" {
			effort = st.WorkerEffort
		}
	}

	wk := &model.Worker{
		Name:      name,
		Kind:      kind,
		Machine:   machine,
		Cwd:       cwd,
		Task:      req.Task,
		Thread:    thread,
		CreatedBy: orStr(req.By, model.RouterName),
		CreatedAt: time.Now(),
		Model:     modelName,
		Effort:    effort,
		Run:       &[2]string{modelName, effort},
		Status:    "starting",
		Session:   sessionID,
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
	verb := "派出"
	if sessionID != "" {
		verb = "续接"
	}
	e.addEvent(ctx, thread, fmt.Sprintf("%s %s（%s%s）", verb, name, kind, machineTag(mc.Name, mc.Local)), name, "spawn")
	e.notifyChanged()

	workerBin := e.binPath
	if !mc.Local || workerBin == "" {
		workerBin = "corgi"
	}
	// Note: Codex `resume <id>` is a subcommand and MUST precede launch flags.
	args := append(ad.ResumeArgs(sessionID), ad.LaunchArgs(modelName, effort, !st.AskPermissions)...)
	if kind == "claude" {
		args = append(args, "--allowedTools", fmt.Sprintf("Bash(%s report:*),Bash(corgi report:*)", workerBin))
	}

	started := make(chan error, 1)
	go func() {
		lctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		if !st.AskPermissions {
			e.preTrustWorkspace(lctx, machine, kind, cwd)
		}
		wsID, paneID, cErr := e.fleet.CreateWorkspace(lctx, machine, cwd, "corgi · "+name)
		if cErr != nil {
			e.failSpawnedWorker(lctx, wk, "", "", fmt.Errorf("create workspace: %w", cErr), started)
			return
		}
		sErr := e.fleet.StartAgent(lctx, paneID, kind, name, args)
		var he *herdr.Error
		blockedOnStart := sErr != nil && errors.As(sErr, &he) && he.Code == "agent_not_ready"
		if sErr != nil && !blockedOnStart {
			_ = e.fleet.CloseWorkspace(lctx, wsID)
			e.failSpawnedWorker(lctx, wk, paneID, wsID, fmt.Errorf("start agent %s: %w", name, sErr), started)
			return
		}

		e.fleet.RememberName(paneID, name)
		e.fleet.RememberRun(paneID, modelName, effort)

		wk.PaneID = paneID
		wk.WorkspaceID = wsID
		wk.Status = "idle"
		if blockedOnStart {
			wk.Status = "blocked"
		}
		_ = e.store.UpsertWorker(lctx, wk)
		e.notifyChanged()

		select {
		case started <- nil:
		default:
		}

		brief := fmt.Sprintf("[corgi task · worker=%s · thread=%s]\n%s\n\nHow this works:\n- Report completion with `%s report \"outcome summary & file paths\"`.\n- If blocked on a decision only the human can make or repeated failures, report `BLOCKED: <need> · tried: <attempts> · recommendation: <choice>`.",
			name, thread, strings.TrimSpace(req.Task), workerBin)
		_, _ = e.Ingest(lctx, "router", thread, model.RouterName, brief, "", name, name)
	}()

	select {
	case sErr := <-started:
		if sErr != nil {
			return nil, sErr
		}
		return wk, nil
	case <-time.After(8 * time.Second):
		return wk, nil
	case <-ctx.Done():
		return wk, nil
	}
}

// LaunchManagedRequest describes a user-initiated agent launch from the `+` modal or History resume.
type LaunchManagedRequest struct {
	Kind      string
	Machine   string
	Cwd       string
	Name      string
	Label     string
	Model     string
	Effort    string
	Prompt    string
	SessionID string
	ExtraArgs []string
}

// LaunchManaged starts an interactive agent on any connected machine and registers it immediately
// as a Corgi-managed Worker with full turn reporting, dialog card protection, and Router visibility.
func (e *Engine) LaunchManaged(ctx context.Context, req LaunchManagedRequest) (*model.Worker, error) {
	kind := agent.NormalizeKind(req.Kind)
	ad, err := e.agents.Get(kind)
	if err != nil {
		return nil, err
	}
	machine := e.fleet.ResolveMachine(req.Machine)
	mc, ok := e.fleet.Machine(machine)
	if !ok {
		return nil, fmt.Errorf("电脑不在线：%s", orStr(req.Machine, machine))
	}
	machine = mc.ID

	have := append([]string(nil), mc.Agents...)
	if mc.Local {
		for _, k := range e.agents.DetectInstalled(e.home, false) {
			if !slices.Contains(have, k) {
				have = append(have, k)
			}
		}
		if len(have) == 0 {
			if chkErr := e.agents.CheckInstalled(e.home, kind); chkErr != nil {
				return nil, chkErr
			}
		}
	}
	if len(have) > 0 && !slices.Contains(have, kind) {
		return nil, fmt.Errorf("%s 未安装 %s（可用：%s）", mc.Name, kind, strings.Join(have, ", "))
	}

	targetHome := mc.Home
	if mc.Local && targetHome == "" {
		targetHome = e.home
	}
	cwd := expandInHome(targetHome, orStr(strings.TrimSpace(req.Cwd), "~"))
	if mc.Local && cwd != "" {
		if fi, statErr := os.Stat(cwd); statErr != nil || !fi.IsDir() {
			return nil, fmt.Errorf("目录不存在: %s", cwd)
		}
	}

	explicitName := strings.TrimSpace(req.Name) != ""
	name := strings.TrimSpace(req.Name)
	if name == "" {
		_, seq, _ := e.store.NextID(ctx, "w")
		name = fmt.Sprintf("%s-%d", kind, seq)
	}
	if !workerNameRe.MatchString(name) || name == model.RouterName || name == model.DreamName {
		return nil, fmt.Errorf("名称需以小写字母开头，仅含字母/数字/-/_")
	}
	if existing, gErr := e.store.GetWorker(ctx, name); gErr == nil && !existing.Ended {
		if explicitName {
			return nil, fmt.Errorf("受管会话 %s 已在运行中", name)
		}
		_, seq, _ := e.store.NextID(ctx, "w")
		name = fmt.Sprintf("%s-%d", name, seq)
	}

	st := e.Settings()
	if !st.AskPermissions {
		e.preTrustWorkspace(ctx, machine, kind, cwd)
	}

	workerBin := e.binPath
	if !mc.Local || workerBin == "" {
		workerBin = "corgi"
	}
	// Codex `resume <id>` is a subcommand and must precede launch flags.
	args := append(ad.ResumeArgs(req.SessionID), ad.LaunchArgs(req.Model, req.Effort, !st.AskPermissions)...)
	if kind == "claude" {
		args = append(args, "--allowedTools", fmt.Sprintf("Bash(%s report:*),Bash(corgi report:*)", workerBin))
	}
	args = append(args, req.ExtraArgs...)

	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "corgi · " + name
	}
	wsID, paneID, err := e.fleet.CreateWorkspace(ctx, machine, cwd, label)
	if err != nil {
		return nil, err
	}
	sErr := e.fleet.StartAgent(ctx, paneID, kind, name, args)
	var he *herdr.Error
	blockedOnStart := sErr != nil && errors.As(sErr, &he) && he.Code == "agent_not_ready"
	if sErr != nil && !blockedOnStart {
		_ = e.fleet.CloseWorkspace(ctx, wsID)
		return nil, sErr
	}

	e.fleet.RememberName(paneID, name)
	e.fleet.RememberRun(paneID, req.Model, req.Effort)

	prompt := strings.TrimSpace(req.Prompt)
	taskDesc := prompt
	if taskDesc == "" {
		taskDesc = "项目会话 · " + cwd
	}
	status := "idle"
	if blockedOnStart {
		status = "blocked"
	} else if prompt != "" {
		status = "starting"
	}

	wk := &model.Worker{
		Name:        name,
		Kind:        kind,
		Machine:     machine,
		Cwd:         cwd,
		Task:        taskDesc,
		Thread:      model.MainThread,
		PaneID:      paneID,
		WorkspaceID: wsID,
		CreatedBy:   "you",
		CreatedAt:   time.Now(),
		Model:       req.Model,
		Effort:      req.Effort,
		Run:         &[2]string{req.Model, req.Effort},
		Status:      status,
		Session:     req.SessionID,
	}
	if err := e.store.UpsertWorker(ctx, wk); err != nil {
		return nil, err
	}

	verb := "新建受管任务"
	if req.SessionID != "" {
		verb = "续接受管会话"
	}
	e.addEvent(ctx, model.MainThread, fmt.Sprintf("%s %s（%s%s · %s）", verb, name, kind, machineTag(mc.Name, mc.Local), cwd), name, "spawn")
	e.notifyChanged()

	if prompt != "" {
		brief := fmt.Sprintf("[corgi task · worker=%s · thread=%s]\n%s\n\nHow this works:\n- Report completion with `%s report \"outcome summary & file paths\"`.\n- If blocked on a decision only the human can make or repeated failures, report `BLOCKED: <need> · tried: <attempts> · recommendation: <choice>`.",
			name, model.MainThread, prompt, workerBin)
		_, _ = e.Ingest(ctx, "router", model.MainThread, "you", brief, "", name, name)
	}
	return wk, nil
}

// CloseManagedWorkspace cleanly stops any active Worker owning workspaceID (preserving its session
// in History) and closes the underlying herdr workspace.
func (e *Engine) CloseManagedWorkspace(ctx context.Context, workspaceID string) error {
	if workers, err := e.store.ListWorkers(ctx); err == nil {
		for _, w := range workers {
			if !w.Ended && w.WorkspaceID == workspaceID {
				return e.StopWorker(ctx, w.Name)
			}
		}
	}
	if e.fleet != nil {
		return e.fleet.CloseWorkspace(ctx, workspaceID)
	}
	return nil
}

func (e *Engine) failSpawnedWorker(ctx context.Context, wk *model.Worker, paneID, wsID string, err error, started chan<- error) {
	wk.PaneID = paneID
	wk.WorkspaceID = wsID
	wk.Ended = true
	wk.Status = "failed"
	_ = e.store.UpsertWorker(ctx, wk)
	e.addEvent(ctx, wk.Thread, wk.Name+" 启动失败："+err.Error(), wk.Name, "error")
	e.notifyChanged()
	select {
	case started <- err:
	default:
		_, _ = e.Ingest(ctx, "worker", wk.Thread, wk.Name,
			fmt.Sprintf("worker %s failed to start: %v", wk.Name, err), "", model.RouterName, wk.Name)
	}
}

// SendToWorker queues a steering message for an active worker, or adopts a live
// manually-started agent matching workerName so the Router can delegate directly to it.
func (e *Engine) SendToWorker(ctx context.Context, workerName, thread, text string) (*model.Trigger, error) {
	wk, err := e.store.GetWorker(ctx, workerName)
	if err != nil || wk.Ended {
		adopted, adoptErr := e.adoptLiveAgent(ctx, workerName, thread, text)
		if adoptErr != nil {
			return nil, fmt.Errorf("no running worker or live agent named %q", workerName)
		}
		wk = adopted
	}
	if thread == "" {
		thread = wk.Thread
	}
	return e.Ingest(ctx, "router", thread, model.RouterName, text, "", wk.Name, wk.Name)
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
	if wk.Session == "" && wk.PaneID != "" {
		if sess := e.lookupRemoteSession(ctx, wk.PaneID); sess != "" {
			wk.Session = sess
		}
	}
	if wk.PaneID != "" && e.fleet != nil {
		// Press Esc first so any running turn (especially Codex's shared daemon) aborts cleanly before close.
		_ = e.fleet.SendKeys(ctx, wk.PaneID, "esc")
		time.Sleep(600 * time.Millisecond)
	}
	if wk.WorkspaceID != "" && e.fleet != nil {
		_ = e.fleet.CloseWorkspace(ctx, wk.WorkspaceID)
	}
	wk.Ended = true
	wk.Status = "done"
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
	if wk.PaneID != "" {
		_ = e.store.DeleteQueuedPromptsByScope(ctx, wk.PaneID)
		e.setQueuePaused(wk.PaneID, false)
	}
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
		if rr, err := e.fleet.Read(ctx, paneID, "detection", "text", 0); err == nil {
			ap := agent.ParseApproval(rr.Text)
			if cursor-1 < len(ap.Choices) {
				return e.fleet.SendKeys(ctx, paneID, agent.MoveKeys(ap, cursor-1)...)
			}
		}
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

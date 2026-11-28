package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

func (e *Engine) requireThreadWorkItem(ctx context.Context, id, thread string) (*model.WorkItem, string) {
	wi, err := e.store.GetWorkItem(ctx, id)
	if err != nil || wi == nil {
		return nil, "找不到工作项：" + id
	}
	if orStr(wi.Thread, model.MainThread) != thread {
		return nil, "工作项不属于当前会话线程"
	}
	return wi, ""
}

func (e *Engine) requireThreadWorker(ctx context.Context, name, thread string) (*model.Worker, string) {
	wk, err := e.store.GetWorker(ctx, name)
	if err != nil || wk == nil {
		return nil, "找不到 Worker：" + name
	}
	if orStr(wk.Thread, model.MainThread) != thread {
		return nil, "目标 Worker 不属于当前会话线程"
	}
	return wk, ""
}

func (e *Engine) toolDispatchWork(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, meta *turnConsentMeta, rawArgs string) string {
	var args struct {
		Mode         string `json:"mode"`
		Goal         string `json:"goal"`
		WorkID       string `json:"work_id"`
		Title        string `json:"title"`
		Project      string `json:"project"`
		Machine      string `json:"machine"`
		Kind         string `json:"kind"`
		Resume       string `json:"resume"`
		Constraint   string `json:"constraint"`
		Worktree     string `json:"worktree"`
		AllowRelease bool   `json:"allow_release"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolErr("dispatch_work 参数 JSON 无效：" + err.Error())
	}
	mode := strings.ToLower(strings.TrimSpace(args.Mode))
	if mode != "readonly" && mode != "prototype" && mode != "implement" {
		return toolErr("mode 必须是 readonly、prototype 或 implement")
	}
	goal := strings.TrimSpace(args.Goal)
	if goal == "" {
		return toolErr("goal 不能为空")
	}

	// Deterministic Strategy 1A boundary (before ResumeWorkerSession or Spawn).
	if args.AllowRelease && mode != "implement" {
		return toolErr(`allow_release 仅可与 mode="implement" 配合使用`)
	}
	wtMode := strings.TrimSpace(args.Worktree)
	if meta == nil || !meta.isHuman {
		if mode != "readonly" || args.AllowRelease || strings.TrimSpace(args.Resume) != "" {
			return toolErr("后台事件只能新派 readonly Worker，不能写盘、发布或续派已有 Worker")
		}
		wtMode = "off"
	}

	targetWorkID := strings.TrimSpace(args.WorkID)
	if targetWorkID == "" && meta != nil && meta.boundWorkItem != nil {
		targetWorkID = meta.boundWorkItem.ID
	}
	if targetWorkID != "" {
		wi, errMsg := e.requireThreadWorkItem(ctx, targetWorkID, turn.thread)
		if errMsg != "" {
			return toolErr(errMsg)
		}
		if wi.ClosedAt != nil {
			return toolErr("work_id 已关闭，如需重新开启请先提示用户或创建新工作项")
		}
	}

	rawConstraint := strings.TrimSpace(args.Constraint)
	if resumeName := strings.TrimSpace(args.Resume); resumeName != "" {
		if prevWk, err := e.store.GetWorker(ctx, resumeName); err == nil && prevWk != nil && orStr(prevWk.Thread, model.MainThread) != turn.thread {
			return toolErr("目标 Worker 不属于当前会话线程")
		}
		if err := e.recordAPIRouterInflight(context.Background(), trig, "resume_worker", resumeName); err != nil {
			return toolErr("记录防重标记失败，已中止续聊：" + err.Error())
		}
		wk, resumed, err := e.ResumeWorkerSession(ctx, ResumeWorkerRequest{Name: resumeName, WorkItemID: targetWorkID, Constraint: rawConstraint})
		if err != nil {
			return toolErr("续聊 Worker 失败：" + err.Error())
		}
		stagedGoal, _, stageWarns, sErr := e.stageTurnMediaForGoal(ctx, workerMachineID(wk), goal, "", trig, meta)
		if sErr != nil {
			return toolErr("附件跨机同步失败：" + sErr.Error())
		}
		effWorkID := wk.WorkItemID
		if _, boundWI, sendErr := e.SendToWorkerWithWork(ctx, wk.Name, turn.thread, stagedGoal, targetWorkID); sendErr != nil {
			return toolErr("续聊已打开但发送任务失败：" + sendErr.Error())
		} else if boundWI != "" {
			effWorkID = boundWI
		}
		e.updateWorkItemScopeOnDispatch(ctx, effWorkID, mode)
		turn.markMutated("已续聊 #" + wk.Name)
		turn.flags.Or(turnFlagDispatched)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		res := map[string]any{"ok": true, "resumed": resumed, "worker": wk.Name, "work_item_id": effWorkID, "machine": orStr(wk.Machine, "hub"), "kind": wk.Kind}
		if len(stageWarns) > 0 {
			res["staging_warnings"] = stageWarns
		}
		return toolJSON(res)
	}

	if wtMode == "" {
		if mode == "implement" && !args.AllowRelease {
			wtMode = "on"
		} else {
			wtMode = "off"
		}
	}

	st := e.Settings()
	var ov model.DispatchOverride
	if trig != nil && trig.Dispatch != nil {
		ov = *trig.Dispatch
	}
	reqMachine := orStr(ov.Machine, strings.TrimSpace(args.Machine))
	reqKind := orStr(ov.Kind, strings.TrimSpace(args.Kind))
	if ov.Kind == "" && (ov.Model != "" || ov.Effort != "") {
		reqKind = orStr(agent.NormalizeKind(st.WorkerKind), reqKind)
	}
	reqModel, reqEffort := ov.Model, ov.Effort
	effMachine := orStr(reqMachine, st.WorkerMachine)
	if effMachine != "" && e.fleet != nil {
		effMachine = e.fleet.ResolveMachine(effMachine)
	}
	if strings.EqualFold(effMachine, "hub") || strings.EqualFold(effMachine, "local") {
		effMachine = ""
	}
	origReq := ""
	if meta != nil {
		origReq = meta.verbatimRequest
	}
	var (
		resolvedCwd, resolvedProjID string
		proj                        *model.ProjectWorkspace
	)
	if projRef := orStr(ov.ProjectID, strings.TrimSpace(args.Project)); projRef != "" {
		proj = ResolveProject(st.Projects, projRef)
		if proj == nil {
			return toolErr("未找到已配置的项目：" + projRef)
		}
	} else {
		proj = ResolveProject(st.Projects, goal, origReq)
	}
	if proj != nil {
		resolvedProjID = proj.ID
		resolvedCwd = ProjectPathForMachine(proj, effMachine)
		if resolvedCwd == "" {
			return toolErr(fmt.Sprintf("项目 %s 未配置机器 %s 的路径，请更换机器或在设置中补充项目路径", proj.Name, orStr(effMachine, "hub")))
		}
	}
	if e.fleet == nil {
		return toolErr("本机或远端 Worker Fleet 暂未连接，无法启动 Worker")
	}

	goal, origReq, stageWarns, sErr := e.stageTurnMediaForGoal(ctx, effMachine, goal, origReq, trig, meta)
	if sErr != nil {
		return toolErr("附件跨机同步失败：" + sErr.Error())
	}

	_, seq, err := e.store.NextID(ctx, "w")
	if err != nil {
		return toolErr("分配 Worker 编号失败：" + err.Error())
	}
	reservedName := fmt.Sprintf("%s-%d", orStr(reqKind, orStr(st.WorkerKind, "claude")), seq)
	if err := e.recordAPIRouterInflight(context.Background(), trig, "dispatch_work", reservedName); err != nil {
		return toolErr("记录防重标记失败，已中止派发：" + err.Error())
	}
	wk, err := e.Spawn(ctx, SpawnRequest{
		Name: reservedName, Kind: reqKind, Machine: orStr(effMachine, "hub"), Model: reqModel, Effort: reqEffort, Cwd: resolvedCwd,
		Task: goal, Thread: turn.thread, By: model.RouterName, Worktree: wtMode,
		WorkID: targetWorkID, WorkTitle: strings.TrimSpace(args.Title), ProjectID: resolvedProjID,
		OriginalRequest: origReq, Constraint: buildModeConstraint(mode, args.AllowRelease, rawConstraint), Mode: mode,
	})
	if err != nil {
		return toolErr("派发 Worker 失败：" + err.Error())
	}
	e.updateWorkItemScopeOnDispatch(ctx, wk.WorkItemID, mode)
	turn.markMutated("已派发 #" + wk.Name)
	turn.flags.Or(turnFlagDispatched)
	e.ackAPITriggerOnce(context.Background(), turn, trig)
	res := map[string]any{
		"ok": true, "worker": wk.Name, "work_item_id": wk.WorkItemID,
		"machine": orStr(wk.Machine, "hub"), "kind": wk.Kind, "model": wk.Model, "branch": wk.WorktreeBranch, "mode": mode,
	}
	if len(stageWarns) > 0 {
		res["staging_warnings"] = stageWarns
	}
	return toolJSON(res)
}

func (e *Engine) updateWorkItemScopeOnDispatch(ctx context.Context, workID, mode string) {
	if e.store == nil || workID == "" || mode == "" {
		return
	}
	if wi, err := e.store.GetWorkItem(ctx, workID); err == nil && wi != nil && (wi.Scope != "implement" || mode == "implement") {
		_, _ = e.store.SetWorkItemScope(ctx, workID, mode)
	}
}

func (e *Engine) toolSteerWorker(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, meta *turnConsentMeta, rawArgs string) string {
	if meta == nil || !meta.isHuman {
		return toolErr("只有人类回合可以向已有 Worker 追加指令")
	}
	var args struct {
		Worker  string `json:"worker"`
		Message string `json:"message"`
		WorkID  string `json:"work_id"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolErr("steer_worker 参数 JSON 无效：" + err.Error())
	}
	name, msg := strings.TrimSpace(args.Worker), strings.TrimSpace(args.Message)
	if name == "" || msg == "" {
		return toolErr("worker 和 message 均不能为空")
	}
	wk, errMsg := e.requireThreadWorker(ctx, name, turn.thread)
	if errMsg != "" {
		return toolErr(errMsg)
	}
	if wk.Ended {
		return toolErr("Worker #" + name + " 已结束，请改用 dispatch_work(resume=" + name + ") 续聊")
	}
	workID := strings.TrimSpace(args.WorkID)
	if workID != "" {
		if _, errMsg := e.requireThreadWorkItem(ctx, workID, turn.thread); errMsg != "" {
			return toolErr(errMsg)
		}
	}
	stagedMsg, _, stageWarns, sErr := e.stageTurnMediaForGoal(ctx, workerMachineID(wk), msg, "", trig, meta)
	if sErr != nil {
		return toolErr("附件跨机同步失败：" + sErr.Error())
	}
	if err := e.recordAPIRouterInflight(context.Background(), trig, "steer_worker", name); err != nil {
		return toolErr("记录防重标记失败，已中止发送：" + err.Error())
	}
	if _, _, err := e.SendToWorkerWithWork(ctx, name, turn.thread, stagedMsg, workID); err != nil {
		return toolErr("发送指令给 #" + name + " 失败：" + err.Error())
	}
	turn.markMutated("已向 #" + name + " 发送跟进指令")
	turn.flags.Or(turnFlagDispatched)
	e.ackAPITriggerOnce(context.Background(), turn, trig)
	res := map[string]any{"ok": true, "worker": name}
	if len(stageWarns) > 0 {
		res["staging_warnings"] = stageWarns
	}
	return toolJSON(res)
}

func (e *Engine) toolManageWork(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, rawArgs string) string {
	var args struct {
		Action   string `json:"action"`
		Target   string `json:"target"`
		Title    string `json:"title"`
		NextStep string `json:"next_step"`
		Note     string `json:"note"`
		Keep     bool   `json:"keep"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolErr("manage_work 参数 JSON 无效：" + err.Error())
	}
	target := strings.TrimSpace(args.Target)
	switch strings.ToLower(strings.TrimSpace(args.Action)) {
	case "create_work":
		wi, err := e.SetWorkItem(ctx, "new", turn.thread, args.Title, args.NextStep, "", "")
		if err != nil {
			return toolErr(err.Error())
		}
		turn.markMutated("已创建工作项 " + wi.ID)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "work_item": wi})
	case "update_work":
		if _, errMsg := e.requireThreadWorkItem(ctx, target, turn.thread); errMsg != "" {
			return toolErr(errMsg)
		}
		wi, err := e.SetWorkItem(ctx, target, turn.thread, args.Title, args.NextStep, "", "")
		if err != nil {
			return toolErr(err.Error())
		}
		turn.markMutated("已更新工作项 " + wi.ID)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "work_item": wi})
	case "close_work":
		if _, errMsg := e.requireThreadWorkItem(ctx, target, turn.thread); errMsg != "" {
			return toolErr(errMsg)
		}
		wi, err := e.CloseWorkItem(ctx, target, args.Note)
		if err != nil {
			return toolErr(err.Error())
		}
		turn.markMutated("已关闭工作项 " + wi.ID)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "work_item": wi})
	case "stop_worker":
		if _, errMsg := e.requireThreadWorker(ctx, target, turn.thread); errMsg != "" {
			return toolErr(errMsg)
		}
		if err := e.StopWorker(ctx, target); err != nil {
			return toolErr(err.Error())
		}
		turn.markMutated("已停止 #" + target)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "worker": target, "stopped": true})
	case "keep_worker":
		if _, errMsg := e.requireThreadWorker(ctx, target, turn.thread); errMsg != "" {
			return toolErr(errMsg)
		}
		wk, err := e.SetWorkerKeep(ctx, target, args.Keep)
		if err != nil {
			return toolErr(err.Error())
		}
		turn.markMutated(fmt.Sprintf("已设置 #%s keep=%v", target, wk.Keep))
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "worker": wk.Name, "keep": wk.Keep})
	default:
		return toolErr("不支持的 manage_work action：" + args.Action)
	}
}

func (e *Engine) toolManageTimer(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, rawArgs string) string {
	var args struct {
		Action  string `json:"action"`
		Spec    string `json:"spec"`
		Text    string `json:"text"`
		TimerID string `json:"timer_id"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolErr("manage_timer 参数 JSON 无效：" + err.Error())
	}
	switch strings.ToLower(strings.TrimSpace(args.Action)) {
	case "list":
		all, err := e.store.ListTimers(ctx)
		if err != nil {
			return toolErr(err.Error())
		}
		var scoped []*model.Timer
		for _, tm := range all {
			if orStr(tm.Thread, model.MainThread) == turn.thread {
				scoped = append(scoped, tm)
			}
		}
		return toolJSON(map[string]any{"ok": true, "timers": scoped})
	case "add":
		if strings.TrimSpace(args.Spec) == "" || strings.TrimSpace(args.Text) == "" {
			return toolErr("add 定时器必须提供 spec 和 text")
		}
		tm, err := e.AddTimer(ctx, args.Spec, args.Text, turn.thread, model.RouterName, "corgi")
		if err != nil {
			return toolErr(err.Error())
		}
		turn.markMutated("已设提醒 " + tm.ID)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "timer": tm})
	case "remove":
		tid := strings.TrimSpace(args.TimerID)
		if tid == "" {
			return toolErr("remove 必须提供 timer_id")
		}
		all, _ := e.store.ListTimers(ctx)
		allowed := false
		for _, tm := range all {
			if tm.ID == tid && orStr(tm.Thread, model.MainThread) == turn.thread {
				allowed = true
				break
			}
		}
		if !allowed {
			return toolErr("找不到本线程下的定时器：" + tid)
		}
		if err := e.store.DeleteTimer(ctx, tid); err != nil {
			return toolErr(err.Error())
		}
		e.notifyChanged()
		turn.markMutated("已删除提醒 " + tid)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "deleted": tid})
	default:
		return toolErr("manage_timer action 必须是 list、add 或 remove")
	}
}

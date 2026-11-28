package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
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
		effWorkID := wk.WorkItemID
		if _, boundWI, sendErr := e.SendToWorkerWithWork(ctx, wk.Name, turn.thread, goal, targetWorkID); sendErr != nil {
			return toolErr("续聊已打开但发送任务失败：" + sendErr.Error())
		} else if boundWI != "" {
			effWorkID = boundWI
		}
		e.updateWorkItemScopeOnDispatch(ctx, effWorkID, mode)
		turn.markMutated("已续聊 #" + wk.Name)
		turn.flags.Or(turnFlagDispatched)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "resumed": resumed, "worker": wk.Name, "work_item_id": effWorkID, "machine": orStr(wk.Machine, "hub"), "kind": wk.Kind})
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
	reqMachine := orStr(strings.TrimSpace(args.Machine), ov.Machine)
	reqKind := orStr(strings.TrimSpace(args.Kind), ov.Kind)
	reqModel, reqEffort := "", ""
	if strings.TrimSpace(args.Kind) == "" || ov.Kind == "" || strings.EqualFold(strings.TrimSpace(args.Kind), ov.Kind) {
		reqModel, reqEffort = ov.Model, ov.Effort
	}
	effMachine := st.WorkerMachine
	if reqMachine != "" {
		effMachine = reqMachine
		if e.fleet != nil {
			effMachine = e.fleet.ResolveMachine(reqMachine)
		}
	}
	if strings.EqualFold(effMachine, "hub") || strings.EqualFold(effMachine, "local") {
		effMachine = ""
	}
	var resolvedCwd, resolvedProjID string
	if projRef := orStr(strings.TrimSpace(args.Project), ov.ProjectID); projRef != "" {
		proj := ResolveProject(st.Projects, projRef)
		if proj == nil {
			return toolErr("未找到已配置的项目：" + projRef)
		}
		resolvedProjID = proj.ID
		resolvedCwd = ProjectPathForMachine(proj, effMachine)
		if resolvedCwd == "" {
			return toolErr(fmt.Sprintf("项目 %s 未配置机器 %s 的路径，请更换机器或在设置中补充项目路径", proj.Name, orStr(effMachine, "hub")))
		}
	}
	if e.fleet == nil {
		return toolErr("本机或远端 Worker Fleet 暂未连接，无法启动 Worker")
	}

	_, seq, err := e.store.NextID(ctx, "w")
	if err != nil {
		return toolErr("分配 Worker 编号失败：" + err.Error())
	}
	reservedName := fmt.Sprintf("%s-%d", orStr(reqKind, orStr(st.WorkerKind, "claude")), seq)
	if err := e.recordAPIRouterInflight(context.Background(), trig, "dispatch_work", reservedName); err != nil {
		return toolErr("记录防重标记失败，已中止派发：" + err.Error())
	}
	origReq := ""
	if meta != nil {
		origReq = meta.verbatimRequest
	}
	wk, err := e.Spawn(ctx, SpawnRequest{
		Name: reservedName, Kind: reqKind, Machine: reqMachine, Model: reqModel, Effort: reqEffort, Cwd: resolvedCwd,
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
	return toolJSON(map[string]any{
		"ok": true, "worker": wk.Name, "work_item_id": wk.WorkItemID,
		"machine": orStr(wk.Machine, "hub"), "kind": wk.Kind, "model": wk.Model, "branch": wk.WorktreeBranch, "mode": mode,
	})
}

func buildModeConstraint(mode string, allowRelease bool, extra string) string {
	var prefix string
	switch mode {
	case "readonly":
		prefix = "【只读范围】仅允许阅读、分析、调研和输出报告，严禁修改仓库源码、严禁 git commit/merge/push。"
	case "prototype":
		prefix = "【原型范围】仅允许产出或更新原型/设计文档/HTML 草图，严禁修改产品源码或执行 git merge/push。"
	case "implement":
		if allowRelease {
			prefix = "【已获用户发布授权】按本次明确目标执行合入、构建、部署与核验，严格遵守任务说明中的范围与检查步骤。"
		} else {
			prefix = "【源码实现范围】仅在当前隔离分支完成本次改动与所请求的测试；严禁擅自 git merge 到主分支、git push 或部署上线。"
		}
	}
	if extra == "" {
		return prefix
	}
	return prefix + "\n" + extra
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
	if err := e.recordAPIRouterInflight(context.Background(), trig, "steer_worker", name); err != nil {
		return toolErr("记录防重标记失败，已中止发送：" + err.Error())
	}
	if _, _, err := e.SendToWorkerWithWork(ctx, name, turn.thread, msg, strings.TrimSpace(args.WorkID)); err != nil {
		return toolErr("发送指令给 #" + name + " 失败：" + err.Error())
	}
	turn.markMutated("已向 #" + name + " 发送跟进指令")
	turn.flags.Or(turnFlagDispatched)
	e.ackAPITriggerOnce(context.Background(), turn, trig)
	return toolJSON(map[string]any{"ok": true, "worker": name})
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

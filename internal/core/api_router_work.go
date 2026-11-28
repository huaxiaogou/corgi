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

// hasHumanDecisionCardApproval verifies that this human turn originated from an explicit
// preset choice selection on a same-thread decision card (Action.Choices) via a
// trusted human channel, never plain chat text, custom free-text, or router_check.
// When both act.WorkItemID and targetWorkID are set, they must match so a decision card
// for one work item cannot authorize release or scope escalation on another.
func hasHumanDecisionCardApproval(meta *turnConsentMeta, targetWorkID string) bool {
	if meta == nil || !meta.isHuman || meta.approvedAction == nil || meta.sourceEntry == nil {
		return false
	}
	act := meta.approvedAction
	if meta.sourceEntry.Role != "user" || strings.TrimSpace(meta.sourceEntry.ActionID) != act.ID {
		return false
	}
	if act.Status != model.ActionResolved || !isHumanChannel(act.ResolvedChannel) {
		return false
	}
	if act.Kind != model.ActionKindRouterChoice && act.Kind != model.ActionKindPatrolDigest {
		return false
	}
	if len(act.Choices) < 2 {
		return false
	}
	if actWI, tgtWI := strings.TrimSpace(act.WorkItemID), strings.TrimSpace(targetWorkID); actWI != "" && tgtWI != "" && actWI != tgtWI {
		return false
	}
	if len(act.ChosenKeys) > 0 {
		for _, k := range act.ChosenKeys {
			if _, _, _, ok := matchStrictActionChoice(act, k); !ok {
				return false
			}
		}
		return true
	}
	key := strings.TrimSpace(act.ChosenKey)
	if key == "" || strings.EqualFold(key, "reply") || strings.EqualFold(key, "custom") || strings.EqualFold(key, "ack") {
		return false
	}
	_, _, _, ok := matchStrictActionChoice(act, key)
	return ok
}

func (e *Engine) lookupResumeWorkerMeta(ctx context.Context, resumeName, thread string) (workItemID string, errMsg string) {
	if e.store == nil || resumeName == "" {
		return "", ""
	}
	if prevWk, err := e.store.GetWorker(ctx, resumeName); err == nil && prevWk != nil {
		if orStr(prevWk.Thread, model.MainThread) != thread {
			return "", "目标 Worker 不属于当前会话线程"
		}
		if wid := strings.TrimSpace(prevWk.WorkItemID); wid != "" {
			return wid, ""
		}
	}
	if ended, err := e.store.ListEndedSessions(ctx, 200); err == nil {
		for _, es := range ended {
			if es == nil || es.Name != resumeName {
				continue
			}
			if es.Thread != "" && orStr(es.Thread, model.MainThread) != thread {
				return "", "目标 Worker 不属于当前会话线程"
			}
			return strings.TrimSpace(es.WorkItemID), ""
		}
	}
	return "", ""
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
	wtMode := strings.ToLower(strings.TrimSpace(args.Worktree))
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
	var scopeWI *model.WorkItem
	if targetWorkID != "" {
		wi, errMsg := e.requireThreadWorkItem(ctx, targetWorkID, turn.thread)
		if errMsg != "" {
			return toolErr(errMsg)
		}
		if wi.ClosedAt != nil {
			return toolErr("work_id 已关闭，如需重新开启请先提示用户或创建新工作项")
		}
		scopeWI = wi
	}

	resumeName := strings.TrimSpace(args.Resume)
	if resumeName != "" {
		resumeWorkID, errMsg := e.lookupResumeWorkerMeta(ctx, resumeName, turn.thread)
		if errMsg != "" {
			return toolErr(errMsg)
		}
		if scopeWI == nil && resumeWorkID != "" {
			if wi, err := e.store.GetWorkItem(ctx, resumeWorkID); err == nil && wi != nil && wi.ClosedAt == nil && orStr(wi.Thread, model.MainThread) == turn.thread {
				scopeWI = wi
			}
		}
	}

	scopeWorkID := ""
	if scopeWI != nil {
		scopeWorkID = scopeWI.ID
	}
	if args.AllowRelease && !hasHumanDecisionCardApproval(meta, scopeWorkID) {
		return toolErr("allow_release=true 必须由用户在含 choices 的决策卡片上点选确认后才能开启，不能仅凭普通消息文本推断；请先调用 reply_user 下发发布确认卡，待用户点选后再执行。")
	}
	if mode == "implement" && isPrototypeOrReadonlyWorkItem(scopeWI) && !hasHumanDecisionCardApproval(meta, scopeWI.ID) {
		return toolErr(fmt.Sprintf(
			"工作项 %s 当前处于 %s 阶段，转入源码修改（mode=\"implement\"）必须由用户在含 choices 的决策卡片上点选拍板，不能仅凭普通消息文本推断；请先调用 reply_user 下发转实现确认卡，待用户点选后再执行。",
			scopeWI.ID, strings.TrimSpace(scopeWI.Scope),
		))
	}

	rawConstraint := strings.TrimSpace(args.Constraint)
	if resumeName != "" {
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
		e.updateWorkItemScopeOnDispatch(ctx, meta, effWorkID, mode)
		turn.markMutated("已续聊 #" + wk.Name)
		turn.flags.Or(turnFlagDispatched)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		res := map[string]any{"ok": true, "resumed": resumed, "worker": wk.Name, "work_item_id": effWorkID, "machine": orStr(wk.Machine, "hub"), "kind": wk.Kind}
		if len(stageWarns) > 0 {
			res["staging_warnings"] = stageWarns
		}
		return toolJSON(res)
	}

	if mode == "implement" {
		if !args.AllowRelease {
			// Without explicit card-approved release, implement MUST stay isolated in a git worktree.
			wtMode = "on"
		} else if wtMode == "" {
			wtMode = "off"
		}
	} else if wtMode == "" {
		wtMode = "off"
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
	e.updateWorkItemScopeOnDispatch(ctx, meta, wk.WorkItemID, mode)
	turn.markMutated("已派发 #" + wk.Name)
	turn.flags.Or(turnFlagDispatched)
	e.ackAPITriggerOnce(context.Background(), turn, trig)
	res := map[string]any{
		"ok": true, "worker": wk.Name, "work_item_id": wk.WorkItemID,
		"machine": orStr(wk.Machine, "hub"), "kind": wk.Kind, "model": wk.Model, "branch": wk.WorktreeBranch, "mode": mode,
	}
	if wk.ReadonlyEnforcement != "" {
		res["readonly_enforcement"] = wk.ReadonlyEnforcement
		res["readonly_label"] = agent.ReadonlyEnforcementLabel(agent.ReadonlyEnforcement(wk.ReadonlyEnforcement))
	}
	if len(stageWarns) > 0 {
		res["staging_warnings"] = stageWarns
	}
	return toolJSON(res)
}

func (e *Engine) updateWorkItemScopeOnDispatch(ctx context.Context, meta *turnConsentMeta, workID, mode string) {
	if e.store == nil || workID == "" || mode == "" {
		return
	}
	wi, err := e.store.GetWorkItem(ctx, workID)
	if err != nil || wi == nil {
		return
	}
	if mode == "implement" && isPrototypeOrReadonlyWorkItem(wi) && !hasHumanDecisionCardApproval(meta, wi.ID) {
		return
	}
	if wi.Scope != "implement" || mode == "implement" {
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

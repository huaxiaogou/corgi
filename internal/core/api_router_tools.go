package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

type turnConsentMeta struct {
	isHuman         bool
	sourceEntry     *model.Entry
	verbatimRequest string
	approvedAction  *model.Action
	boundWorkItem   *model.WorkItem
}

func (e *Engine) resolveTurnConsentMeta(ctx context.Context, trig *model.Trigger) *turnConsentMeta {
	meta := &turnConsentMeta{}
	if trig == nil {
		return meta
	}
	thread := orStr(strings.TrimSpace(trig.Thread), model.MainThread)
	meta.isHuman = (trig.Source == "chat" || trig.Source == "wecom") && trig.Policy == "respond" && !strings.EqualFold(strings.TrimSpace(trig.From), "machine")
	meta.verbatimRequest = strings.TrimSpace(trig.Text)

	if e.store == nil {
		return meta
	}
	if trig.Ref != "" {
		if en, err := e.store.GetEntry(ctx, trig.Ref); err == nil && en != nil {
			meta.sourceEntry = en
			if strings.EqualFold(strings.TrimSpace(en.Channel), "machine") {
				meta.isHuman = false
			}
			if strings.TrimSpace(en.Text) != "" {
				meta.verbatimRequest = strings.TrimSpace(en.Text)
			}
			if en.ActionID != "" {
				if act, aErr := e.store.GetAction(ctx, en.ActionID); aErr == nil && act != nil && orStr(act.Thread, model.MainThread) == thread {
					if isHumanResolvedAction(act) {
						meta.approvedAction = act
					}
				}
			}
		}
	}

	// Bind work item strictly from explicit structured references (no natural-language keyword guessing).
	boundID := strings.TrimSpace(trig.WorkItemID)
	if boundID == "" && meta.approvedAction != nil {
		boundID = strings.TrimSpace(meta.approvedAction.WorkItemID)
	}
	if boundID == "" && meta.sourceEntry != nil {
		boundID = strings.TrimSpace(meta.sourceEntry.WorkItemID)
		if boundID == "" && meta.sourceEntry.Re != nil {
			if meta.sourceEntry.Re.Entry != "" {
				if refEn, err := e.store.GetEntry(ctx, meta.sourceEntry.Re.Entry); err == nil && refEn != nil {
					boundID = strings.TrimSpace(refEn.WorkItemID)
					if boundID == "" && refEn.WorkerRef != nil {
						boundID = strings.TrimSpace(refEn.WorkerRef.WorkItemID)
					}
				}
			}
			if boundID == "" && meta.sourceEntry.Re.Worker != "" {
				if wk, err := e.store.GetWorker(ctx, meta.sourceEntry.Re.Worker); err == nil && wk != nil {
					boundID = strings.TrimSpace(wk.WorkItemID)
				}
			}
		}
	}

	if boundID != "" {
		if wi, err := e.store.GetWorkItem(ctx, boundID); err == nil && wi != nil && wi.ClosedAt == nil && orStr(wi.Thread, model.MainThread) == thread {
			meta.boundWorkItem = wi
		}
	}
	return meta
}

func isHumanResolvedAction(act *model.Action) bool {
	if act == nil || act.Status != model.ActionResolved {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(act.ResolvedChannel)) {
	case "web", "wecom", "cli":
	default:
		switch strings.ToLower(strings.TrimSpace(act.ResolvedBy)) {
		case "web", "wecom", "cli":
		default:
			return false
		}
	}
	if strings.EqualFold(strings.TrimSpace(act.ChosenKey), "reply") {
		return false
	}
	return strings.TrimSpace(act.ChosenLabel) != "" || strings.TrimSpace(act.ChosenKey) != ""
}

func isPrototypeOrReadonlyWorkItem(wi *model.WorkItem) bool {
	if wi == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(wi.Scope)) {
	case "prototype", "readonly":
		return true
	default:
		return false
	}
}

// apiRouterToolDefs returns the 8 deterministic, strictly ordered native Go tool definitions
// so the static system+tools prefix consistently hits prompt_cache_key.
func apiRouterToolDefs() []LLMToolDef {
	return []LLMToolDef{
		{
			Type: "function",
			Function: LLMToolFunctionSpec{
				Name:        "dispatch_work",
				Description: "启动新 Coding CLI Worker 或 resume 续聊已有 Worker。必须明确声明 mode（readonly / prototype / implement）。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"mode":       map[string]any{"type": "string", "enum": []string{"readonly", "prototype", "implement"}, "description": "任务模式：readonly 只读调研/分析，prototype 原型/方案草稿，implement 修改源码/跑测试"},
						"goal":       map[string]any{"type": "string", "description": "清晰、自包含的任务说明与验收标准"},
						"work_id":    map[string]any{"type": "string", "description": "关联的开放 work_item ID（留空则按上下文自动绑定或新建）"},
						"title":      map[string]any{"type": "string", "description": "新建 work_item 时的简洁标题（≤20字）"},
						"project":    map[string]any{"type": "string", "description": "已配置的项目 ID 或名称"},
						"machine":    map[string]any{"type": "string", "description": "运行机器 ID（留空用全局默认）"},
						"kind":       map[string]any{"type": "string", "enum": []string{"claude", "codex", "agy", "codebuddy"}, "description": "指定 CLI 工具（留空用全局默认）"},
						"resume":        map[string]any{"type": "string", "description": "续聊已有/已结束 Worker 的名称（可选）"},
						"constraint":    map[string]any{"type": "string", "description": "给 Worker 的额外硬约束（可选）"},
						"worktree":      map[string]any{"type": "string", "enum": []string{"auto", "on", "off"}, "description": "worktree 隔离策略（普通 implement 默认 on，allow_release 默认 off）"},
						"allow_release": map[string]any{"type": "boolean", "description": "仅当用户已明确批准合并主分支、推送远端、发版或部署上线时设为 true（仅人类直接指令回合可用）"},
					},
					"required": []string{"mode", "goal"},
				},
			},
		},
		{
			Type: "function",
			Function: LLMToolFunctionSpec{
				Name:        "manage_timer",
				Description: "列出、创建或删除本线程的定时提醒/巡检任务。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action":   map[string]any{"type": "string", "enum": []string{"list", "add", "remove"}},
						"spec":     map[string]any{"type": "string", "description": "add 时必填，如 'in 30m', 'daily 09:00', 'every 2h', 'at 2026-11-28 10:00'"},
						"text":     map[string]any{"type": "string", "description": "add 时必填，到点提醒内容"},
						"target":   map[string]any{"type": "string", "description": "可选目标（默认大脑本身）"},
						"timer_id": map[string]any{"type": "string", "description": "remove 时必填的 timer ID"},
					},
					"required": []string{"action"},
				},
			},
		},
		{
			Type: "function",
			Function: LLMToolFunctionSpec{
				Name:        "manage_work",
				Description: "管理工作项（新建/更新/关闭）或控制活跃 Worker（停止/保活）。不含代答终端审批弹窗。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action":    map[string]any{"type": "string", "enum": []string{"create_work", "update_work", "close_work", "stop_worker", "keep_worker"}},
						"target":    map[string]any{"type": "string", "description": "work_item ID 或 worker 名称（create_work 时可留空）"},
						"title":     map[string]any{"type": "string", "description": "工作项标题"},
						"next_step": map[string]any{"type": "string", "description": "下一步说明"},
						"note":      map[string]any{"type": "string", "description": "close_work 必填的关闭说明"},
						"keep":      map[string]any{"type": "boolean", "description": "keep_worker 时是否保活"},
					},
					"required": []string{"action"},
				},
			},
		},
		{
			Type: "function",
			Function: LLMToolFunctionSpec{
				Name:        "read_detail",
				Description: "按需分页读取 Worker 详情、截断 Trigger、WorkItem、Action、记忆笔记，或受限只读检查已配置项目文件与 Git 状态。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"kind":   map[string]any{"type": "string", "enum": []string{"worker", "trigger", "work_item", "action", "note", "project_file", "project_git"}},
						"target": map[string]any{"type": "string", "description": "对应 ID / 名称；project_file 格式为 '<project_id>:<rel_path>'；project_git 格式为 '<project_id>' 或 '<project_id>:status|log|diff'"},
						"offset": map[string]any{"type": "integer", "description": "字符分页偏移量（默认 0）"},
						"limit":  map[string]any{"type": "integer", "description": "返回最大字符数（默认 4000，上限 7000）"},
					},
					"required": []string{"kind", "target"},
				},
			},
		},
		{
			Type: "function",
			Function: LLMToolFunctionSpec{
				Name:        "reply_user",
				Description: "向用户发送最终回复、成果交付或决策卡片。作为最终回复成功发送后将直接结束本轮。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"purpose": map[string]any{"type": "string", "enum": []string{"answer", "result", "attention"}, "description": "answer=普通问答（禁按钮），result=交付成果/附件，attention=需人确认的决策卡或提醒"},
						"text":    map[string]any{"type": "string", "description": "回复正文（2项以上用 '- ' 列表分条）"},
						"choices": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "仅 purpose=attention 时可用，2–4 个清晰选项"},
						"multi":   map[string]any{"type": "boolean", "description": "attention 卡片是否多选"},
						"check":   map[string]any{"type": "boolean", "description": "仅 purpose=attention 且无需多选项时的轻量已读提醒"},
						"files":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "显式附带的绝对文件路径"},
						"machine": map[string]any{"type": "string", "description": "files 所在机器 ID（Hub 本机留空）"},
						"work_id": map[string]any{"type": "string", "description": "关联的 work_item ID"},
						"re_id":   map[string]any{"type": "string", "description": "引用的 entry ID 或 trigger ID"},
						"worker":  map[string]any{"type": "string", "description": "关联的 Worker 名称（配合 re_id 可自动附带完整报告快照）"},
					},
					"required": []string{"purpose", "text"},
				},
			},
		},
		{
			Type: "function",
			Function: LLMToolFunctionSpec{
				Name:        "save_memory",
				Description: "更新持久记忆文件（仅允许 instructions.md 或 notes/<topic>.md）。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file":    map[string]any{"type": "string", "description": "相对 memory/ 的路径，如 'instructions.md' 或 'notes/project-foo.md'"},
						"content": map[string]any{"type": "string", "description": "完整新内容（≤32KB）"},
					},
					"required": []string{"file", "content"},
				},
			},
		},
		{
			Type: "function",
			Function: LLMToolFunctionSpec{
				Name:        "steer_worker",
				Description: "向运行中的 Worker 追加指令或纠偏。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"worker":  map[string]any{"type": "string", "description": "目标 Worker 名称"},
						"message": map[string]any{"type": "string", "description": "发给 Worker 的指令内容"},
						"work_id": map[string]any{"type": "string", "description": "可选工作项 ID"},
					},
					"required": []string{"worker", "message"},
				},
			},
		},
		{
			Type: "function",
			Function: LLMToolFunctionSpec{
				Name:        "verify_worker",
				Description: "对指定 Worker 的 worktree 执行仓库自动检测的构建/测试验证，可选独立代码审查。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"worker":       map[string]any{"type": "string", "description": "目标 Worker 名称"},
						"review":       map[string]any{"type": "boolean", "description": "是否启用独立审查"},
						"reviewer":     map[string]any{"type": "string", "enum": []string{"codex", "claude"}, "description": "独立审查器"},
						"send_on_fail": map[string]any{"type": "boolean", "description": "失败时是否自动发回 Worker 修复（需具备 implement 授权）"},
					},
					"required": []string{"worker"},
				},
			},
		},
	}
}

func toolJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"ok":false,"error":"marshal tool result failed"}`
	}
	return string(b)
}

func toolErr(msg string) string {
	return toolJSON(map[string]any{"ok": false, "error": msg})
}

func (e *Engine) execAPIRouterTool(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, meta *turnConsentMeta, tc LLMToolCall) (string, bool) {
	switch tc.Function.Name {
	case "reply_user":
		return e.toolReplyUser(ctx, turn, trig, tc.Function.Arguments)
	case "dispatch_work":
		return e.toolDispatchWork(ctx, turn, trig, meta, tc.Function.Arguments), false
	case "steer_worker":
		return e.toolSteerWorker(ctx, turn, trig, meta, tc.Function.Arguments), false
	case "verify_worker":
		return e.toolVerifyWorker(ctx, turn, trig, meta, tc.Function.Arguments), false
	case "manage_work":
		return e.toolManageWork(ctx, turn, trig, tc.Function.Arguments), false
	case "manage_timer":
		return e.toolManageTimer(ctx, turn, trig, tc.Function.Arguments), false
	case "read_detail":
		return e.toolReadDetail(ctx, turn, tc.Function.Arguments), false
	case "save_memory":
		return e.toolSaveMemory(ctx, turn, tc.Function.Arguments), false
	default:
		return toolErr("未知工具：" + tc.Function.Name), false
	}
}

func (e *Engine) toolReplyUser(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, rawArgs string) (string, bool) {
	var args struct {
		Purpose string   `json:"purpose"`
		Text    string   `json:"text"`
		Choices []string `json:"choices"`
		Multi   bool     `json:"multi"`
		Check   bool     `json:"check"`
		Files   []string `json:"files"`
		Machine string   `json:"machine"`
		WorkID  string   `json:"work_id"`
		ReID    string   `json:"re_id"`
		Worker  string   `json:"worker"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolErr("reply_user 参数 JSON 无效：" + err.Error()), false
	}
	purpose := strings.ToLower(strings.TrimSpace(args.Purpose))
	text := strings.TrimSpace(args.Text)
	if text == "" {
		return toolErr("reply_user 的 text 不能为空"), false
	}

	switch purpose {
	case "answer":
		if len(args.Choices) > 0 || args.Check {
			return toolErr("purpose=\"answer\" 用于普通问答，禁止携带 choices 或 check；如需用户拍板请改用 purpose=\"attention\""), false
		}
	case "attention":
		if len(args.Choices) == 0 && !args.Check {
			return toolErr("purpose=\"attention\" 必须提供 2–4 个 choices 选项，或显式设置 check=true"), false
		}
		if len(args.Choices) == 1 {
			return toolErr("决策卡片 choices 至少需要 2 个选项，或改用 check=true / purpose=\"answer\""), false
		}
	case "result":
		if len(args.Choices) > 0 {
			return toolErr("purpose=\"result\" 用于成果交付，如需同时拍板请改用 purpose=\"attention\""), false
		}
		// Reject claiming an attachment is included when neither explicit files nor auto-attached worker report exists.
		if len(args.Files) == 0 && claimsAttachmentIncluded(text) && !(strings.TrimSpace(args.Worker) != "" && strings.TrimSpace(args.ReID) != "") {
			return toolErr("正文声称已附文件/附件但未传 files 参数；请通过 files（必要时带 machine）显式附文件，或传 re_id+worker 自动附带报告快照，或去掉声称附件的措辞"), false
		}
		if len(args.Files) == 0 {
			if p := mentionsDeliverablePath(text); p != "" && strings.TrimSpace(args.Worker) == "" {
				return toolErr(fmt.Sprintf("交付正文包含本地产物路径 %s 但未通过 files 上传，请将其放入 files 参数并删去裸本地路径", p)), false
			}
		}
	default:
		return toolErr("purpose 必须是 answer、result 或 attention"), false
	}

	reID := strings.TrimSpace(args.ReID)
	if reID == "" {
		reID = trig.Ref
	}
	if reID != "" {
		if prev, err := e.store.GetEntry(ctx, reID); err == nil && prev != nil && orStr(prev.Thread, model.MainThread) != turn.thread {
			return toolErr("re_id 不属于当前会话线程，已拒绝跨线程引用"), false
		} else if prevTr, err := e.store.GetTrigger(ctx, reID); err == nil && prevTr != nil && orStr(prevTr.Thread, model.MainThread) != turn.thread {
			return toolErr("re_id 对应的 trigger 不属于当前会话线程，已拒绝跨线程引用"), false
		}
	}

	workID := strings.TrimSpace(args.WorkID)
	if workID != "" {
		wi, err := e.store.GetWorkItem(ctx, workID)
		if err != nil {
			return toolErr("work_id 不存在：" + workID), false
		}
		if orStr(wi.Thread, model.MainThread) != turn.thread {
			return toolErr("work_id 不属于当前会话线程"), false
		}
	}

	mode := ""
	if args.Multi && len(args.Choices) > 0 {
		mode = model.ActionModeMulti
	}
	e.recordAPIRouterInflight(context.Background(), trig, "reply_user", "")
	res, err := e.ReplyDetailed(ctx, ReplyRequest{
		Thread:  turn.thread,
		Text:    text,
		ReID:    reID,
		Options: args.Choices,
		Mode:    mode,
		Files:   args.Files,
		Machine: strings.TrimSpace(args.Machine),
		WorkID:  workID,
		Check:   args.Check,
		Worker:  strings.TrimSpace(args.Worker),
	})
	if err != nil {
		return toolErr("发送回复失败：" + err.Error()), false
	}

	e.recordAPIRouterInflight(context.Background(), trig, "reply_user", res.Entry.ID)
	turn.markMutated("已回复 " + res.Entry.ID)
	turn.markReplied()
	e.ackAPITriggerOnce(context.Background(), turn, trig)

	out := map[string]any{
		"ok":    true,
		"entry": res.Entry.ID,
	}
	if res.Action != nil {
		out["action"] = res.Action.ID
	}
	if res.Warning != "" {
		out["warning"] = res.Warning
	}
	return toolJSON(out), true
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
	switch mode {
	case "readonly", "prototype", "implement":
	default:
		return toolErr("mode 必须是 readonly、prototype 或 implement")
	}
	goal := strings.TrimSpace(args.Goal)
	if goal == "" {
		return toolErr("goal 不能为空")
	}

	// Deterministic structural boundary (zero natural-language regex):
	// non-human triggers (worker reports / timers / system / candidate) may never autonomously edit code or release.
	if args.AllowRelease {
		if meta == nil || !meta.isHuman {
			return toolErr("非人类直接指令禁止开启 allow_release 执行合并/推送/部署操作；请先用 reply_user(purpose=\"attention\") 发确认卡请用户拍板。")
		}
		if mode != "implement" {
			return toolErr("allow_release 仅可与 mode=\"implement\" 配合使用")
		}
	}
	if mode == "implement" && (meta == nil || !meta.isHuman) {
		return toolErr("非人类直接指令（Worker 汇报 / 定时器 / 系统 / candidate 事件）禁止自动启动 implement 写代码；请改用 reply_user(purpose=\"attention\") 发确认卡请用户决定。")
	}

	targetWorkID := strings.TrimSpace(args.WorkID)
	if targetWorkID == "" && meta != nil && meta.boundWorkItem != nil {
		targetWorkID = meta.boundWorkItem.ID
	}
	if targetWorkID != "" {
		wi, err := e.store.GetWorkItem(ctx, targetWorkID)
		if err != nil {
			return toolErr("work_id 不存在：" + targetWorkID)
		}
		if orStr(wi.Thread, model.MainThread) != turn.thread {
			return toolErr("work_id 不属于当前会话线程")
		}
		if wi.ClosedAt != nil {
			return toolErr("work_id 已关闭，如需重新开启请先提示用户或创建新工作项")
		}
	}

	// Build scope constraint for newly spawned workers.
	rawConstraint := strings.TrimSpace(args.Constraint)
	constraint := rawConstraint
	switch mode {
	case "readonly":
		prefix := "【只读范围】仅允许阅读、分析、调研和输出报告，严禁修改仓库源码、严禁 git commit/merge/push。"
		if constraint == "" {
			constraint = prefix
		} else {
			constraint = prefix + "\n" + constraint
		}
	case "prototype":
		prefix := "【原型范围】仅允许产出或更新原型/设计文档/HTML 草图，严禁修改产品源码或执行 git merge/push。"
		if constraint == "" {
			constraint = prefix
		} else {
			constraint = prefix + "\n" + constraint
		}
	case "implement":
		prefix := "【源码实现范围】仅在当前隔离分支完成本次改动与所请求的测试；严禁擅自 git merge 到主分支、git push 或部署上线。"
		if args.AllowRelease {
			prefix = "【已获用户发布授权】按本次明确目标执行合入、构建、部署与核验，严格遵守任务说明中的范围与检查步骤。"
		}
		if constraint == "" {
			constraint = prefix
		} else {
			constraint = prefix + "\n" + constraint
		}
	}

	// Handle resume of an existing/ended worker.
	if resumeName := strings.TrimSpace(args.Resume); resumeName != "" {
		prevWk, gErr := e.store.GetWorker(ctx, resumeName)
		if gErr == nil && prevWk != nil && orStr(prevWk.Thread, model.MainThread) != turn.thread {
			return toolErr("目标 Worker 不属于当前会话线程")
		}
		e.recordAPIRouterInflight(context.Background(), trig, "resume_worker", resumeName)
		wk, resumed, err := e.ResumeWorkerSession(ctx, ResumeWorkerRequest{
			Name:       resumeName,
			WorkItemID: targetWorkID,
			Constraint: rawConstraint,
		})
		if err != nil {
			return toolErr("续聊 Worker 失败：" + err.Error())
		}
		effWorkID := wk.WorkItemID
		if goal != "" {
			_, boundWI, sendErr := e.sendToWorkerInternal(ctx, wk.Name, turn.thread, goal, targetWorkID, "", mode == "implement", false)
			if sendErr != nil {
				return toolErr("续聊已打开但发送任务失败：" + sendErr.Error())
			}
			if boundWI != "" {
				effWorkID = boundWI
			}
		} else if mode == "implement" {
			e.markWorkerImplementDispatched(wk.Name)
		}
		if effWorkID != "" {
			e.updateWorkItemScopeOnDispatch(ctx, effWorkID, mode)
		}
		turn.markMutated("已续聊 #" + wk.Name)
		turn.markDispatched()
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{
			"ok":           true,
			"resumed":      resumed,
			"worker":       wk.Name,
			"work_item_id": effWorkID,
			"machine":      orStr(wk.Machine, "hub"),
			"kind":         wk.Kind,
		})
	}

	wtMode := strings.TrimSpace(args.Worktree)
	if mode == "implement" && wtMode == "" {
		if args.AllowRelease {
			wtMode = "off"
		} else {
			wtMode = "on"
		}
	}
	origReq := ""
	if meta != nil {
		origReq = meta.verbatimRequest
	}

	if e.fleet == nil {
		return toolErr("本机或远端 Worker Fleet 暂未连接，无法启动 Worker")
	}

	// Pre-reserve worker name for crash-recovery inspection.
	_, seq, _ := e.store.NextID(ctx, "w")
	st := e.Settings()
	effKind := orStr(strings.TrimSpace(args.Kind), orStr(st.WorkerKind, "claude"))
	reservedName := fmt.Sprintf("%s-%d", effKind, seq)
	e.recordAPIRouterInflight(context.Background(), trig, "dispatch_work", reservedName)

	wk, err := e.Spawn(ctx, SpawnRequest{
		Name:            reservedName,
		Kind:            strings.TrimSpace(args.Kind),
		Machine:         strings.TrimSpace(args.Machine),
		Task:            goal,
		Thread:          turn.thread,
		By:              model.RouterName,
		Worktree:        wtMode,
		WorkID:          targetWorkID,
		WorkTitle:       strings.TrimSpace(args.Title),
		ProjectID:       strings.TrimSpace(args.Project),
		OriginalRequest: origReq,
		Constraint:      constraint,
		Mode:            mode,
	})
	if err != nil {
		return toolErr("派发 Worker 失败：" + err.Error())
	}
	if wk.WorkItemID != "" {
		e.updateWorkItemScopeOnDispatch(ctx, wk.WorkItemID, mode)
	}
	turn.markMutated("已派发 #" + wk.Name)
	turn.markDispatched()
	e.ackAPITriggerOnce(context.Background(), turn, trig)
	return toolJSON(map[string]any{
		"ok":           true,
		"worker":       wk.Name,
		"work_item_id": wk.WorkItemID,
		"machine":      orStr(wk.Machine, "hub"),
		"kind":         wk.Kind,
		"model":        wk.Model,
		"branch":       wk.WorktreeBranch,
		"mode":         mode,
	})
}

func (e *Engine) updateWorkItemScopeOnDispatch(ctx context.Context, workID, mode string) {
	if e.store == nil || workID == "" || mode == "" {
		return
	}
	wi, err := e.store.GetWorkItem(ctx, workID)
	if err != nil || wi == nil {
		return
	}
	// Never downgrade an already authorized "implement" work item back to readonly.
	if wi.Scope == "implement" && mode != "implement" {
		return
	}
	_, _ = e.store.SetWorkItemScope(ctx, workID, mode)
}

func (e *Engine) toolSteerWorker(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, meta *turnConsentMeta, rawArgs string) string {
	var args struct {
		Worker  string `json:"worker"`
		Message string `json:"message"`
		WorkID  string `json:"work_id"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolErr("steer_worker 参数 JSON 无效：" + err.Error())
	}
	name := strings.TrimSpace(args.Worker)
	msg := strings.TrimSpace(args.Message)
	if name == "" || msg == "" {
		return toolErr("worker 和 message 均不能为空")
	}
	wk, err := e.store.GetWorker(ctx, name)
	if err != nil || wk == nil {
		return toolErr("找不到 Worker：" + name)
	}
	if orStr(wk.Thread, model.MainThread) != turn.thread {
		return toolErr("目标 Worker 不属于当前会话线程")
	}
	if wk.Ended {
		return toolErr("Worker #" + name + " 已结束，请改用 dispatch_work(resume=" + name + ") 续聊")
	}

	e.recordAPIRouterInflight(context.Background(), trig, "steer_worker", name)
	if _, _, err := e.SendToWorkerWithWork(ctx, name, turn.thread, msg, strings.TrimSpace(args.WorkID)); err != nil {
		return toolErr("发送指令给 #" + name + " 失败：" + err.Error())
	}
	turn.markMutated("已向 #" + name + " 发送跟进指令")
	turn.markDispatched()
	e.ackAPITriggerOnce(context.Background(), turn, trig)
	return toolJSON(map[string]any{"ok": true, "worker": name})
}

func (e *Engine) toolVerifyWorker(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, meta *turnConsentMeta, rawArgs string) string {
	var args struct {
		Worker     string `json:"worker"`
		Review     bool   `json:"review"`
		Reviewer   string `json:"reviewer"`
		SendOnFail bool   `json:"send_on_fail"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolErr("verify_worker 参数 JSON 无效：" + err.Error())
	}
	name := strings.TrimSpace(args.Worker)
	if name == "" {
		return toolErr("worker 不能为空")
	}
	wk, err := e.store.GetWorker(ctx, name)
	if err != nil || wk == nil {
		return toolErr("找不到 Worker：" + name)
	}
	if orStr(wk.Thread, model.MainThread) != turn.thread {
		return toolErr("目标 Worker 不属于当前会话线程")
	}
	if args.SendOnFail {
		if meta == nil || !meta.isHuman {
			return toolErr("非人类直接指令禁止开启 send_on_fail 自动派发修复；请先不带 send_on_fail 执行验收，再视结果汇报或发确认卡。")
		}
		e.recordAPIRouterInflight(context.Background(), trig, "verify_worker", name)
	}

	res, err := e.VerifyWorker(ctx, VerifyWorkerRequest{
		Worker:   name,
		Review:   args.Review,
		Reviewer: strings.TrimSpace(args.Reviewer),
		Send:     args.SendOnFail,
	})
	if err != nil {
		return toolErr("验证失败：" + err.Error())
	}
	if args.SendOnFail && res.SentToWorker {
		turn.markMutated("已将验证失败结果回弹给 #" + name)
		turn.markDispatched()
		e.ackAPITriggerOnce(context.Background(), turn, trig)
	}
	return toolJSON(map[string]any{"ok": true, "result": res})
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
		e.recordAPIRouterInflight(context.Background(), trig, "create_work", "")
		wi, err := e.SetWorkItem(ctx, "new", turn.thread, args.Title, args.NextStep, "", "")
		if err != nil {
			return toolErr(err.Error())
		}
		turn.markMutated("已创建工作项 " + wi.ID)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "work_item": wi})
	case "update_work":
		wi, err := e.store.GetWorkItem(ctx, target)
		if err != nil || wi == nil {
			return toolErr("找不到工作项：" + target)
		}
		if orStr(wi.Thread, model.MainThread) != turn.thread {
			return toolErr("工作项不属于当前线程")
		}
		updated, err := e.SetWorkItem(ctx, target, turn.thread, args.Title, args.NextStep, "", "")
		if err != nil {
			return toolErr(err.Error())
		}
		turn.markMutated("已更新工作项 " + updated.ID)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "work_item": updated})
	case "close_work":
		wi, err := e.store.GetWorkItem(ctx, target)
		if err != nil || wi == nil {
			return toolErr("找不到工作项：" + target)
		}
		if orStr(wi.Thread, model.MainThread) != turn.thread {
			return toolErr("工作项不属于当前线程")
		}
		closed, err := e.CloseWorkItem(ctx, target, args.Note)
		if err != nil {
			return toolErr(err.Error())
		}
		turn.markMutated("已关闭工作项 " + closed.ID)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "work_item": closed})
	case "stop_worker":
		wk, err := e.store.GetWorker(ctx, target)
		if err != nil || wk == nil {
			return toolErr("找不到 Worker：" + target)
		}
		if orStr(wk.Thread, model.MainThread) != turn.thread {
			return toolErr("Worker 不属于当前线程")
		}
		if err := e.StopWorker(ctx, target); err != nil {
			return toolErr(err.Error())
		}
		turn.markMutated("已停止 #" + target)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "worker": target, "stopped": true})
	case "keep_worker":
		wk, err := e.store.GetWorker(ctx, target)
		if err != nil || wk == nil {
			return toolErr("找不到 Worker：" + target)
		}
		if orStr(wk.Thread, model.MainThread) != turn.thread {
			return toolErr("Worker 不属于当前线程")
		}
		updated, err := e.SetWorkerKeep(ctx, target, args.Keep)
		if err != nil {
			return toolErr(err.Error())
		}
		turn.markMutated(fmt.Sprintf("已设置 #%s keep=%v", target, updated.Keep))
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "worker": updated.Name, "keep": updated.Keep})
	default:
		return toolErr("不支持的 manage_work action：" + args.Action)
	}
}

func (e *Engine) toolManageTimer(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, rawArgs string) string {
	var args struct {
		Action  string `json:"action"`
		Spec    string `json:"spec"`
		Text    string `json:"text"`
		Target  string `json:"target"`
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
		target := strings.TrimSpace(args.Target)
		if target != "" && target != model.RouterName {
			return toolErr("为保证安全边界，定时器仅允许唤醒大脑（留空或 corgi），不可直接定时操纵 Worker")
		}
		e.recordAPIRouterInflight(context.Background(), trig, "add_timer", "")
		// Persist model.RouterName explicitly so subscribed workers in the thread never intercept a router timer.
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

func (e *Engine) toolReadDetail(ctx context.Context, turn *apiRouterTurn, rawArgs string) string {
	var args struct {
		Kind   string `json:"kind"`
		Target string `json:"target"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolErr("read_detail 参数 JSON 无效：" + err.Error())
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 4000
	}
	if limit > 7000 {
		limit = 7000
	}
	offset := args.Offset
	if offset < 0 {
		offset = 0
	}
	target := strings.TrimSpace(args.Target)
	if target == "" {
		return toolErr("target 不能为空")
	}

	paginate := func(s string) map[string]any {
		runes := []rune(s)
		total := len(runes)
		if offset >= total {
			return map[string]any{"ok": true, "total_chars": total, "offset": offset, "has_more": false, "content": ""}
		}
		end := offset + limit
		if end > total {
			end = total
		}
		return map[string]any{
			"ok":          true,
			"total_chars": total,
			"offset":      offset,
			"has_more":    end < total,
			"content":     string(runes[offset:end]),
		}
	}

	switch strings.ToLower(strings.TrimSpace(args.Kind)) {
	case "worker":
		wk, err := e.store.GetWorker(ctx, target)
		if err != nil || wk == nil {
			return toolErr("找不到 Worker：" + target)
		}
		if orStr(wk.Thread, model.MainThread) != turn.thread {
			return toolErr("无权跨线程读取 Worker：" + target)
		}
		out := map[string]any{"worker": wk}
		if !wk.Ended && wk.PaneID != "" {
			out["last_message"] = e.lastWorkerSummary(ctx, wk)
		}
		b, _ := json.Marshal(out)
		return toolJSON(paginate(string(b)))
	case "trigger":
		tr, err := e.store.GetTrigger(ctx, target)
		if err != nil || tr == nil {
			return toolErr("找不到 Trigger：" + target)
		}
		if orStr(tr.Thread, model.MainThread) != turn.thread {
			return toolErr("无权跨线程读取 Trigger：" + target)
		}
		return toolJSON(paginate(tr.Text))
	case "work_item":
		wi, err := e.store.GetWorkItem(ctx, target)
		if err != nil || wi == nil {
			return toolErr("找不到工作项：" + target)
		}
		if orStr(wi.Thread, model.MainThread) != turn.thread {
			return toolErr("无权跨线程读取工作项：" + target)
		}
		b, _ := json.Marshal(wi)
		return toolJSON(paginate(string(b)))
	case "action":
		act, err := e.store.GetAction(ctx, target)
		if err != nil || act == nil {
			return toolErr("找不到卡片：" + target)
		}
		if orStr(act.Thread, model.MainThread) != turn.thread {
			return toolErr("无权跨线程读取卡片：" + target)
		}
		b, _ := json.Marshal(act)
		return toolJSON(paginate(string(b)))
	case "note":
		if e.memory == nil {
			return toolErr("memory manager unavailable")
		}
		rel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(target)), "/")
		if rel != "instructions.md" && !strings.HasPrefix(rel, "notes/") {
			rel = "notes/" + rel
		}
		if !strings.HasSuffix(rel, ".md") {
			rel += ".md"
		}
		if strings.Contains(rel, "..") {
			return toolErr("非法笔记路径")
		}
		raw, err := os.ReadFile(filepath.Join(e.memory.MemDir(), rel))
		if err != nil {
			return toolErr("读取笔记失败：" + err.Error())
		}
		return toolJSON(paginate(string(raw)))
	case "project_file":
		if turn.thread != model.MainThread {
			return toolErr("仅主会话支持直接读取已配置项目文件")
		}
		parts := strings.SplitN(target, ":", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return toolErr("project_file 的 target 格式必须为 '<project_id>:<relative_path>'")
		}
		root, err := e.resolveLocalProjectRoot(strings.TrimSpace(parts[0]))
		if err != nil {
			return toolErr(err.Error())
		}
		rel := strings.TrimLeft(filepath.ToSlash(strings.TrimSpace(parts[1])), "/")
		if strings.Contains(rel, "..") || strings.HasPrefix(rel, ".git/") || rel == ".git" {
			return toolErr("不允许越界路径或 .git 内部文件")
		}
		full := filepath.Join(root, filepath.FromSlash(rel))
		realFull, err := filepath.EvalSymlinks(full)
		if err != nil {
			return toolErr("文件不存在或无法访问：" + rel)
		}
		if !strings.HasPrefix(realFull, root+string(os.PathSeparator)) && realFull != root {
			return toolErr("符号链接越界，已拒绝读取")
		}
		for _, seg := range strings.Split(filepath.ToSlash(realFull), "/") {
			if seg == ".git" {
				return toolErr("不允许读取 .git 内部文件")
			}
		}
		if e.store != nil && IsProtectedHubDBPath(e.store.DBPath(), realFull) {
			return toolErr("受保护的数据库文件不可读取")
		}
		fi, err := os.Stat(realFull)
		if err != nil || fi.IsDir() || !fi.Mode().IsRegular() {
			return toolErr("目标不是普通文件：" + rel)
		}
		if fi.Size() > 512<<10 {
			return toolErr("文件超过 512KB，请改派只读 Worker 检索")
		}
		raw, err := os.ReadFile(realFull)
		if err != nil {
			return toolErr("读取文件失败：" + err.Error())
		}
		return toolJSON(paginate(string(raw)))
	case "project_git":
		if turn.thread != model.MainThread {
			return toolErr("仅主会话支持直接查看项目 Git 状态")
		}
		parts := strings.SplitN(target, ":", 2)
		projID := strings.TrimSpace(parts[0])
		sub := "status"
		if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
			sub = strings.ToLower(strings.TrimSpace(parts[1]))
		}
		root, err := e.resolveLocalProjectRoot(projID)
		if err != nil {
			return toolErr(err.Error())
		}
		var gitArgs []string
		switch sub {
		case "status":
			gitArgs = []string{"-C", root, "status", "--short", "--branch", "--", "."}
		case "log":
			gitArgs = []string{"-C", root, "log", "-n", "15", "--oneline", "--decorate", "--", "."}
		case "diff":
			gitArgs = []string{"-C", root, "diff", "--stat", "--", "."}
		default:
			return toolErr("project_git 仅支持 status、log 或 diff")
		}
		gctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		out, err := exec.CommandContext(gctx, "git", gitArgs...).CombinedOutput()
		if err != nil {
			return toolErr("git 只读查询失败：" + oneLineClip(string(out), 200))
		}
		return toolJSON(paginate(string(out)))
	default:
		return toolErr("不支持的 read_detail kind：" + args.Kind)
	}
}

func (e *Engine) resolveLocalProjectRoot(projectRef string) (string, error) {
	st := e.Settings()
	proj := ResolveProject(st.Projects, projectRef)
	if proj == nil {
		return "", fmt.Errorf("未找到已配置的项目工作区：%s", projectRef)
	}
	rawRoot := ProjectPathForMachine(proj, "")
	if rawRoot == "" {
		return "", fmt.Errorf("项目 %s 未配置 Hub 本机路径（若在远端机器，请派 readonly Worker 查看）", proj.Name)
	}
	expanded := expandInHome(e.home, rawRoot)
	realRoot, err := filepath.EvalSymlinks(expanded)
	if err != nil {
		return "", fmt.Errorf("项目路径不可用：%s", expanded)
	}
	fi, err := os.Stat(realRoot)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("项目路径不是目录：%s", realRoot)
	}
	return filepath.Clean(realRoot), nil
}

func (e *Engine) toolSaveMemory(ctx context.Context, turn *apiRouterTurn, rawArgs string) string {
	var args struct {
		File    string `json:"file"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolErr("save_memory 参数 JSON 无效：" + err.Error())
	}
	if e.memory == nil {
		return toolErr("memory manager unavailable")
	}
	rel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(strings.TrimSpace(args.File))), "/")
	if rel == "instructions.md" && turn.thread != model.MainThread {
		return toolErr("企微群会话禁止修改主人的全局 instructions.md")
	}
	if rel != "instructions.md" && !(strings.HasPrefix(rel, "notes/") && strings.HasSuffix(rel, ".md")) {
		return toolErr("save_memory 仅允许写入 instructions.md 或 notes/<topic>.md")
	}
	if strings.Contains(rel, "..") {
		return toolErr("非法路径")
	}
	if len(args.Content) > 32<<10 {
		return toolErr("单份记忆文件不能超过 32KB")
	}
	if err := e.memory.SaveFile(rel, args.Content); err != nil {
		return toolErr("写入记忆失败：" + err.Error())
	}
	e.notifyChanged()
	return toolJSON(map[string]any{"ok": true, "file": rel})
}

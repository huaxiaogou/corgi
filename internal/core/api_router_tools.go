package core

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

func prop(typ, desc string) map[string]any {
	if typ == "[]string" {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	return map[string]any{"type": typ, "description": desc}
}
func propEnum(vals []string, desc string) map[string]any {
	return map[string]any{"type": "string", "enum": vals, "description": desc}
}
func toolSpec(name, desc string, props map[string]any, req ...string) LLMToolDef {
	return LLMToolDef{Type: "function", Function: LLMToolFunctionSpec{Name: name, Description: desc, Parameters: map[string]any{"type": "object", "properties": props, "required": req}}}
}

// apiRouterToolDefs returns the 9 deterministic, strictly ordered native Go tool definitions.
func apiRouterToolDefs() []LLMToolDef {
	return []LLMToolDef{
		toolSpec("dispatch_work", "启动新 Coding CLI Worker 或 resume 续聊已有 Worker。非人类回合仅允许新派 readonly。", map[string]any{
			"mode":          propEnum([]string{"readonly", "prototype", "implement"}, "readonly 只读调研，prototype 原型/草稿，implement 修改源码/跑测试（已有 prototype/readonly 工作项转 implement 必须先经决策卡拍板）"),
			"goal":          prop("string", "清晰、自包含的任务说明与验收标准"),
			"work_id":       prop("string", "关联的开放 work_item ID"),
			"title":         prop("string", "新建 work_item 时的简洁标题（≤20字）"),
			"project":       prop("string", "已配置的项目 ID 或名称"),
			"machine":       prop("string", "运行机器 ID（留空用全局默认）"),
			"kind":          propEnum([]string{"claude", "codex", "agy", "codebuddy"}, "指定 CLI 工具（留空用全局默认）"),
			"resume":        prop("string", "续聊已有/已结束 Worker 的名称（仅人类回合）"),
			"constraint":    prop("string", "给 Worker 的额外硬约束"),
			"worktree":      propEnum([]string{"auto", "on", "off"}, "worktree 隔离策略（未拍板发布的 implement 强制为 on）"),
			"allow_release": prop("boolean", "仅人类在含 choices 的决策卡上点选批准合入/推送/发版部署时设为 true（仅限决策卡拍板回合 + implement；普通文本消息需先发确认卡）"),
		}, "mode", "goal"),
		configRouterToolDef(),
		toolSpec("manage_timer", "列出、创建或删除本线程的定时提醒/巡检任务（固定唤醒大脑）。", map[string]any{
			"action":   propEnum([]string{"list", "add", "remove"}, "操作类型"),
			"spec":     prop("string", "add 时必填，如 'in 30m', 'daily 09:00', 'every 2h', 'at 2026-11-28 10:00'"),
			"text":     prop("string", "add 时必填，到点提醒内容"),
			"timer_id": prop("string", "remove 时必填的 timer ID"),
		}, "action"),
		toolSpec("manage_work", "管理工作项（新建/更新/关闭）或控制活跃 Worker（停止/保活）。", map[string]any{
			"action":    propEnum([]string{"create_work", "update_work", "close_work", "stop_worker", "keep_worker"}, "操作类型"),
			"target":    prop("string", "work_item ID 或 worker 名称"),
			"title":     prop("string", "工作项标题"),
			"next_step": prop("string", "下一步说明"),
			"note":      prop("string", "close_work 必填的关闭说明"),
			"keep":      prop("boolean", "keep_worker 时是否保活"),
		}, "action"),
		toolSpec("no_action", "仅用于后台巡检/晨检、进度播报或 webhook candidate 事件确认无需打扰用户时静默结束本轮。", map[string]any{
			"reason": prop("string", "简短说明本次无需打扰用户的内部原因"),
		}, "reason"),
		toolSpec("read_detail", "按需分页读取 Worker 详情、截断 Trigger、WorkItem、Action 或记忆笔记。", map[string]any{
			"kind":   propEnum([]string{"worker", "trigger", "work_item", "action", "note"}, "详情类型"),
			"target": prop("string", "对应 ID / 名称 / 笔记名"),
			"offset": prop("integer", "字符分页偏移量（默认 0）"),
			"limit":  prop("integer", "返回最大字符数（默认 4000，上限 7000）"),
		}, "kind", "target"),
		toolSpec("reply_user", "向用户发送最终回复、成果附件或决策卡片（有 choices 或 check=true 为决策/提醒卡；有 files 或 worker+re_id 为带交付回复）。发送成功后直接结束本轮。", map[string]any{
			"title":   prop("string", "决策卡可选短标题（≤20字纯文本，以事项名称为主语）"),
			"text":    prop("string", "回复正文（2项以上用 '- ' 列表分条；决策卡聚焦当前一件事，尽量 ≤100 字）"),
			"choices": prop("[]string", "决策卡片 2–4 个清晰选项（每项尽量 ≤14 字）"),
			"multi":   prop("boolean", "决策卡是否多选"),
			"check":   prop("boolean", "轻量已读提醒（无需多选项时使用）"),
			"files":   prop("[]string", "显式附带的绝对文件路径"),
			"machine": prop("string", "files 所在机器 ID（Hub 本机留空）"),
			"work_id": prop("string", "关联的 work_item ID"),
			"re_id":   prop("string", "引用的 entry ID 或 trigger ID"),
			"worker":  prop("string", "关联的 Worker 名称（配合 re_id 可自动附带完整报告快照）"),
		}, "text"),
		toolSpec("save_memory", "原子更新持久记忆文件（仅允许 instructions.md 或单层 notes/<slug>.md，≤32KB）。", map[string]any{
			"file":    prop("string", "相对 memory/ 的路径，如 'instructions.md' 或 'notes/project-foo.md'"),
			"content": prop("string", "完整新内容（≤32KB）"),
		}, "file", "content"),
		toolSpec("steer_worker", "向运行中的同线程 Worker 追加指令或纠偏（仅人类回合可用）。", map[string]any{
			"worker":  prop("string", "目标 Worker 名称"),
			"message": prop("string", "发给 Worker 的指令内容"),
			"work_id": prop("string", "可选工作项 ID"),
		}, "worker", "message"),
	}
}

func toolJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"ok":false,"error":"marshal tool result failed"}`
	}
	return string(b)
}
func toolErr(msg string) string { return toolJSON(map[string]any{"ok": false, "error": msg}) }

func paginateRunes(s string, offset, limit int) map[string]any {
	if limit <= 0 {
		limit = 4000
	}
	limit, offset, runes := min(limit, 7000), max(offset, 0), []rune(s)
	if offset >= len(runes) {
		return map[string]any{"ok": true, "total_chars": len(runes), "offset": offset, "has_more": false, "content": ""}
	}
	end := min(offset+limit, len(runes))
	return map[string]any{"ok": true, "total_chars": len(runes), "offset": offset, "has_more": end < len(runes), "content": string(runes[offset:end])}
}

// canAPIRouterSilence enforces the exact deterministic silence whitelist (Strategy 1A).
func canAPIRouterSilence(t *model.Trigger) bool {
	if t == nil {
		return false
	}
	if t.Policy == "candidate" {
		return strings.HasPrefix(t.Source, "webhook:")
	}
	if t.Policy != "respond" {
		return false
	}
	switch t.Source {
	case "system":
		return (t.From == "corgi" && (t.Ref == "idle-patrol" || t.Ref == "morning-digest")) || (t.From == "dream" && t.Ref != "")
	case "worker":
		body, ok := strings.CutPrefix(strings.TrimSpace(t.Text), "report from "+t.From+":\n")
		return t.From != "" && ok && isProgressReportText(body)
	}
	return false
}

func (e *Engine) execAPIRouterTool(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, meta *turnConsentMeta, tc LLMToolCall) (string, bool) {
	switch tc.Function.Name {
	case "reply_user":
		return e.toolReplyUser(ctx, turn, trig, tc.Function.Arguments)
	case "no_action":
		return e.toolNoAction(ctx, turn, trig, tc.Function.Arguments)
	case "dispatch_work":
		return e.toolDispatchWork(ctx, turn, trig, meta, tc.Function.Arguments), false
	case "manage_config":
		return e.toolManageConfig(ctx, turn, trig, meta, tc.Function.Arguments)
	case "steer_worker":
		return e.toolSteerWorker(ctx, turn, trig, meta, tc.Function.Arguments), false
	case "manage_work":
		return e.toolManageWork(ctx, turn, trig, tc.Function.Arguments), false
	case "manage_timer":
		return e.toolManageTimer(ctx, turn, trig, tc.Function.Arguments), false
	case "read_detail":
		return e.toolReadDetail(ctx, turn, tc.Function.Arguments), false
	case "save_memory":
		return e.toolSaveMemory(ctx, turn, tc.Function.Arguments), false
	}
	return toolErr("未知工具：" + tc.Function.Name), false
}

func (e *Engine) toolNoAction(_ context.Context, turn *apiRouterTurn, trig *model.Trigger, rawArgs string) (string, bool) {
	if !canAPIRouterSilence(trig) {
		return toolErr("当前事件必须向用户作出明确答复或同步结果（人类发言、任务完结汇报、故障提醒均不可静默），请改用 reply_user 回复用户。"), false
	}
	var args struct {
		Reason string `json:"reason"`
	}
	if strings.TrimSpace(rawArgs) != "" {
		if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
			return toolErr("no_action 参数 JSON 无效：" + err.Error()), false
		}
	}
	if strings.TrimSpace(args.Reason) == "" {
		return toolErr("no_action 需要提供简短 reason 说明无需打扰的原因"), false
	}
	e.logger.Info("api router no_action", "trigger", trig.ID, "thread", turn.thread, "source", trig.Source, "reason", oneLineClip(args.Reason, 160))
	turn.flags.Or(turnFlagNoAction)
	e.ackAPITriggerOnce(context.Background(), turn, trig)
	return toolJSON(map[string]any{"ok": true, "silent": true}), true
}

func (e *Engine) toolReplyUser(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, rawArgs string) (string, bool) {
	var args struct {
		Title   string   `json:"title"`
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
	text := strings.TrimSpace(args.Text)
	if text == "" {
		return toolErr("reply_user 的 text 不能为空"), false
	}
	if len(args.Choices) == 1 || (args.Multi && len(args.Choices) < 2) {
		return toolErr("决策卡片 choices 至少需要 2 个选项，或改用 check=true / 普通回复"), false
	}
	reID, workID := orStr(strings.TrimSpace(args.ReID), trig.Ref), strings.TrimSpace(args.WorkID)
	if reID != "" {
		if prev, err := e.store.GetEntry(ctx, reID); err == nil && prev != nil && orStr(prev.Thread, model.MainThread) != turn.thread {
			return toolErr("re_id 不属于当前会话线程，已拒绝跨线程引用"), false
		} else if prevTr, err := e.store.GetTrigger(ctx, reID); err == nil && prevTr != nil && orStr(prevTr.Thread, model.MainThread) != turn.thread {
			return toolErr("re_id 对应的 trigger 不属于当前会话线程，已拒绝跨线程引用"), false
		}
	}
	if _, errMsg := e.requireThreadWorkItem(ctx, workID, turn.thread); workID != "" && errMsg != "" {
		return toolErr(errMsg), false
	}
	if err := e.recordAPIRouterInflight(context.Background(), trig, "reply_user", ""); err != nil {
		return toolErr("记录防重标记失败，已中止发送：" + err.Error()), false
	}
	res, err := e.ReplyDetailed(ctx, ReplyRequest{
		Thread: turn.thread, Title: strings.TrimSpace(args.Title), Text: text, ReID: reID,
		Options: args.Choices, Mode: condStr(args.Multi && len(args.Choices) >= 2, model.ActionModeMulti, ""),
		Files: args.Files, Machine: strings.TrimSpace(args.Machine), WorkID: workID, Check: args.Check, Worker: strings.TrimSpace(args.Worker),
	})
	if err != nil {
		return toolErr("发送回复失败：" + err.Error()), false
	}
	_ = e.recordAPIRouterInflight(context.Background(), trig, "reply_user", res.Entry.ID)
	turn.markMutated("已回复 " + res.Entry.ID)
	turn.flags.Or(turnFlagReplied)
	e.ackAPITriggerOnce(context.Background(), turn, trig)
	out := map[string]any{"ok": true, "entry": res.Entry.ID}
	if res.Action != nil {
		out["action"] = res.Action.ID
	}
	if res.Warning != "" {
		out["warning"] = res.Warning
	}
	return toolJSON(out), true
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
	target := strings.TrimSpace(args.Target)
	if target == "" {
		return toolErr("target 不能为空")
	}
	pageJSON := func(v any) string {
		b, _ := json.Marshal(v)
		return toolJSON(paginateRunes(string(b), args.Offset, args.Limit))
	}
	switch strings.ToLower(strings.TrimSpace(args.Kind)) {
	case "worker":
		wk, errMsg := e.requireThreadWorker(ctx, target, turn.thread)
		if errMsg != "" {
			return toolErr(errMsg)
		}
		out := map[string]any{"worker": wk}
		if !wk.Ended && wk.PaneID != "" {
			out["last_message"] = e.lastWorkerSummary(ctx, wk)
			if e.fleet != nil {
				rctx, cancel := context.WithTimeout(ctx, 4*time.Second)
				rr, rErr := e.fleet.Read(rctx, wk.PaneID, "recent", "text", 40)
				cancel()
				if rErr == nil {
					if tail := compactScreenTail(rr.Text, 18, 1200); tail != "" {
						out["screen_tail"] = tail
					}
				}
			}
		}
		return pageJSON(out)
	case "trigger":
		tr, err := e.store.GetTrigger(ctx, target)
		if err != nil || tr == nil || orStr(tr.Thread, model.MainThread) != turn.thread {
			return toolErr("找不到或无权读取 Trigger：" + target)
		}
		return toolJSON(paginateRunes(tr.Text, args.Offset, args.Limit))
	case "work_item":
		wi, errMsg := e.requireThreadWorkItem(ctx, target, turn.thread)
		if errMsg != "" {
			return toolErr(errMsg)
		}
		return pageJSON(wi)
	case "action":
		act, err := e.store.GetAction(ctx, target)
		if err != nil || act == nil || orStr(act.Thread, model.MainThread) != turn.thread {
			return toolErr("找不到或无权读取卡片：" + target)
		}
		return pageJSON(act)
	case "note":
		if e.memory == nil {
			return toolErr("memory manager unavailable")
		}
		raw, err := e.memory.ReadNote(target)
		if err != nil {
			return toolErr("读取笔记失败：" + err.Error())
		}
		return toolJSON(paginateRunes(string(raw), args.Offset, args.Limit))
	}
	return toolErr("不支持的 read_detail kind：" + args.Kind)
}

func (e *Engine) toolSaveMemory(_ context.Context, turn *apiRouterTurn, rawArgs string) string {
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
	rel, err := ValidateMemoryWritePath(args.File)
	if err != nil {
		return toolErr(err.Error())
	}
	if rel == "instructions.md" && turn.thread != model.MainThread {
		return toolErr("企微群会话禁止修改主人的全局 instructions.md")
	}
	if err := e.memory.SaveFile(rel, args.Content); err != nil {
		return toolErr("写入记忆失败：" + err.Error())
	}
	e.notifyChanged()
	return toolJSON(map[string]any{"ok": true, "file": rel})
}

func (e *Engine) pushToolProgress(ctx, bg context.Context, turn *apiRouterTurn, tc LLMToolCall, lines *[]string) {
	line := strings.TrimSpace(e.formatAPIToolProgress(ctx, turn.thread, tc))
	if line == "" {
		return
	}
	if len(*lines) == 0 || (*lines)[len(*lines)-1] != "• "+line {
		*lines = append(*lines, "• "+line)
		if len(*lines) > 6 {
			*lines = (*lines)[len(*lines)-6:]
		}
	}
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnAPIRouterProgress(bg, turn.thread, turn.triggerID, strings.Join(*lines, "\n"))
}

func (e *Engine) formatAPIToolProgress(ctx context.Context, thread string, tc LLMToolCall) string {
	staticLabels := map[string]string{
		"reply_user": "正在整理回复…", "no_action": "正在完成核对…", "manage_config": "正在处理配置…",
		"manage_timer": "正在更新定时提醒…", "save_memory": "正在更新记忆笔记…",
	}
	if label, ok := staticLabels[tc.Function.Name]; ok {
		return label
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(tc.Function.Arguments), &raw)
	s := func(k string) string { v, _ := raw[k].(string); return strings.TrimSpace(v) }
	lbl := func(wiID, wkName string) string {
		if _, errMsg := e.requireThreadWorkItem(ctx, wiID, thread); wiID != "" && errMsg != "" {
			wiID = ""
		}
		if _, errMsg := e.requireThreadWorker(ctx, wkName, thread); wkName != "" && errMsg != "" {
			wkName = ""
		}
		t := e.workItemTitleFor(ctx, wiID, wkName)
		return condStr(t != "", "「"+oneLineClip(t, 28)+"」", "")
	}
	switch tc.Function.Name {
	case "read_detail":
		switch s("kind") {
		case "worker":
			return "正在查看" + lbl("", s("target")) + "最新进展…"
		case "work_item":
			return "正在查阅" + lbl(s("target"), "") + "事项详情…"
		case "note":
			return "正在查阅记忆笔记…"
		}
		return "正在查阅任务详情…"
	case "dispatch_work":
		m := orStr(lbl(s("work_id"), s("resume")), condStr(s("title") != "", "「"+oneLineClip(s("title"), 28)+"」", ""))
		return condStr(s("resume") != "", "正在续接推进", "正在安排推进") + orStr(m, "执行任务") + "…"
	case "steer_worker":
		return "正在向" + orStr(lbl(s("work_id"), s("worker")), "任务") + "发送跟进指令…"
	case "manage_work":
		switch s("action") {
		case "stop_worker":
			return "正在停止" + lbl("", s("target")) + "的执行进程…"
		case "close_work":
			return "正在关闭事项" + lbl(s("target"), "") + "…"
		}
		return "正在更新事项状态…"
	}
	return "正在处理中…"
}

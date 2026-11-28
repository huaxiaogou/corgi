package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

func (e *Engine) formatInflightRecoveryNote(ctx context.Context, m apiRouterInflight) string {
	if m.ReservedID != "" && e.store != nil {
		if wk, err := e.store.GetWorker(ctx, m.ReservedID); err == nil && wk != nil {
			return fmt.Sprintf("上次大脑回合在派发 #%s 后因 Hub 重启中断，该 Worker 已保留（状态：%s），未重复派发", wk.Name, wk.Status)
		}
		if en, err := e.store.GetEntry(ctx, m.ReservedID); err == nil && en != nil {
			return fmt.Sprintf("上次大脑回复（%s）已入库后因 Hub 重启中断，未重复发送", en.ID)
		}
		if wi, err := e.store.GetWorkItem(ctx, m.ReservedID); err == nil && wi != nil {
			return fmt.Sprintf("上次大脑回合在创建工作项「%s」（%s）后因 Hub 重启中断，已保留该工作项，未重复创建", oneLineClip(wi.Title, 40), wi.ID)
		}
		if timers, err := e.store.ListTimers(ctx); err == nil {
			for _, tm := range timers {
				if tm != nil && tm.ID == m.ReservedID {
					return fmt.Sprintf("上次大脑回合在创建定时提醒（%s）后因 Hub 重启中断，已保留该提醒，未重复创建", tm.ID)
				}
			}
		}
	}
	return fmt.Sprintf("上次大脑回合（%s）在 Hub 重启时中断，为防重复执行已跳过自动重放", m.Op)
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
		title := strings.TrimSpace(args.Title)
		if title == "" {
			return toolErr("create_work 必须提供 title")
		}
		if err := e.recordAPIRouterInflight(context.Background(), trig, "create_work", ""); err != nil {
			return toolErr("记录防重标记失败，已中止创建工作项：" + err.Error())
		}
		wi, err := e.SetWorkItem(ctx, "new", turn.thread, title, args.NextStep, "", "")
		if err != nil {
			return toolErr(err.Error())
		}
		_ = e.recordAPIRouterInflight(context.Background(), trig, "create_work", wi.ID)
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
	case "reopen_work":
		if _, errMsg := e.requireThreadWorkItem(ctx, target, turn.thread); errMsg != "" {
			return toolErr(errMsg)
		}
		wi, err := e.ReopenWorkItem(ctx, target)
		if err != nil {
			return toolErr(err.Error())
		}
		turn.markMutated("已重开工作项 " + wi.ID)
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
		spec, text := strings.TrimSpace(args.Spec), strings.TrimSpace(args.Text)
		if spec == "" || text == "" {
			return toolErr("add 定时器必须提供 spec 和 text")
		}
		if _, err := ParseTimerSpec(spec, time.Now()); err != nil {
			return toolErr(err.Error())
		}
		if err := e.recordAPIRouterInflight(context.Background(), trig, "manage_timer_add", ""); err != nil {
			return toolErr("记录防重标记失败，已中止创建定时器：" + err.Error())
		}
		tm, err := e.AddTimer(ctx, spec, text, turn.thread, model.RouterName, "corgi")
		if err != nil {
			return toolErr(err.Error())
		}
		_ = e.recordAPIRouterInflight(context.Background(), trig, "manage_timer_add", tm.ID)
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
	live := turn.setLiveProgress(*lines)
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnAPIRouterProgress(bg, turn.thread, turn.triggerID, strings.Join(*lines, "\n"))
	sink.OnAPIRouterLive(bg, live)
}

func (e *Engine) formatAPIToolProgress(ctx context.Context, thread string, tc LLMToolCall) string {
	staticLabels := map[string]string{
		"reply_user": "正在整理回复…", "no_action": "正在完成核对…", "manage_config": "正在处理配置…",
		"manage_context": "正在整理对话上下文…", "manage_timer": "正在更新定时提醒…", "save_memory": "正在更新记忆笔记…",
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
		case "artifact":
			return "正在查阅交付快照全文…"
		case "history":
			return "正在检索历史对话记录…"
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

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
)

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

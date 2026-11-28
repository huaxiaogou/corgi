package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
)

// RouterContextSummary builds the compact telemetry snapshot shown in RouterView.Context.
func (e *Engine) RouterContextSummary(ctx context.Context, thread string) *model.RouterContextView {
	thread = orStr(strings.TrimSpace(thread), model.MainThread)
	budget := e.Settings().EffectiveRouterContextBudgetTokens()
	view := &model.RouterContextView{
		Epoch:        1,
		BudgetTokens: budget,
		Compacting:   e.isRouterContextCompacting(thread),
	}
	if e.store == nil {
		return view
	}
	if st, err := e.store.GetRouterContextState(ctx, thread); err == nil {
		view.Epoch = st.Epoch
		view.EstTokens = st.TotalEstTokens
		view.LastPromptTokens = st.LastPromptTokens
		view.LastCachedTokens = st.LastCachedTokens
		view.CompactionCount = st.CompactionCount
		if n, cErr := e.store.CountActiveRouterContextTurns(ctx, thread, st.Epoch); cErr == nil {
			view.ActiveTurns = n
		}
	}
	return view
}

// InspectRouterContext returns active working-context telemetry, WorkItem groups, and turn previews.
func (e *Engine) InspectRouterContext(ctx context.Context, thread string) (*model.RouterContextInspect, error) {
	thread = orStr(strings.TrimSpace(thread), model.MainThread)
	summary := e.RouterContextSummary(ctx, thread)
	out := &model.RouterContextInspect{
		Thread:  thread,
		Summary: *summary,
		Groups:  []model.RouterContextGroup{},
		Turns:   []model.RouterContextTurnPreview{},
	}
	if e.store == nil {
		return out, nil
	}
	turns, err := e.store.ListActiveRouterContextTurns(ctx, thread, summary.Epoch)
	if err != nil {
		return nil, err
	}
	wiByID := map[string]*model.WorkItem{}
	if items, iErr := e.store.ListWorkItems(ctx, true); iErr == nil {
		for _, wi := range items {
			if wi != nil {
				wiByID[wi.ID] = wi
			}
		}
	}
	groupMap := map[string]*model.RouterContextGroup{}
	for _, t := range turns {
		if t == nil {
			continue
		}
		wiTitle := ""
		if t.WorkItemID != "" {
			if wi := wiByID[t.WorkItemID]; wi != nil {
				wiTitle = wi.Title
			}
		}
		out.Turns = append(out.Turns, model.RouterContextTurnPreview{
			ID:               t.ID,
			Seq:              t.Seq,
			Kind:             t.Kind,
			Source:           t.Source,
			WorkItemID:       t.WorkItemID,
			WorkItemIDs:      t.WorkItemIDs,
			WorkItemTitle:    wiTitle,
			UserPreview:      oneLineClip(t.UserText, 120),
			AssistantPreview: oneLineClip(t.AssistantText, 140),
			EstTokens:        t.EstTokens,
			CreatedAt:        t.CreatedAt,
		})
		if t.Kind == "summary" {
			continue
		}
		key := t.WorkItemID
		g := groupMap[key]
		if g == nil {
			title := "未归类对话"
			closed := false
			if key != "" {
				if wi := wiByID[key]; wi != nil {
					title = orStr(wi.Title, key)
					closed = wi.ClosedAt != nil
				} else {
					title = key
				}
			}
			g = &model.RouterContextGroup{WorkItemID: key, Title: title, Closed: closed}
			groupMap[key] = g
		}
		g.TurnCount++
		g.EstTokens += t.EstTokens
	}
	for _, g := range groupMap {
		out.Groups = append(out.Groups, *g)
	}
	sort.Slice(out.Groups, func(i, j int) bool {
		if (out.Groups[i].WorkItemID == "") != (out.Groups[j].WorkItemID == "") {
			return out.Groups[i].WorkItemID != ""
		}
		if out.Groups[i].EstTokens != out.Groups[j].EstTokens {
			return out.Groups[i].EstTokens > out.Groups[j].EstTokens
		}
		return out.Groups[i].WorkItemID < out.Groups[j].WorkItemID
	})
	return out, nil
}

// PruneRouterContext executes selective discard, immediate compaction, or full thread context reset
// while preserving lossless chat entries.
func (e *Engine) PruneRouterContext(ctx context.Context, req PruneRouterContextRequest) (*model.RouterContextPruneResult, error) {
	if e.store == nil {
		return nil, fmt.Errorf("store unavailable")
	}
	thread := orStr(strings.TrimSpace(req.Thread), model.MainThread)
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	reason := orStr(strings.TrimSpace(req.Reason), "manual")

	var (
		dropped, freed int
		compacting     bool
		msg            string
	)
	switch mode {
	case "work_item":
		wid := strings.TrimSpace(req.WorkItemID)
		if wid == "" {
			return nil, fmt.Errorf("work_item_id 不能为空")
		}
		var err error
		dropped, freed, _, err = e.store.DropRouterContextTurnsByWorkItem(ctx, thread, wid, reason)
		if err != nil {
			return nil, err
		}
		if dropped == 0 {
			msg = "当前活跃上下文中没有该事项的轮次"
		} else {
			msg = fmt.Sprintf("已从大脑上下文抛弃 %d 轮（约释放 %d tokens，聊天记录已保留）", dropped, freed)
		}
	case "turns":
		if len(req.TurnIDs) == 0 {
			return nil, fmt.Errorf("turn_ids 不能为空")
		}
		var err error
		dropped, freed, _, err = e.store.DropRouterContextTurnsByIDs(ctx, thread, req.TurnIDs, reason)
		if err != nil {
			return nil, err
		}
		msg = fmt.Sprintf("已抛弃 %d 轮上下文（约释放 %d tokens，聊天记录已保留）", dropped, freed)
	case "before":
		if req.BeforeSeq <= 0 {
			return nil, fmt.Errorf("before_seq 必须大于 0")
		}
		var err error
		dropped, freed, _, err = e.store.DropRouterContextTurnsBeforeSeq(ctx, thread, req.BeforeSeq, reason)
		if err != nil {
			return nil, err
		}
		msg = fmt.Sprintf("已抛弃 seq < %d 的 %d 轮较早上下文（约释放 %d tokens）", req.BeforeSeq, dropped, freed)
	case "compact":
		st, err := e.store.GetRouterContextState(ctx, thread)
		if err != nil {
			return nil, err
		}
		turns, err := e.store.ListActiveRouterContextTurns(ctx, thread, st.Epoch)
		if err != nil {
			return nil, err
		}
		if len(turns) <= 1 {
			msg = "当前上下文轮次较少，暂无需压缩"
		} else {
			cfg, modelName, effort, _, cErr := e.resolveActiveRouterAPIConfig(ctx)
			if cErr != nil {
				return nil, cErr
			}
			if e.maybeStartContextCompaction(thread, st, cfg, modelName, effort, true) {
				compacting = true
				msg = "已启动后台压缩，旧轮次将整理为前情摘要并保留最近轮次原文"
			} else {
				compacting = e.isRouterContextCompacting(thread)
				msg = "后台压缩正在进行中，请稍候"
			}
		}
	case "clear":
		var err error
		dropped, freed, _, err = e.store.ClearRouterContextThread(ctx, thread)
		if err != nil {
			return nil, err
		}
		msg = fmt.Sprintf("已清空大脑工作上下文（释放 %d 轮，历史聊天记录与长期记忆均已保留）", dropped)
	default:
		return nil, fmt.Errorf("不支持的上下文操作 mode：%s", mode)
	}

	e.notifyChanged()
	return &model.RouterContextPruneResult{
		OK:             true,
		Mode:           mode,
		DroppedTurns:   dropped,
		FreedEstTokens: freed,
		Compacting:     compacting,
		Message:        msg,
		Summary:        *e.RouterContextSummary(ctx, thread),
	}, nil
}

func manageContextToolDef() LLMToolDef {
	return toolSpec("manage_context", "查看、按事项/轮次抛弃、立即压缩或清空当前线程的大脑工作上下文（不影响聊天记录 entries 与长期记忆；写操作仅限人类回合）。", map[string]any{
		"action":       propEnum([]string{"inspect", "prune_work_item", "prune_turns", "compact", "clear"}, "操作类型"),
		"work_item_id": prop("string", "prune_work_item 时必填的工作项 ID"),
		"turn_ids":     prop("[]string", "prune_turns 时必填的上下文轮次 ID 列表（如 rt_12）"),
	}, "action")
}

func (e *Engine) toolManageContext(ctx context.Context, turn *apiRouterTurn, meta *turnConsentMeta, rawArgs string) string {
	var args struct {
		Action     string   `json:"action"`
		WorkItemID string   `json:"work_item_id"`
		TurnIDs    []string `json:"turn_ids"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolErr("manage_context 参数 JSON 无效：" + err.Error())
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "inspect" {
		info, err := e.InspectRouterContext(ctx, turn.thread)
		if err != nil {
			return toolErr("读取上下文状态失败：" + err.Error())
		}
		return toolJSON(map[string]any{"ok": true, "context": info})
	}
	if meta == nil || !meta.isHuman {
		return toolErr("仅人类对话回合可以修改或抛弃工作上下文")
	}
	modeMap := map[string]string{
		"prune_work_item": "work_item",
		"prune_turns":     "turns",
		"compact":         "compact",
		"clear":           "clear",
	}
	mode, ok := modeMap[action]
	if !ok {
		return toolErr("不支持的 manage_context action：" + args.Action)
	}
	res, err := e.PruneRouterContext(ctx, PruneRouterContextRequest{
		Thread:     turn.thread,
		Mode:       mode,
		WorkItemID: args.WorkItemID,
		TurnIDs:    args.TurnIDs,
		Reason:     "router_tool",
	})
	if err != nil {
		return toolErr(err.Error())
	}
	return toolJSON(res)
}

// recordAPIToolTrace extracts a compact, bounded breadcrumb + all touched work_item_ids from a tool execution
// so follow-up turns recall tool findings without repeating read_detail or losing work-item attribution.
func (e *Engine) recordAPIToolTrace(ctx context.Context, turn *apiRouterTurn, tc LLMToolCall, out string) {
	if turn == nil || tc.Function.Name == "reply_user" || tc.Function.Name == "no_action" {
		return
	}
	var argsMap, outMap map[string]any
	_ = json.Unmarshal([]byte(tc.Function.Arguments), &argsMap)
	_ = json.Unmarshal([]byte(out), &outMap)
	strVal := func(m map[string]any, key string) string {
		if m == nil {
			return ""
		}
		v, _ := m[key].(string)
		return strings.TrimSpace(v)
	}

	var wiIDs []string
	addWI := func(id string) {
		if id = strings.TrimSpace(id); id != "" {
			wiIDs = append(wiIDs, id)
		}
	}
	addWI(strVal(argsMap, "work_id"))
	addWI(strVal(argsMap, "work_item_id"))
	addWI(strVal(outMap, "work_item_id"))
	if wiObj, ok := outMap["work_item"].(map[string]any); ok {
		addWI(strVal(wiObj, "id"))
	}
	if tc.Function.Name == "manage_work" {
		act := strings.ToLower(strVal(argsMap, "action"))
		if act == "update_work" || act == "close_work" || act == "reopen_work" {
			addWI(strVal(argsMap, "target"))
		}
	}
	if tc.Function.Name == "read_detail" {
		kind := strings.ToLower(strVal(argsMap, "kind"))
		target := strVal(argsMap, "target")
		if kind == "work_item" {
			addWI(target)
		} else if kind == "worker" && target != "" && e.store != nil {
			if wk, err := e.store.GetWorker(ctx, target); err == nil && wk != nil {
				addWI(wk.WorkItemID)
			}
		}
	}
	if tc.Function.Name == "steer_worker" && e.store != nil {
		if wName := strVal(argsMap, "worker"); wName != "" {
			if wk, err := e.store.GetWorker(ctx, wName); err == nil && wk != nil {
				addWI(wk.WorkItemID)
			}
		}
	}

	okFlag, _ := outMap["ok"].(bool)
	errText := strVal(outMap, "error")
	var line string
	switch tc.Function.Name {
	case "read_detail":
		kind, target := strVal(argsMap, "kind"), strVal(argsMap, "target")
		if !okFlag {
			line = fmt.Sprintf("- read_detail(%s, %s) 失败：%s", kind, target, oneLineClip(errText, 120))
		} else if content := strVal(outMap, "content"); content != "" {
			line = fmt.Sprintf("- read_detail(%s, %s)：%s", kind, target, clipRunes(strings.TrimSpace(content), maxToolReadExcerptRunes, "...[截断]"))
		} else {
			line = fmt.Sprintf("- read_detail(%s, %s)：%s", kind, target, oneLineClip(out, maxToolTraceEntryRunes))
		}
	case "dispatch_work":
		mode, title, wkName := strVal(argsMap, "mode"), strVal(argsMap, "title"), strVal(outMap, "worker")
		if !okFlag {
			line = fmt.Sprintf("- dispatch_work(%s) 失败：%s", mode, oneLineClip(errText, 140))
		} else {
			line = fmt.Sprintf("- dispatch_work(%s) -> #%s work=%s branch=%s %s",
				mode, wkName, strVal(outMap, "work_item_id"), strVal(outMap, "branch"), oneLineClip(title, 40))
		}
	case "steer_worker":
		wkName := strVal(argsMap, "worker")
		if !okFlag {
			line = fmt.Sprintf("- steer_worker(#%s) 失败：%s", wkName, oneLineClip(errText, 120))
		} else {
			line = fmt.Sprintf("- steer_worker(#%s)：%s", wkName, oneLineClip(strVal(argsMap, "message"), 120))
		}
	default:
		if !okFlag {
			line = fmt.Sprintf("- %s 失败：%s", tc.Function.Name, oneLineClip(errText, 140))
		} else {
			act := strVal(argsMap, "action")
			prefix := tc.Function.Name
			if act != "" {
				prefix += "(" + act + ")"
			}
			line = fmt.Sprintf("- %s -> %s", prefix, oneLineClip(out, maxToolTraceEntryRunes))
		}
	}
	turn.recordToolTrace(line, wiIDs...)
}

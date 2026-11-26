package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/core"
)

type wecomFocus struct {
	Worker     string `json:"worker"`
	Title      string `json:"title,omitempty"`
	Machine    string `json:"machine,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Session    string `json:"session,omitempty"`
	WorkItemID string `json:"work_item_id,omitempty"`
}

func wecomFocusKey(chatType, chatID, fromUser string) string {
	if strings.EqualFold(strings.TrimSpace(chatType), "group") && strings.TrimSpace(chatID) != "" {
		return "wecom_focus:" + strings.TrimSpace(chatID) + ":" + strings.TrimSpace(fromUser)
	}
	id := strings.TrimSpace(fromUser)
	if id == "" {
		id = strings.TrimSpace(chatID)
	}
	return "wecom_focus:" + id
}

func (b *Bus) loadWeComFocus(ctx context.Context, key string) *wecomFocus {
	if b.engine == nil || key == "" {
		return nil
	}
	raw, ok, err := b.engine.GetKV(ctx, key)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	var f wecomFocus
	if json.Unmarshal([]byte(raw), &f) != nil || strings.TrimSpace(f.Worker) == "" {
		return nil
	}
	return &f
}

func (b *Bus) saveWeComFocus(ctx context.Context, key string, f *wecomFocus) {
	if b.engine == nil || key == "" {
		return
	}
	if f == nil || strings.TrimSpace(f.Worker) == "" {
		_ = b.engine.DeleteKV(ctx, key)
		return
	}
	if raw, err := json.Marshal(f); err == nil {
		_ = b.engine.SetKV(ctx, key, string(raw))
	}
}

func isReturnMainCommand(text string) bool {
	s := strings.ToLower(strings.TrimSpace(text))
	switch s {
	case "回主对话", "返回主对话", "回到主对话",
		"回主会话", "返回主会话", "回到主会话",
		"回主聊天", "返回主聊天",
		"退出直聊", "退出",
		"回corgi", "回 corgi", "返回corgi", "返回 corgi",
		"回柯基", "返回柯基",
		"/back", "/main", "/corgi", "/exit", "/q":
		return true
	default:
		return false
	}
}

func isWorkerPickerCommand(text string) bool {
	s := strings.TrimSpace(text)
	switch strings.ToLower(s) {
	case "/w", "/workers", "切换", "切换对话":
		return true
	default:
		return false
	}
}

func parseDirectSwitchTarget(text string) (string, bool) {
	s := strings.TrimSpace(text)
	low := strings.ToLower(s)
	for _, prefix := range []string{"/w ", "/to "} {
		if strings.HasPrefix(low, prefix) {
			name := strings.TrimSpace(strings.TrimPrefix(s[len(prefix):], "#"))
			if name != "" {
				return name, true
			}
		}
	}
	return "", false
}

// sendInstantWeComText responds via a single sealed stream frame when reqID is present,
// falling back to active markdown when stream is unavailable.
func (b *Bus) sendInstantWeComText(ctx context.Context, chatID, reqID, markdown string) {
	if strings.TrimSpace(reqID) != "" && b.canSendStream() {
		sess := wecom.NewStreamSession(reqID, chatID, b.sendStreamFn)
		if _, err := sess.Finish(ctx, "", markdown); err == nil {
			return
		}
	}
	if b.canSendMarkdown() {
		_ = b.sendFn(ctx, chatID, markdown)
	}
}

func (b *Bus) selectableWeComOptions(ctx context.Context) ([]core.WorkerPickItem, []wecom.WorkerPickOption) {
	items := b.engine.ListSelectableWorkers(ctx, 30)
	opts := make([]wecom.WorkerPickOption, 0, len(items))
	for _, it := range items {
		opts = append(opts, wecom.WorkerPickOption{
			Name:        it.Name,
			Title:       it.Title,
			Machine:     it.Machine,
			Kind:        it.Kind,
			Status:      it.Status,
			HasWorkItem: it.HasWorkItem,
			Ended:       it.Ended,
		})
	}
	return items, opts
}

func (b *Bus) resolveSwitchQuery(ctx context.Context, query string) (workerName, displayTitle string) {
	q := strings.TrimSpace(strings.TrimPrefix(query, "#"))
	if q == "" {
		return "", ""
	}
	items := b.engine.ListSelectableWorkers(ctx, 30)
	// Support 1-based index (`/w 1`), exact worker name, or Chinese title keyword (`/w 太湖`).
	if idx, err := strconv.Atoi(q); err == nil && idx >= 1 && idx <= len(items) {
		return items[idx-1].Name, items[idx-1].Title
	}
	low := strings.ToLower(q)
	for _, it := range items {
		if strings.EqualFold(it.Name, q) {
			return it.Name, it.Title
		}
	}
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.Title), low) || strings.Contains(strings.ToLower(it.Name), low) {
			return it.Name, it.Title
		}
	}
	return q, ""
}

func (b *Bus) handleWeComControlOrDirect(ctx context.Context, msg wecom.IncomingMessage, thread, text string) bool {
	focusKey := wecomFocusKey(msg.ChatType, msg.ChatID, msg.FromUser)
	replyChatID := msg.ChatID
	if replyChatID == "" {
		replyChatID = msg.FromUser
	}

	// 1. Return to Corgi main chat.
	if isReturnMainCommand(text) {
		b.saveWeComFocus(ctx, focusKey, nil)
		b.sendInstantWeComText(ctx, replyChatID, msg.ReqID, "已回到 **Corgi 主对话**。")
		return true
	}

	// 2. Pop up clickable, paginated Worker/matter picker card.
	if isWorkerPickerCommand(text) {
		curWorker, curTitle := "", ""
		if f := b.loadWeComFocus(ctx, focusKey); f != nil {
			curWorker, curTitle = f.Worker, f.Title
		}
		items, opts := b.selectableWeComOptions(ctx)
		if len(items) == 0 && curWorker == "" {
			b.sendInstantWeComText(ctx, replyChatID, msg.ReqID, "目前还没有可切换的事项或 Worker，直接在主对话派活即可。")
			return true
		}
		if b.wecom != nil && b.wecom.Online() {
			if err := b.wecom.SendWorkerPickerCard(ctx, replyChatID, curWorker, curTitle, opts, 0); err != nil {
				b.logger.Warn("send wecom worker picker card failed", "err", err)
				b.sendInstantWeComText(ctx, replyChatID, msg.ReqID, "发送切换卡片失败，可直接发 `/w 1` 或 `/w <关键词>` 切换。")
			}
			return true
		}
		// Fallback text list when client is mocked without template_card support.
		var lines []string
		for i, it := range items {
			lines = append(lines, fmt.Sprintf("%d. **%s**（`#%s` · %s）", i+1, it.Title, it.Name, it.Machine))
		}
		b.sendInstantWeComText(ctx, replyChatID, msg.ReqID, "可选事项 / 会话：\n"+strings.Join(lines, "\n")+"\n\n发送 `/w <编号或关键词>` 切入直聊。")
		return true
	}

	// 3. Explicit `/w <编号|名称|关键词>` or `/to <...>` switch.
	if query, ok := parseDirectSwitchTarget(text); ok {
		workerName, title := b.resolveSwitchQuery(ctx, query)
		b.switchWeComFocusTo(ctx, focusKey, replyChatID, msg.ReqID, workerName, title, nil)
		return true
	}

	// 4. Native quote containing `[#worker]` -> one-shot direct follow-up without changing saved focus.
	if tag := core.ExtractQuoteWorkerTag(msg.QuoteText); tag != "" {
		b.dispatchWeComDirectWorker(ctx, msg, thread, replyChatID, wecomFocus{Worker: tag}, text, false)
		return true
	}

	// 5. Active Worker focus mode -> route straight to focused Worker.
	if focus := b.loadWeComFocus(ctx, focusKey); focus != nil {
		b.dispatchWeComDirectWorker(ctx, msg, thread, replyChatID, *focus, text, true)
		return true
	}
	return false
}

func (b *Bus) switchWeComFocusTo(ctx context.Context, focusKey, chatID, reqID, workerName, displayTitle string, cardEv *wecom.CardClickEvent) {
	if displayTitle == "" {
		for _, it := range b.engine.ListSelectableWorkers(ctx, 30) {
			if it.Name == workerName {
				displayTitle = it.Title
				break
			}
		}
	}
	w, resumed, err := b.engine.ResumeWorkerSession(ctx, core.ResumeWorkerRequest{Name: workerName})
	if err != nil {
		errMsg := fmt.Sprintf("无法切到「%s」：%s（仍保持原对话对象）", firstNonEmptyStr(displayTitle, "#"+workerName), err.Error())
		if cardEv != nil && b.wecom != nil {
			_ = b.wecom.UpdateNavCard(ctx, cardEv.ReqID, cardEv.TaskID, "切换失败", "切换失败", errMsg)
		} else {
			b.sendInstantWeComText(ctx, chatID, reqID, errMsg)
		}
		return
	}
	label := firstNonEmptyStr(displayTitle, w.Title, w.Name)
	machineLabel := b.engine.MachineDisplayName(w.Machine)
	b.saveWeComFocus(ctx, focusKey, &wecomFocus{
		Worker:     w.Name,
		Title:      label,
		Machine:    w.Machine,
		Kind:       w.Kind,
		Session:    w.Session,
		WorkItemID: w.WorkItemID,
	})
	actionVerb := "已切到"
	if resumed {
		actionVerb = "已恢复并切到"
	}
	if cardEv != nil && b.wecom != nil {
		cardDetail := fmt.Sprintf("%s · %s · 直接发消息即可，发「回主对话」返回 Corgi。", machineLabel, w.Kind)
		_ = b.wecom.UpdateNavCard(ctx, cardEv.ReqID, cardEv.TaskID, actionVerb+"："+label, "已切换", cardDetail)
		return
	}
	msg := fmt.Sprintf("%s **%s**（`#%s` · %s · %s）。直接发消息即可，发「回主对话」返回 Corgi。", actionVerb, label, w.Name, machineLabel, w.Kind)
	b.sendInstantWeComText(ctx, chatID, reqID, msg)
}

func (b *Bus) handleNavCardClick(ctx context.Context, ev wecom.CardClickEvent) bool {
	if ev.ActionID != "nav" && !strings.HasPrefix(ev.TaskID, "nav:") {
		return false
	}
	focusKey := wecomFocusKey(ev.ChatType, ev.ChatID, ev.FromUser)
	replyChatID := ev.ChatID
	if replyChatID == "" {
		replyChatID = ev.FromUser
	}
	if ev.ChoiceKey == "main" {
		b.saveWeComFocus(ctx, focusKey, nil)
		if b.wecom != nil {
			_ = b.wecom.UpdateNavCard(ctx, ev.ReqID, ev.TaskID, "已回到 Corgi 主对话", "已回主对话", "后续消息将发回 Corgi 主对话。")
		}
		return true
	}
	if pageStr, ok := strings.CutPrefix(ev.ChoiceKey, "page:"); ok {
		page, _ := strconv.Atoi(strings.TrimSpace(pageStr))
		curWorker, curTitle := "", ""
		if f := b.loadWeComFocus(ctx, focusKey); f != nil {
			curWorker, curTitle = f.Worker, f.Title
		}
		_, opts := b.selectableWeComOptions(ctx)
		if b.wecom != nil {
			if err := b.wecom.UpdateWorkerPickerCard(ctx, ev.ReqID, ev.TaskID, curWorker, curTitle, opts, page); err != nil {
				b.logger.Warn("update wecom worker picker page failed", "page", page, "err", err)
			}
		}
		return true
	}
	if target, ok := strings.CutPrefix(ev.ChoiceKey, "focus:"); ok && strings.TrimSpace(target) != "" {
		b.switchWeComFocusTo(ctx, focusKey, replyChatID, "", strings.TrimSpace(target), "", &ev)
		return true
	}
	return true
}

func (b *Bus) dispatchWeComDirectWorker(ctx context.Context, msg wecom.IncomingMessage, thread, chatID string, target wecomFocus, text string, inFocusMode bool) {
	// Open passive stream immediately for instant WeCom "思考中..." feedback, then start watching
	// once SendDirectWorkerMessage has resolved/resumed the exact live Worker pane.
	stream, watchCtx := b.openWorkerStream(ctx, thread, chatID, msg.ReqID, firstNonEmptyStr(target.Title, target.Worker))

	onDone := func(cbCtx context.Context, res core.DirectTurnResult) {
		header := fmt.Sprintf("[#%s · %s]", res.WorkerName, res.MachineLabel)
		if strings.TrimSpace(target.Title) != "" && target.Title != res.WorkerName {
			header = fmt.Sprintf("[#%s] %s（%s）", res.WorkerName, target.Title, res.MachineLabel)
		}
		var body string
		if res.QuotaError != "" {
			body = fmt.Sprintf("%s\n\n⚠ Worker 遇到模型/配额错误：%s", header, res.QuotaError)
		} else {
			ans := strings.TrimSpace(res.FullText)
			if ans == "" {
				ans = "已完成本轮处理。"
			}
			body = header + "\n\n" + ans
			if strings.TrimSpace(res.PreviewURL) != "" {
				body += "\n\n🔗 [在线预览 · 完整报告](" + res.PreviewURL + ")"
			}
		}

		sealed := false
		if stream != nil {
			if ok, err := stream.seal(cbCtx, "", body); err != nil {
				b.logger.Warn("seal wecom worker direct stream failed", "worker", res.WorkerName, "err", err)
			} else {
				sealed = ok
			}
		}
		if !sealed && b.canSendMarkdown() {
			if err := b.sendFn(cbCtx, chatID, body); err != nil {
				b.logger.Warn("send wecom direct worker reply failed", "worker", res.WorkerName, "err", err)
			}
		}
		// Attach offline-readable .md file if Worker answer is long (>1800 runes) or exceeded stream budget.
		if res.QuotaError == "" && b.sendFileFn != nil && (b.wecom == nil || b.wecom.Online()) {
			if utf8.RuneCountInString(res.FullText) > 1800 || len(body) > wecom.StreamMaxBytes-512 {
				fname := fmt.Sprintf("%s-%s.md", res.WorkerName, time.Now().Format("150405"))
				if err := b.sendFileFn(cbCtx, chatID, fname, []byte(res.FullText)); err != nil {
					b.logger.Warn("send wecom direct worker report file failed", "worker", res.WorkerName, "err", err)
				}
			}
		}
	}

	w, _, err := b.engine.SendDirectWorkerMessage(ctx, core.DirectWorkerRequest{
		Name:       target.Worker,
		Machine:    target.Machine,
		Session:    target.Session,
		Kind:       target.Kind,
		WorkItemID: target.WorkItemID,
		Text:       text,
		OnDone:     onDone,
	})
	if err != nil {
		errText := fmt.Sprintf("发送给「%s」失败：%s", firstNonEmptyStr(target.Title, "#"+target.Worker), err.Error())
		if inFocusMode {
			errText += "\n可发送「回主对话」返回 Corgi，或发 `/w` 切换其他事项。"
		}
		if stream != nil {
			if ok, _ := stream.seal(ctx, "", errText); ok {
				return
			}
		}
		b.sendInstantWeComText(ctx, chatID, msg.ReqID, errText)
		return
	}

	if w != nil {
		b.beginWatchWorkerStream(watchCtx, stream, w.Name, b.engine.MachineDisplayName(w.Machine))
	}

	// Keep saved focus metadata fresh (e.g. newly assigned session ID or WorkItemID).
	if inFocusMode && w != nil {
		focusKey := wecomFocusKey(msg.ChatType, msg.ChatID, msg.FromUser)
		b.saveWeComFocus(ctx, focusKey, &wecomFocus{
			Worker:     w.Name,
			Title:      firstNonEmptyStr(target.Title, w.Title, w.Name),
			Machine:    w.Machine,
			Kind:       w.Kind,
			Session:    w.Session,
			WorkItemID: w.WorkItemID,
		})
	}
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

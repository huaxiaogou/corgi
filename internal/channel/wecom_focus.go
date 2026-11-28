package channel

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
)

type wecomFocus = core.WeComFocus

// directReplyMaxBytes reserves headroom for WeCom's <think>...</think> envelope inside StreamMaxBytes
// so FormatStreamContent never clips off the direct-chat footer or preview link.
const directReplyMaxBytes = wecom.StreamMaxBytes - 768

func wecomFocusKey(chatType, chatID, fromUser string) string {
	if strings.EqualFold(strings.TrimSpace(chatType), "group") && strings.TrimSpace(chatID) != "" {
		return core.WeComFocusKVPrefix + strings.TrimSpace(chatID) + ":" + strings.TrimSpace(fromUser)
	}
	id := strings.TrimSpace(fromUser)
	if id == "" {
		id = strings.TrimSpace(chatID)
	}
	return core.WeComFocusKVPrefix + id
}

func (b *Bus) loadWeComFocusResult(ctx context.Context, key string) core.WeComFocusLoadResult {
	if b.engine == nil || key == "" {
		return core.WeComFocusLoadResult{TTL: core.DefaultWeComFocusTTL}
	}
	return b.engine.LoadWeComFocus(ctx, key)
}

func (b *Bus) loadWeComFocus(ctx context.Context, key string) *wecomFocus {
	return b.loadWeComFocusResult(ctx, key).Focus
}

func (b *Bus) saveWeComFocus(ctx context.Context, key string, f *wecomFocus) {
	if b.engine == nil || key == "" {
		return
	}
	_ = b.engine.SaveWeComFocus(ctx, key, f)
}

func isHelpCommand(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "/help", "帮助":
		return true
	default:
		return false
	}
}

func isReturnMainCommand(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "/back", "回主对话", "回主会话":
		return true
	default:
		return false
	}
}

func isWorkerPickerCommand(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "/w", "切换":
		return true
	default:
		return false
	}
}

func parseDecisionListCommand(text string) (query string, ok bool) {
	s := strings.TrimSpace(text)
	low := strings.ToLower(s)
	if low == "/d" || low == "待拍板" || low == "待办" {
		return "", true
	}
	if strings.HasPrefix(low, "/d ") {
		return strings.TrimSpace(s[3:]), true
	}
	return "", false
}

func parseDirectSwitchTarget(text string) (string, bool) {
	s := strings.TrimSpace(text)
	if strings.HasPrefix(strings.ToLower(s), "/w ") {
		name := strings.TrimSpace(strings.TrimPrefix(s[3:], "#"))
		if name != "" {
			return name, true
		}
	}
	return "", false
}

func formatFocusTTLShort(d time.Duration) string {
	if d <= 0 {
		d = core.DefaultWeComFocusTTL
	}
	mins := int(d / time.Minute)
	if mins <= 0 {
		return "1m"
	}
	return fmt.Sprintf("%dm", mins)
}

func formatDirectHeader(title, workerName, machineLabel string) string {
	workerName = strings.TrimSpace(workerName)
	title = strings.TrimSpace(title)
	machineLabel = strings.TrimSpace(machineLabel)
	if title != "" && !strings.EqualFold(title, workerName) {
		if machineLabel != "" {
			return fmt.Sprintf("↳ 直聊 · %s（#%s · %s）", title, workerName, machineLabel)
		}
		return fmt.Sprintf("↳ 直聊 · %s（#%s）", title, workerName)
	}
	if machineLabel != "" {
		return fmt.Sprintf("↳ 直聊 · #%s（%s）", workerName, machineLabel)
	}
	return fmt.Sprintf("↳ 直聊 · #%s", workerName)
}

func formatDirectFooter(title, workerName string, inFocusMode bool, currentFocusWorker string) string {
	workerName = strings.TrimSpace(workerName)
	title = strings.TrimSpace(title)
	currentFocusWorker = strings.TrimSpace(currentFocusWorker)
	if workerName == "" {
		return ""
	}
	if inFocusMode {
		if title != "" && !strings.EqualFold(title, workerName) {
			return fmt.Sprintf("> **↳ 直聊中 · %s（#%s）** · 发 /back 回 Corgi · /w 切换", title, workerName)
		}
		return fmt.Sprintf("> **↳ 直聊中 · #%s** · 发 /back 回 Corgi · /w 切换", workerName)
	}
	if currentFocusWorker != "" && !strings.EqualFold(currentFocusWorker, workerName) {
		return fmt.Sprintf("> ↳ 单次直问 · #%s · 当前焦点仍在 #%s（发 /back 回 Corgi）", workerName, currentFocusWorker)
	}
	return fmt.Sprintf("> ↳ 单次直问 · #%s · 当前仍在 Corgi 主对话", workerName)
}

func trimUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	i := maxBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// formatDirectReplyBody composes a WeCom direct-chat reply with dedicated header, optional preview link,
// and persistent bottom footer. When content exceeds maxBytes, only the middle answer text is trimmed
// so the header, preview link, and bottom identity footer always survive intact.
func formatDirectReplyBody(title, workerName, machineLabel, fullText, previewURL, quotaError string, inFocusMode bool, currentFocusWorker string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = directReplyMaxBytes
	}
	header := formatDirectHeader(title, workerName, machineLabel)
	footer := formatDirectFooter(title, workerName, inFocusMode, currentFocusWorker)

	var tailParts []string
	if quotaError == "" && strings.TrimSpace(previewURL) != "" {
		tailParts = append(tailParts, "🔗 [在线预览 · 完整报告]("+strings.TrimSpace(previewURL)+")")
	}
	if footer != "" {
		tailParts = append(tailParts, footer)
	}
	tail := strings.Join(tailParts, "\n\n")

	var mainText string
	if strings.TrimSpace(quotaError) != "" {
		mainText = "⚠ Worker 遇到模型/配额错误：" + strings.TrimSpace(quotaError)
	} else {
		mainText = formatMarkdownForWeCom(fullText, previewURL)
		if mainText == "" {
			mainText = "已完成本轮处理。"
		}
	}

	const clipNotice = "\n\n…（内容过长，完整报告见预览链接或附件）"
	overhead := len(header) + len("\n\n")
	if tail != "" {
		overhead += len("\n\n") + len(tail)
	}
	avail := maxBytes - overhead
	if avail > 0 && len(mainText) > avail {
		if avail > len(clipNotice) {
			mainText = strings.TrimRight(trimUTF8Bytes(mainText, avail-len(clipNotice)), " \t\n") + clipNotice
		} else {
			mainText = trimUTF8Bytes(mainText, avail)
		}
	}

	if tail == "" {
		return header + "\n\n" + mainText
	}
	return header + "\n\n" + mainText + "\n\n" + tail
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

func (b *Bus) formatWeComHelp(ctx context.Context, focusKey string) string {
	status := "Corgi 主对话"
	if f := b.loadWeComFocus(ctx, focusKey); f != nil {
		label := firstNonEmptyStr(f.Title, "#"+f.Worker)
		if label == "#"+f.Worker || strings.EqualFold(label, f.Worker) {
			status = fmt.Sprintf("直聊中 · `#%s`", f.Worker)
		} else {
			status = fmt.Sprintf("直聊中 · %s（`#%s`）", label, f.Worker)
		}
	}
	return fmt.Sprintf(
		"**当前对话**：%s\n\n"+
			"**快捷命令**\n"+
			"- `/d`：查看所有待拍板事项并唤起拍板卡（`/d <编号>` 唤起指定项）\n"+
			"- `/w`：弹出事项卡片切换直聊（也支持 `/w 1` 或 `/w 关键词`）\n"+
			"- `/ctx`：查看大脑上下文占用与缓存命中（`/ctx compact` 压缩 · `/ctx clear` 清空重开）\n"+
			"- `/back`：返回 Corgi 主对话\n"+
			"- `/help`：查看当前状态与命令\n\n"+
			"提示：长按引用带 `↳ 直聊` 标记的消息可单次追问该会话；直聊中长按引用 Corgi 回复可单次问回 Corgi 而不退出当前直聊。",
		status,
	)
}

func (b *Bus) handleWeComControlOrDirect(ctx context.Context, msg wecom.IncomingMessage, thread, text string, mediaRefs []model.MediaRef) bool {
	focusKey := wecomFocusKey(msg.ChatType, msg.ChatID, msg.FromUser)
	replyChatID := msg.ChatID
	if replyChatID == "" {
		replyChatID = msg.FromUser
	}
	b.clearPendingFocusNotice(thread)

	// 0. Instant help & current conversation status.
	if isHelpCommand(text) {
		b.sendInstantWeComText(ctx, replyChatID, msg.ReqID, b.formatWeComHelp(ctx, focusKey))
		return true
	}
	if b.handleWeComContextCommand(ctx, replyChatID, msg.ReqID, thread, text) {
		return true
	}

	// 0.5 Pending real decision list & on-demand card re-surface (`/d`, `/d <编号>`, `待拍板`).
	if q, ok := parseDecisionListCommand(text); ok {
		b.handleWeComDecisionListCommand(ctx, thread, replyChatID, msg.ReqID, q)
		return true
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
			b.sendInstantWeComText(ctx, replyChatID, msg.ReqID, "目前还没有可切换的事项或会话，直接在 Corgi 主对话派活即可。")
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

	// 3. Explicit `/w <编号|名称|关键词>` switch.
	if query, ok := parseDirectSwitchTarget(text); ok {
		workerName, title := b.resolveSwitchQuery(ctx, query)
		b.switchWeComFocusTo(ctx, focusKey, replyChatID, msg.ReqID, workerName, title, nil)
		return true
	}

	// 4. Native quote containing dedicated direct-chat marker (`↳ 直聊` / `↳ 直聊中` / `↳ 单次直问`).
	// Never triggers on Corgi reports or legacy `[#worker]` tags.
	if tag := core.ExtractQuoteWorkerTag(msg.QuoteText); tag != "" {
		curFocus := b.loadWeComFocus(ctx, focusKey)
		if curFocus != nil && strings.EqualFold(curFocus.Worker, tag) {
			b.dispatchWeComDirectWorker(ctx, msg, thread, replyChatID, focusKey, *curFocus, text, mediaRefs, true, curFocus.Worker)
			return true
		}
		curWorker := ""
		if curFocus != nil {
			curWorker = curFocus.Worker
		}
		b.dispatchWeComDirectWorker(ctx, msg, thread, replyChatID, focusKey, wecomFocus{Worker: tag}, text, mediaRefs, false, curWorker)
		return true
	}

	// 5. Active Worker focus mode with built-in 60m TTL & liveness checks.
	loadRes := b.loadWeComFocusResult(ctx, focusKey)
	if loadRes.Expired {
		switch loadRes.ExpiredReason {
		case "ttl":
			b.setPendingFocusNotice(thread, fmt.Sprintf("> 已超过 %s 未直聊，已自动回到 Corgi 主对话（原直聊 #%s）", formatFocusTTLShort(loadRes.TTL), loadRes.ExpiredWorker))
		case "ended":
			b.setPendingFocusNotice(thread, fmt.Sprintf("> 原直聊会话 #%s 已结束，已自动回到 Corgi 主对话", loadRes.ExpiredWorker))
		}
		return false
	}
	if focus := loadRes.Focus; focus != nil {
		// Quoting any Corgi reply (plain or worker-referenced report) routes this single turn
		// back to Corgi Router without losing the current direct-chat focus.
		if strings.TrimSpace(msg.QuoteText) != "" && b.engine != nil && b.engine.MatchesMainChatCorgiReply(ctx, thread, msg.QuoteText) {
			return false
		}
		b.dispatchWeComDirectWorker(ctx, msg, thread, replyChatID, focusKey, *focus, text, mediaRefs, true, focus.Worker)
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
	actionVerb := "已切入直聊"
	if resumed {
		actionVerb = "已恢复并切入直聊"
	}
	cardDetail := fmt.Sprintf("%s · %s · 直接发消息即可，60 分钟未直聊自动回 Corgi。", machineLabel, w.Kind)
	if cardEv != nil && b.wecom != nil {
		if err := b.wecom.UpdateDirectFocusCard(ctx, cardEv.ReqID, cardEv.TaskID, actionVerb+" · "+label, w.Name, cardDetail); err != nil {
			b.logger.Warn("update wecom direct focus card failed", "worker", w.Name, "err", err)
		}
		return
	}
	if b.wecom != nil && b.wecom.Online() {
		if err := b.wecom.SendDirectFocusCard(ctx, chatID, actionVerb+" · "+label, w.Name, cardDetail); err == nil {
			return
		} else {
			b.logger.Warn("send wecom direct focus card failed", "worker", w.Name, "err", err)
		}
	}
	msg := fmt.Sprintf("%s：**%s**（`#%s` · %s · %s）。直接发消息即可，发 `/back` 回 Corgi 主对话。", actionVerb, label, w.Name, machineLabel, w.Kind)
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
			_ = b.wecom.UpdateNavCard(ctx, ev.ReqID, ev.TaskID, "已回到 Corgi 主对话", "已回 Corgi", "后续消息将发给 Corgi 主对话。")
		}
		return true
	}
	if ev.ChoiceKey == "picker" {
		curWorker, curTitle := "", ""
		if f := b.loadWeComFocus(ctx, focusKey); f != nil {
			curWorker, curTitle = f.Worker, f.Title
		}
		_, opts := b.selectableWeComOptions(ctx)
		if b.wecom != nil {
			if err := b.wecom.UpdateWorkerPickerCard(ctx, ev.ReqID, ev.TaskID, curWorker, curTitle, opts, 0); err != nil {
				b.logger.Warn("open wecom worker picker from focus card failed", "err", err)
			}
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

func (b *Bus) dispatchWeComDirectWorker(ctx context.Context, msg wecom.IncomingMessage, thread, chatID, focusKey string, target wecomFocus, text string, mediaRefs []model.MediaRef, inFocusMode bool, currentFocusWorker string) {
	// Open passive stream immediately for instant WeCom "思考中..." feedback, then start watching
	// once SendDirectWorkerMessage has resolved/resumed the exact live Worker pane.
	stream, watchCtx := b.openWorkerStream(ctx, thread, chatID, msg.ReqID, target.Worker, target.Title, inFocusMode, currentFocusWorker)

	onDone := func(cbCtx context.Context, res core.DirectTurnResult) {
		if inFocusMode && b.engine != nil {
			b.engine.TouchWeComFocus(cbCtx, focusKey, res.WorkerName)
		}
		body := formatDirectReplyBody(
			target.Title,
			res.WorkerName,
			res.MachineLabel,
			res.FullText,
			res.PreviewURL,
			res.QuotaError,
			inFocusMode,
			currentFocusWorker,
			directReplyMaxBytes,
		)

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
			if utf8.RuneCountInString(res.FullText) > 1800 || len(res.FullText) > directReplyMaxBytes-1024 {
				fname := fmt.Sprintf("%s-%s.md", res.WorkerName, time.Now().Format("150405"))
				if err := b.sendFileFn(cbCtx, chatID, fname, []byte(res.FullText)); err != nil {
					b.logger.Warn("send wecom direct worker report file failed", "worker", res.WorkerName, "err", err)
				}
			}
		}
	}

	promptText := formatWeComDirectPrompt(msg.QuoteText, text)

	w, _, err := b.engine.SendDirectWorkerMessage(ctx, core.DirectWorkerRequest{
		Name:       target.Worker,
		Machine:    target.Machine,
		Session:    target.Session,
		Kind:       target.Kind,
		WorkItemID: target.WorkItemID,
		Text:       promptText,
		Media:      mediaRefs,
		OnDone:     onDone,
	})
	if err != nil {
		errText := fmt.Sprintf("发送给「%s」失败：%s", firstNonEmptyStr(target.Title, "#"+target.Worker), err.Error())
		if inFocusMode {
			errText += "\n可发 `/back` 返回 Corgi 主对话，或发 `/w` 切换其他事项。"
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

	// Keep saved focus metadata and UpdatedAt fresh (e.g. newly assigned session ID or WorkItemID).
	if inFocusMode && w != nil {
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

func formatWeComDirectPrompt(quoteText, text string) string {
	if q := strings.Join(strings.Fields(strings.TrimSpace(quoteText)), " "); q != "" {
		r := []rune(q)
		if len(r) > 240 {
			q = string(r[:240]) + "…"
		}
		return fmt.Sprintf("[引用：%s]\n%s", q, text)
	}
	return text
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

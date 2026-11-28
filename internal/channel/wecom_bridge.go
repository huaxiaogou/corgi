package channel

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
)

// OnUserMessage bridges messages typed in Web back to WeCom:
//   - WeCom group threads (`wecom:*`) mirror immediately as `**[Web]** ...` to that group.
//   - Main single-chat thread (`chat:main`) does NOT send a standalone left-side bubble; instead it
//     records a compact summary so Corgi's first WeCom reply prepends a native grey `> 来自 Web：...`
//     blockquote line (zero extra bubbles or duplicate notifications).
//
// Action-linked decision entries are skipped here because OnActionResolved pushes a structured sync receipt.
func (b *Bus) OnUserMessage(ctx context.Context, entry *model.Entry) {
	if entry == nil || entry.Channel != "web" || entry.ActionID != "" {
		return
	}
	if entry.Thread == model.MainThread {
		if summary := formatWebQuoteSummary(entry.Text, 140); summary != "" {
			b.setPendingWebQuote(entry.Thread, summary)
		}
		return
	}
	if !strings.HasPrefix(entry.Thread, "wecom:") || !b.canSendMarkdown() {
		return
	}
	chatID := threadToWeComChat(entry.Thread)
	if chatID == "" {
		return
	}
	body := formatWebMirrorMessage(entry.Text)
	if body == "" {
		return
	}
	if err := b.sendFn(ctx, chatID, body); err != nil {
		b.logger.Warn("mirror web group message to wecom failed", "chat_id", chatID, "err", err)
	}
}

func (b *Bus) setPendingWebQuote(thread, summary string) {
	if thread == "" || summary == "" {
		return
	}
	b.mu.Lock()
	if b.pendingWebQuote == nil {
		b.pendingWebQuote = map[string]string{}
	}
	b.pendingWebQuote[thread] = summary
	b.mu.Unlock()
}

func (b *Bus) takePendingWebQuote(thread string) string {
	if thread == "" {
		return ""
	}
	b.mu.Lock()
	q := b.pendingWebQuote[thread]
	delete(b.pendingWebQuote, thread)
	b.mu.Unlock()
	return q
}

func (b *Bus) clearPendingWebQuote(thread string) {
	if thread == "" {
		return
	}
	b.mu.Lock()
	delete(b.pendingWebQuote, thread)
	b.mu.Unlock()
}

func (b *Bus) setPendingFocusNotice(thread, notice string) {
	notice = strings.TrimSpace(notice)
	if thread == "" || notice == "" {
		return
	}
	b.mu.Lock()
	if b.pendingFocusNotice == nil {
		b.pendingFocusNotice = map[string]string{}
	}
	b.pendingFocusNotice[thread] = notice
	b.mu.Unlock()
}

func (b *Bus) takePendingFocusNotice(thread string) string {
	if thread == "" {
		return ""
	}
	b.mu.Lock()
	n := b.pendingFocusNotice[thread]
	delete(b.pendingFocusNotice, thread)
	b.mu.Unlock()
	return n
}

func (b *Bus) clearPendingFocusNotice(thread string) {
	if thread == "" {
		return
	}
	b.mu.Lock()
	delete(b.pendingFocusNotice, thread)
	b.mu.Unlock()
}

func formatWebQuoteSummary(raw string, maxRunes int) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if maxRunes <= 0 {
		maxRunes = 140
	}
	mainText := raw
	var attachNames []string
	if idx := strings.Index(raw, "\n\n附件：\n"); idx >= 0 {
		mainText = strings.TrimSpace(raw[:idx])
		attachBlock := raw[idx+len("\n\n附件：\n"):]
		for _, line := range strings.Split(attachBlock, "\n") {
			p := strings.TrimSpace(line)
			if p == "" {
				continue
			}
			base := filepath.Base(p)
			if base != "" && base != "." && base != "/" {
				attachNames = append(attachNames, base)
			}
		}
	}
	mainText = strings.Join(strings.Fields(mainText), " ")
	if r := []rune(mainText); len(r) > maxRunes {
		mainText = string(r[:maxRunes]) + "…"
	}
	attachSuffix := ""
	if len(attachNames) > 0 {
		if len(attachNames) > 3 {
			attachNames = append(attachNames[:3], "等")
		}
		attachSuffix = "（附件：" + strings.Join(attachNames, "、") + "）"
	}
	if mainText == "" && attachSuffix != "" {
		return "发送了" + strings.TrimPrefix(strings.TrimSuffix(attachSuffix, "）"), "（")
	}
	return mainText + attachSuffix
}

func formatWebMirrorMessage(raw string) string {
	summary := formatWebQuoteSummary(raw, 240)
	if summary == "" {
		return ""
	}
	return "**[Web]** " + summary
}

func prependReplyHeaders(webQuote, focusNotice, body string) string {
	var headers []string
	if q := strings.TrimSpace(webQuote); q != "" {
		headers = append(headers, "> 来自 Web："+q)
	}
	if n := strings.TrimSpace(focusNotice); n != "" {
		headers = append(headers, n)
	}
	if len(headers) == 0 {
		return body
	}
	head := strings.Join(headers, "\n")
	if strings.TrimSpace(body) == "" {
		return head
	}
	return head + "\n\n" + body
}

func (b *Bus) OnActionCreated(ctx context.Context, action *model.Action) {
	if action == nil || !action.IsDecision() || b.wecom == nil || !b.wecom.Online() {
		return
	}
	if suppress, _ := b.wecomDeliveryState(action.Thread, time.Now()); suppress {
		return
	}
	chatID := threadToWeComChat(action.Thread)
	_ = b.pushWeComDecisionCard(ctx, action.Thread, chatID, action, false)
}

// OnActionResolved synchronizes non-WeCom action resolutions (Web, CLI, Router, dismiss, snooze)
// back to WeCom via a compact status receipt ONLY when the original card was actually pushed to WeCom.
func (b *Bus) OnActionResolved(ctx context.Context, action *model.Action) {
	if action == nil || !action.PushedToWeCom {
		return
	}
	if strings.EqualFold(strings.TrimSpace(action.ResolvedBy), "wecom") || strings.EqualFold(strings.TrimSpace(action.ResolvedChannel), "wecom") {
		return
	}
	if action.Status == model.ActionExpired && strings.EqualFold(strings.TrimSpace(action.ResolvedBy), "expired") {
		return
	}
	if !b.canSendMarkdown() {
		return
	}
	body := wecom.FormatActionResolvedMarkdown(action)
	if body == "" {
		return
	}
	b.mu.Lock()
	send := b.sendFn
	b.mu.Unlock()
	if send == nil {
		return
	}
	chatID := threadToWeComChat(action.Thread)
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := send(sctx, chatID, body); err != nil {
		b.logger.Warn("sync resolved action status to wecom failed", "action", action.ID, "status", action.Status, "err", err)
	}
}

// HandleWeComMessage bridges an allowlisted WeCom message (and any media attachments) into Engine.Say.
func (b *Bus) HandleWeComMessage(ctx context.Context, msg wecom.IncomingMessage) {
	now := time.Now()
	b.mu.Lock()
	b.wecomActiveAt = now
	uploadDir := b.uploadDir
	if id := strings.TrimSpace(msg.MsgID); id != "" {
		if b.seenWeComMsgs == nil {
			b.seenWeComMsgs = map[string]time.Time{}
		}
		if _, dup := b.seenWeComMsgs[id]; dup {
			b.mu.Unlock()
			return
		}
		b.seenWeComMsgs[id] = now
		if len(b.seenWeComMsgs) > 128 {
			for k, ts := range b.seenWeComMsgs {
				if now.Sub(ts) > 10*time.Minute {
					delete(b.seenWeComMsgs, k)
				}
			}
		}
	}
	b.mu.Unlock()

	text := msg.Text
	var mediaRefs []model.MediaRef
	if len(msg.Attachments) > 0 && uploadDir != "" {
		var saved []string
		for _, att := range msg.Attachments {
			p, err := wecom.DownloadMedia(ctx, att, uploadDir)
			if err != nil {
				b.logger.Warn("download wecom media failed", "type", att.Type, "err", err)
				continue
			}
			saved = append(saved, p)
			mediaRefs = append(mediaRefs, model.MediaRef{
				Path: p,
				Name: filepath.Base(p),
			})
		}
		if len(saved) > 0 {
			if text != "" {
				text += "\n\n"
			}
			text += "附件：\n" + strings.Join(saved, "\n")
		}
	}
	if strings.TrimSpace(text) == "" {
		return
	}

	thread := model.MainThread
	if msg.ChatType == "group" && msg.ChatID != "" {
		thread = "wecom:" + msg.ChatID
	}

	if b.handleWeComControlOrDirect(ctx, msg, thread, text, mediaRefs) {
		return
	}
	b.clearPendingWebQuote(thread)

	replyChatID := msg.ChatID
	if replyChatID == "" {
		replyChatID = msg.FromUser
	}
	st := b.startRouterStream(ctx, thread, replyChatID, msg.ReqID)
	en, trig, err := b.engine.Say(ctx, core.SayRequest{
		Thread:    thread,
		Channel:   "wecom",
		From:      msg.FromUser,
		Text:      text,
		QuoteText: msg.QuoteText,
		Media:     mediaRefs,
		IdemKey:   "wecom:" + msg.MsgID,
		OnCommitted: func(committedEntry *model.Entry, committedTrig *model.Trigger) {
			if st == nil {
				return
			}
			entryID, trigID := "", ""
			if committedEntry != nil {
				entryID = committedEntry.ID
			}
			if committedTrig != nil {
				trigID = committedTrig.ID
			}
			st.setRefIDs(entryID, trigID)
		},
	})
	if err != nil {
		b.logger.Warn("ingest wecom message failed", "msgid", msg.MsgID, "err", err)
		if st != nil {
			b.clearRouterStreamIfMatch(st)
			_, _ = st.seal(ctx, "", "接收消息失败："+err.Error())
		}
		return
	}
	if st != nil && st.getTriggerID() == "" {
		entryID, trigID := "", ""
		if en != nil {
			entryID = en.ID
		}
		if trig != nil {
			trigID = trig.ID
		}
		st.setRefIDs(entryID, trigID)
	}
}

// HandleWeComCardClick resolves the target Action atomically and updates the WeCom card in-place.
// Navigation cards (Worker picker / return to main) are intercepted before Action resolution.
// If the Action was already resolved or expired elsewhere, it still refreshes the stale WeCom card.
func (b *Bus) HandleWeComCardClick(ctx context.Context, ev wecom.CardClickEvent) {
	b.mu.Lock()
	b.wecomActiveAt = time.Now()
	b.mu.Unlock()

	if b.handleNavCardClick(ctx, ev) {
		return
	}
	b.handleDecisionCardClick(ctx, ev)
}

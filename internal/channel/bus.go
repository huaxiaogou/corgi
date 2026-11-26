package channel

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
)

var mdReportPathRe = regexp.MustCompile(`((?:~/|/[\w.-]+/)[\w./-]+\.md)\b`)

type outboxItem struct {
	text string
	at   time.Time
}

// Bus implements core.ChannelSink, fanning out state changes to Web SSE subscribers
// and pacing outbound notifications, report files, and cards to the WeCom Smart Bot connection.
type Bus struct {
	engine     *core.Engine
	wecom      *wecom.Client
	logger     *slog.Logger
	flushDelay time.Duration
	uploadDir  string
	home       string
	sendFn     func(ctx context.Context, chatID, markdown string) error
	sendFileFn func(ctx context.Context, chatID, filename string, data []byte) error

	mu            sync.Mutex
	subs          map[chan struct{}]struct{}
	outbox        map[string][]outboxItem
	webSeenAt     time.Time
	wecomActiveAt time.Time
}

// NewBus wires the Super-Agent Engine to Web SSE subscribers and the WeCom client.
func NewBus(eng *core.Engine, wc *wecom.Client, logger *slog.Logger) *Bus {
	if logger == nil {
		logger = slog.Default()
	}
	b := &Bus{
		engine:     eng,
		wecom:      wc,
		logger:     logger,
		flushDelay: 20 * time.Second,
		subs:       map[chan struct{}]struct{}{},
		outbox:     map[string][]outboxItem{},
	}
	if wc != nil {
		b.sendFn = wc.SendMarkdown
		b.sendFileFn = wc.SendFile
	}
	eng.SetSink(b)
	return b
}

// SetPaths configures local directories for inbound WeCom media downloads and report file lookups.
func (b *Bus) SetPaths(uploadDir, home string) {
	b.mu.Lock()
	b.uploadDir = uploadDir
	b.home = home
	b.mu.Unlock()
}

// Run periodically flushes paced outbox batches until ctx is canceled.
func (b *Bus) Run(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			b.FlushDue(fctx, true)
			cancel()
			return
		case <-tick.C:
			b.FlushDue(ctx, false)
		}
	}
}

// RecordWebSeen marks the Web console as actively visible so quiet-mode Outbox can
// suppress redundant single-chat WeCom notifications while the user is looking at Web.
func (b *Bus) RecordWebSeen() {
	b.mu.Lock()
	b.webSeenAt = time.Now()
	b.mu.Unlock()
}

// Subscribe registers a Web SSE listener notified whenever state changes.
func (b *Bus) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

func (b *Bus) OnStateChanged() {
	b.mu.Lock()
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	b.mu.Unlock()
}

func (b *Bus) WeComOnline() bool {
	return b.wecom != nil && b.wecom.Online()
}

func (b *Bus) canSendMarkdown() bool {
	if b.sendFn == nil {
		return false
	}
	if b.wecom != nil && !b.wecom.Online() {
		return false
	}
	return true
}

// OnUserMessage mirrors messages typed in a Web WeCom-group channel back to that WeCom group.
// Action-linked decision entries are skipped here because OnActionResolved pushes a structured sync receipt.
func (b *Bus) OnUserMessage(ctx context.Context, entry *model.Entry) {
	if entry == nil || entry.Channel != "web" || entry.ActionID != "" || !strings.HasPrefix(entry.Thread, "wecom:") {
		return
	}
	if !b.canSendMarkdown() {
		return
	}
	chatID := threadToWeComChat(entry.Thread)
	if chatID == "" {
		return
	}
	if err := b.sendFn(ctx, chatID, "**[Web]** "+entry.Text); err != nil {
		b.logger.Warn("mirror web group message to wecom failed", "chat_id", chatID, "err", err)
	}
}

func (b *Bus) OnReply(ctx context.Context, entry *model.Entry, action *model.Action) {
	chatID := threadToWeComChat(entry.Thread)

	// Decision cards (--opt / --check) bypass Outbox batching and push immediately.
	if action != nil && len(action.Choices) > 0 {
		if b.wecom != nil && b.wecom.Online() {
			if err := b.wecom.SendActionCard(ctx, chatID, action); err != nil {
				b.logger.Warn("push action card to wecom failed", "action", action.ID, "err", err)
			}
		}
		return
	}

	if !b.canSendMarkdown() {
		return
	}

	// Quiet mode suppresses redundant main-thread pushes while user is actively viewing Web,
	// unless they recently initiated conversation from WeCom.
	notifyLevel := "quiet"
	if b.engine != nil && b.engine.Settings().NotifyLevel == "normal" {
		notifyLevel = "normal"
	}
	now := time.Now()
	b.mu.Lock()
	webActive := !b.webSeenAt.IsZero() && now.Sub(b.webSeenAt) < 60*time.Second
	wecomRecent := !b.wecomActiveAt.IsZero() && now.Sub(b.wecomActiveAt) < 5*time.Minute
	if notifyLevel == "quiet" && entry.Thread == model.MainThread && webActive && !wecomRecent {
		b.mu.Unlock()
		return
	}
	body := formatWeComReply(entry)
	if wecomRecent {
		// Active WeCom conversation: deliver immediately instead of holding 20s in Outbox.
		send := b.sendFn
		b.mu.Unlock()
		if err := send(ctx, chatID, body); err != nil {
			b.logger.Warn("immediate wecom reply failed", "chat_id", chatID, "err", err)
		}
	} else {
		b.outbox[chatID] = append(b.outbox[chatID], outboxItem{text: body, at: now})
		b.mu.Unlock()
	}

	// Offline-readable report delivery: push referenced .md report files or very long replies as attachments.
	b.maybeSendReportAttachment(ctx, chatID, entry.Text)
}

func formatWeComReply(entry *model.Entry) string {
	txt := strings.TrimSpace(entry.Text)
	if !strings.HasPrefix(entry.Thread, "wecom:") || strings.HasPrefix(txt, "[#") {
		return txt
	}
	tag := ""
	if entry.Re != nil && entry.Re.Worker != "" {
		tag = entry.Re.Worker
	} else if entry.Ref != "" && !strings.Contains(entry.Ref, ":") {
		tag = entry.Ref
	}
	if tag != "" {
		return "[#" + tag + "] " + txt
	}
	return txt
}

func (b *Bus) maybeSendReportAttachment(ctx context.Context, chatID, text string) {
	if b.sendFileFn == nil || (b.wecom != nil && !b.wecom.Online()) {
		return
	}
	b.mu.Lock()
	home := b.home
	b.mu.Unlock()

	// 1. If reply references a local .md report path (≤ 5 MB), send it as a file attachment.
	if m := mdReportPathRe.FindStringSubmatch(text); len(m) >= 2 {
		p := m[1]
		if strings.HasPrefix(p, "~/") && home != "" {
			p = filepath.Join(home, p[2:])
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Size() > 0 && fi.Size() <= 5<<20 {
			if data, err := os.ReadFile(p); err == nil {
				if err := b.sendFileFn(ctx, chatID, filepath.Base(p), data); err != nil {
					b.logger.Warn("push markdown report file to wecom failed", "path", p, "err", err)
				}
				return
			}
		}
	}

	// 2. If reply body itself is very long (>1800 runes), also attach as an offline-readable .md file.
	if len([]rune(text)) > 1800 {
		name := "corgi-report-" + time.Now().Format("150405") + ".md"
		if err := b.sendFileFn(ctx, chatID, name, []byte(text)); err != nil {
			b.logger.Warn("push long reply attachment to wecom failed", "err", err)
		}
	}
}

// FlushDue delivers coalesced Outbox messages whose buffer window has elapsed (or all when force=true).
func (b *Bus) FlushDue(ctx context.Context, force bool) {
	if !b.canSendMarkdown() {
		return
	}
	now := time.Now()
	ready := map[string]string{}

	b.mu.Lock()
	for chatID, items := range b.outbox {
		if len(items) == 0 {
			delete(b.outbox, chatID)
			continue
		}
		if !force && now.Sub(items[0].at) < b.flushDelay {
			continue
		}
		parts := make([]string, 0, len(items))
		for _, it := range items {
			if t := strings.TrimSpace(it.text); t != "" {
				parts = append(parts, t)
			}
		}
		delete(b.outbox, chatID)
		if len(parts) > 0 {
			ready[chatID] = strings.Join(parts, "\n\n---\n\n")
		}
	}
	send := b.sendFn
	b.mu.Unlock()

	for chatID, body := range ready {
		if err := send(ctx, chatID, body); err != nil {
			b.logger.Warn("flush outbox markdown failed", "chat_id", chatID, "err", err)
		}
	}
}

func (b *Bus) OnActionCreated(ctx context.Context, action *model.Action) {
	if b.wecom == nil || !b.wecom.Online() {
		return
	}
	chatID := threadToWeComChat(action.Thread)
	if err := b.wecom.SendActionCard(ctx, chatID, action); err != nil {
		b.logger.Warn("push worker action card to wecom failed", "action", action.ID, "err", err)
	}
}

// OnActionResolved synchronizes non-WeCom action resolutions (Web, CLI, Router, dismiss, snooze)
// back to WeCom immediately via a compact status receipt, bypassing quiet-mode suppression and Outbox
// batching to match SendActionCard delivery. WeCom Smart Bot WSS only permits in-place card mutation
// (`aibot_respond_update_msg`) within 5s of an inbound button-click callback req_id; if the user later
// clicks the stale card, HandleWeComCardClick still refreshes it in-place without double-executing.
func (b *Bus) OnActionResolved(ctx context.Context, action *model.Action) {
	if action == nil || strings.EqualFold(strings.TrimSpace(action.ResolvedBy), "wecom") {
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
	b.mu.Lock()
	b.wecomActiveAt = time.Now()
	uploadDir := b.uploadDir
	b.mu.Unlock()

	text := msg.Text
	if len(msg.Attachments) > 0 && uploadDir != "" {
		var saved []string
		for _, att := range msg.Attachments {
			p, err := wecom.DownloadMedia(ctx, att, uploadDir)
			if err != nil {
				b.logger.Warn("download wecom media failed", "type", att.Type, "err", err)
				continue
			}
			saved = append(saved, p)
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
	_, _, err := b.engine.Say(ctx, core.SayRequest{
		Thread:    thread,
		Channel:   "wecom",
		From:      msg.FromUser,
		Text:      text,
		QuoteText: msg.QuoteText,
		IdemKey:   "wecom:" + msg.MsgID,
	})
	if err != nil {
		b.logger.Warn("ingest wecom message failed", "msgid", msg.MsgID, "err", err)
	}
}

// HandleWeComCardClick resolves the target Action atomically and updates the WeCom card in-place.
// If the Action was already resolved or expired elsewhere, it still refreshes the stale WeCom card.
func (b *Bus) HandleWeComCardClick(ctx context.Context, ev wecom.CardClickEvent) {
	b.mu.Lock()
	b.wecomActiveAt = time.Now()
	b.mu.Unlock()

	act, err := b.engine.ResolveAction(ctx, ev.ActionID, "wecom", ev.ChoiceKey)
	if err != nil {
		if errors.Is(err, store.ErrActionAlreadyResolved) {
			existing, gErr := b.engine.GetAction(ctx, ev.ActionID)
			if gErr != nil || existing == nil {
				b.logger.Info("wecom card click ignored (already resolved)", "action", ev.ActionID, "err", gErr)
				return
			}
			act = existing
			b.logger.Info("refreshing already-resolved wecom action card", "action", act.ID, "status", act.Status, "resolved_by", act.ResolvedBy)
		} else {
			b.logger.Warn("wecom card click failed", "action", ev.ActionID, "choice", ev.ChoiceKey, "err", err)
			return
		}
	} else {
		b.logger.Info("resolved action from wecom card click", "action", act.ID, "choice_key", act.ChosenKey, "choice_label", act.ChosenLabel)
	}
	if b.wecom != nil {
		if err := b.wecom.UpdateCardResolvedWithTask(ctx, ev.ReqID, ev.TaskID, act); err != nil {
			b.logger.Warn("update wecom template card failed", "action", act.ID, "req_id", ev.ReqID, "err", err)
		}
	}
}

func threadToWeComChat(thread string) string {
	if chat, ok := strings.CutPrefix(thread, "wecom:"); ok {
		return chat
	}
	return ""
}

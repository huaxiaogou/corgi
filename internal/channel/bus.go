package channel

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
)

type outboxItem struct {
	text          string
	at            time.Time
	attempts      int
	immediate     bool
	useACK        bool
	hasFiles      bool
	entry         *model.Entry
	attachments   []core.AttachmentFile
	allowLongFile bool
}

// Bus implements core.ChannelSink, fanning out state changes to Web SSE subscribers
// and pacing outbound notifications, report files, and cards to the WeCom Smart Bot connection.
type Bus struct {
	engine          *core.Engine
	wecom           *wecom.Client
	logger          *slog.Logger
	flushDelay      time.Duration
	uploadDir       string
	home            string
	sendFn          func(ctx context.Context, chatID, markdown string) error
	sendMarkdownACK func(ctx context.Context, chatID, markdown string) error
	sendFileFn      func(ctx context.Context, chatID, filename string, data []byte) error
	sendStreamFn    wecom.StreamSendFunc

	mu                sync.Mutex
	flushMu           sync.Mutex
	chatSendMu        map[string]*sync.Mutex
	subs              map[chan struct{}]struct{}
	outbox            map[string][]outboxItem
	routerStreams     map[string][]*activeWeComStream
	lastRouterReplyAt map[string]time.Time
	seenWeComMsgs     map[string]time.Time
	pendingWebQuote   map[string]string
	webSeenAt         time.Time
	wecomActiveAt     time.Time
}

// NewBus wires the Super-Agent Engine to Web SSE subscribers and the WeCom client.
func NewBus(eng *core.Engine, wc *wecom.Client, logger *slog.Logger) *Bus {
	if logger == nil {
		logger = slog.Default()
	}
	b := &Bus{
		engine:            eng,
		wecom:             wc,
		logger:            logger,
		flushDelay:        20 * time.Second,
		chatSendMu:        map[string]*sync.Mutex{},
		subs:              map[chan struct{}]struct{}{},
		outbox:            map[string][]outboxItem{},
		routerStreams:     map[string][]*activeWeComStream{},
		lastRouterReplyAt: map[string]time.Time{},
		seenWeComMsgs:     map[string]time.Time{},
		pendingWebQuote:   map[string]string{},
	}
	if wc != nil {
		b.sendFn = wc.SendMarkdown
		b.sendMarkdownACK = wc.SendMarkdownACK
		b.sendFileFn = wc.SendFile
		b.sendStreamFn = wc.SendStreamReply
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

func (b *Bus) wecomTransportOnline() bool {
	if b.wecom != nil {
		return b.wecom.Online()
	}
	return b.sendFn != nil || b.sendStreamFn != nil
}

func (b *Bus) chatSendLock(chatID string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.chatSendMu == nil {
		b.chatSendMu = map[string]*sync.Mutex{}
	}
	mu := b.chatSendMu[chatID]
	if mu == nil {
		mu = &sync.Mutex{}
		b.chatSendMu[chatID] = mu
	}
	return mu
}

func (b *Bus) canonicalWeComChat(thread, streamChatID string) string {
	chatID := threadToWeComChat(thread)
	if chatID == "" && strings.TrimSpace(streamChatID) != "" {
		chatID = strings.TrimSpace(streamChatID)
	}
	if resolved := b.resolveWeComChat(chatID); resolved != "" {
		return resolved
	}
	return chatID
}

func (b *Bus) hasOutboxFor(chatID string) bool {
	b.mu.Lock()
	n := len(b.outbox[chatID])
	b.mu.Unlock()
	return n > 0
}

func (b *Bus) enqueueOutboxTail(chatID string, item outboxItem) {
	b.mu.Lock()
	if b.outbox == nil {
		b.outbox = map[string][]outboxItem{}
	}
	b.outbox[chatID] = append(b.outbox[chatID], item)
	b.mu.Unlock()
}

func (b *Bus) enqueueOutboxHead(chatID string, items ...outboxItem) {
	if len(items) == 0 {
		return
	}
	b.mu.Lock()
	if b.outbox == nil {
		b.outbox = map[string][]outboxItem{}
	}
	b.outbox[chatID] = append(items, b.outbox[chatID]...)
	b.mu.Unlock()
}

func isUnknownDeliveryErr(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// OnUserMessage bridges messages typed in Web back to WeCom:
// - WeCom group threads (`wecom:*`) mirror immediately as `**[Web]** ...` to that group.
// - Main single-chat thread (`chat:main`) does NOT send a standalone left-side bubble; instead it
//   records a compact summary so Corgi's first WeCom reply prepends a native grey `> 来自 Web：...`
//   blockquote line (zero extra bubbles or duplicate notifications).
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

func prependWebQuoteHeader(webQuote, body string) string {
	q := strings.TrimSpace(webQuote)
	if q == "" {
		return body
	}
	header := "> 来自 Web：" + q
	if strings.TrimSpace(body) == "" {
		return header
	}
	return header + "\n\n" + body
}

func (b *Bus) notifyLevel() string {
	if b.engine != nil && b.engine.Settings().NotifyLevel == "normal" {
		return "normal"
	}
	return "quiet"
}

func (b *Bus) wecomDeliveryState(thread string, now time.Time) (suppress bool, wecomRecent bool) {
	level := b.notifyLevel()
	b.mu.Lock()
	webActive := !b.webSeenAt.IsZero() && now.Sub(b.webSeenAt) < 75*time.Second
	wecomRecent = !b.wecomActiveAt.IsZero() && now.Sub(b.wecomActiveAt) < 5*time.Minute
	b.mu.Unlock()
	suppress = level == "quiet" && thread == model.MainThread && webActive && !wecomRecent
	return suppress, wecomRecent
}

func (b *Bus) resolveWeComChat(chatID string) string {
	if b.wecom != nil {
		return b.wecom.ResolveTarget(chatID)
	}
	return strings.TrimSpace(chatID)
}

func (b *Bus) sendACKMarkdown(ctx context.Context, chatID, markdown string) (status, errMsg string) {
	send := b.sendMarkdownACK
	if send == nil {
		send = b.sendFn
	}
	if send == nil || (b.wecom != nil && !b.wecom.Online()) {
		return "skipped", "wecom offline or not configured"
	}
	if err := send(ctx, chatID, markdown); err != nil {
		b.logger.Warn("send wecom preview link message failed", "chat_id", chatID, "err", err)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return "unknown", err.Error()
		}
		return "failed", err.Error()
	}
	return "delivered", ""
}

func escapeWeComLinkLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`, `(`, `\(`, `)`, `\)`, "\r", "", "\n", " ").Replace(strings.TrimSpace(s))
}

func formatPreviewLinksBlock(attachments []core.AttachmentFile) string {
	var lines []string
	for _, att := range attachments {
		if strings.TrimSpace(att.ShareURL) == "" {
			continue
		}
		label := escapeWeComLinkLabel(att.Name)
		if label == "" {
			label = "预览文件"
		}
		lines = append(lines, "🔗 [在线预览 · "+label+"]("+att.ShareURL+")")
	}
	return strings.Join(lines, "\n")
}

func (b *Bus) OnReply(ctx context.Context, entry *model.Entry, action *model.Action, attachments []core.AttachmentFile) core.ReplyDelivery {
	chatID := threadToWeComChat(entry.Thread)
	now := time.Now()
	b.recordRouterReplyAt(entry.Thread, now)
	suppress, wecomRecent := b.wecomDeliveryState(entry.Thread, now)
	hasExplicitFiles := len(attachments) > 0 || len(entry.Files) > 0
	linksBlock := formatPreviewLinksBlock(attachments)

	// Resolve recipient ONCE across the entire reply when delivering explicit files so link + source files never split targets.
	if hasExplicitFiles {
		if resolved := b.resolveWeComChat(chatID); resolved != "" {
			chatID = resolved
		}
	}

	if suppress {
		b.clearPendingWebQuote(entry.Thread)
		if !hasExplicitFiles {
			return core.ReplyDelivery{}
		}
		// Explicit --file deliveries bypass quiet-mode Web suppression: keep prose/card quiet,
		// send 1 compact links-only message (when configured) and all original source files.
		var del core.ReplyDelivery
		if len(attachments) > 0 {
			if linksBlock != "" {
				del.LinkStatus, del.LinkError = b.sendACKMarkdown(ctx, chatID, linksBlock)
			} else {
				del.LinkStatus = "skipped"
				del.LinkError = "preview link skipped: configure public_url or --url"
			}
		}
		del.Files = b.sendReplyAttachments(ctx, chatID, entry, attachments, false)
		return del
	}

	// Decision cards (--opt / --check) bypass Outbox batching and push immediately when not suppressed.
	if action != nil && (len(action.Choices) > 0 || action.Kind == model.ActionKindRouterCheck) {
		b.clearPendingWebQuote(entry.Thread)
		if st := b.takeRouterStream(entry.Thread); st != nil {
			if st.chatID != "" && chatID == "" {
				chatID = st.chatID
			}
			_, _ = st.seal(ctx, "", "请在下方卡片选择 👇")
		}
		if b.wecom != nil && b.wecom.Online() {
			if err := b.wecom.SendActionCard(ctx, chatID, action); err != nil {
				b.logger.Warn("push action card to wecom failed", "action", action.ID, "err", err)
			} else {
				action.PushedToWeCom = true
				if b.engine != nil {
					_ = b.engine.MarkActionPushedToWeCom(ctx, action.ID)
				}
			}
		}
		if hasExplicitFiles {
			var del core.ReplyDelivery
			if len(attachments) > 0 {
				if linksBlock != "" {
					del.LinkStatus, del.LinkError = b.sendACKMarkdown(ctx, chatID, linksBlock)
				} else {
					del.LinkStatus = "skipped"
					del.LinkError = "preview link skipped: configure public_url or --url"
				}
			}
			del.Files = b.sendReplyAttachments(ctx, chatID, entry, attachments, false)
			return del
		}
		return core.ReplyDelivery{}
	}

	webQuote := b.takePendingWebQuote(entry.Thread)

	// Per-recipient causal FIFO Delivery Gate:
	// Take any open Router stream up front so canonical recipient chatID is unified across
	// streams, immediate replies, file deliveries, and outbox retries.
	st := b.takeRouterStream(entry.Thread)
	streamChatID := ""
	if st != nil {
		streamChatID = st.chatID
	}
	chatID = b.canonicalWeComChat(entry.Thread, streamChatID)

	body := prependWebQuoteHeader(webQuote, formatWeComReply(entry))
	combined := body
	if hasExplicitFiles && linksBlock != "" {
		if combined != "" {
			combined = combined + "\n\n" + linksBlock
		} else {
			combined = linksBlock
		}
	}

	item := outboxItem{
		text:          combined,
		at:            now,
		immediate:     hasExplicitFiles || st != nil || wecomRecent || webQuote != "",
		useACK:        hasExplicitFiles && linksBlock != "",
		hasFiles:      hasExplicitFiles,
		entry:         entry,
		attachments:   attachments,
		allowLongFile: !hasExplicitFiles,
	}

	// Non-immediate background replies (no active stream, not recent WeCom, no Web quote, no explicit files)
	// enqueue into the paced 20s batching outbox even during a brief WS reconnect so they are never dropped.
	if !item.immediate {
		if b.sendFn != nil || b.wecom != nil {
			b.enqueueOutboxTail(chatID, item)
		}
		return core.ReplyDelivery{
			Files: b.sendReplyAttachments(ctx, chatID, entry, attachments, true),
		}
	}

	// Serialize all immediate deliveries for the same WeCom recipient so an older reply queued during
	// a transient WS disconnect ALWAYS lands before a newer reply can seal or send.
	cLock := b.chatSendLock(chatID)
	cLock.Lock()
	defer cLock.Unlock()

	// If older replies are already queued for this recipient and WS is online now, drain them FIRST
	// so this reply can still cleanly seal its own passive stream in strict causal order.
	if b.hasOutboxFor(chatID) && b.canSendMarkdown() {
		_ = b.drainChatOutboxLocked(ctx, chatID, true)
	}

	// If older unsent replies remain ahead of us OR transport is currently offline, preserve strict
	// FIFO order by queuing behind them (never sealing a newer stream out of order and never dropping).
	if b.hasOutboxFor(chatID) || !b.wecomTransportOnline() {
		if st != nil {
			st.abort()
		}
		if b.sendFn != nil || b.wecom != nil {
			b.enqueueOutboxTail(chatID, item)
		}
		return queuedReplyDelivery(entry, attachments, linksBlock)
	}

	// Fast path: recipient FIFO is empty and transport is online.
	var del core.ReplyDelivery
	streamSealed := false
	if st != nil {
		if ok, err := st.seal(ctx, "", combined); err != nil {
			b.logger.Warn("seal wecom router stream failed", "thread", entry.Thread, "has_files", hasExplicitFiles, "err", err)
		} else if ok {
			streamSealed = true
		}
	}

	if streamSealed {
		if hasExplicitFiles && len(attachments) > 0 {
			if linksBlock != "" {
				del.LinkStatus = "delivered"
			} else {
				del.LinkStatus = "skipped"
				del.LinkError = "preview link skipped: configure public_url or --url"
			}
		}
		del.Files = b.sendReplyAttachments(ctx, chatID, entry, attachments, !hasExplicitFiles)
		return del
	}

	// Stream was absent or seal failed (e.g. WS dropped right before write).
	if !b.canSendMarkdown() {
		if b.sendFn != nil || b.wecom != nil {
			b.enqueueOutboxHead(chatID, item)
		}
		return queuedReplyDelivery(entry, attachments, linksBlock)
	}

	if hasExplicitFiles {
		if linksBlock != "" {
			status, errMsg := b.sendACKMarkdown(ctx, chatID, combined)
			del.LinkStatus, del.LinkError = status, errMsg
			if status == "failed" || status == "skipped" {
				// Pre-write / disconnect failure: hold whole item at head of FIFO for reconnect retry.
				b.enqueueOutboxHead(chatID, item)
				del.Files = queuedAttachmentResults(attachments, "queued for retry after wecom reconnect")
				return del
			}
			// status == "delivered" or "unknown": do NOT replay body; send attachments once.
		} else {
			if len(attachments) > 0 {
				del.LinkStatus = "skipped"
				del.LinkError = "preview link skipped: configure public_url or --url"
			}
			if body != "" {
				status, _ := b.sendACKMarkdown(ctx, chatID, body)
				if status == "failed" || status == "skipped" {
					b.enqueueOutboxHead(chatID, item)
					del.Files = queuedAttachmentResults(attachments, "queued for retry after wecom reconnect")
					return del
				}
			}
		}
		del.Files = b.sendReplyAttachments(ctx, chatID, entry, attachments, false)
		return del
	}

	if body != "" {
		if err := b.sendFn(ctx, chatID, body); err != nil {
			b.logger.Warn("immediate wecom reply failed", "chat_id", chatID, "err", err)
			if !isUnknownDeliveryErr(err) {
				b.enqueueOutboxHead(chatID, item)
				return del
			}
		}
	}
	del.Files = b.sendReplyAttachments(ctx, chatID, entry, attachments, true)
	return del
}

func queuedReplyDelivery(entry *model.Entry, attachments []core.AttachmentFile, linksBlock string) core.ReplyDelivery {
	var del core.ReplyDelivery
	if len(attachments) > 0 {
		if linksBlock != "" {
			del.LinkStatus = "queued"
			del.LinkError = "wecom reconnecting; queued in FIFO outbox"
		} else {
			del.LinkStatus = "skipped"
			del.LinkError = "preview link skipped: configure public_url or --url"
		}
		del.Files = queuedAttachmentResults(attachments, "wecom reconnecting; queued in FIFO outbox")
	}
	return del
}

func queuedAttachmentResults(attachments []core.AttachmentFile, reason string) []core.AttachmentDelivery {
	if len(attachments) == 0 {
		return nil
	}
	out := make([]core.AttachmentDelivery, 0, len(attachments))
	for _, att := range attachments {
		out = append(out, core.AttachmentDelivery{
			ArtifactID: att.ArtifactID,
			Path:       att.Path,
			Machine:    att.Machine,
			Name:       att.Name,
			Kind:       att.Kind,
			PreviewURL: att.ShareURL,
			Status:     "queued",
			Error:      reason,
		})
	}
	return out
}

func formatWeComReply(entry *model.Entry) string {
	txt := strings.TrimSpace(entry.Text)
	if strings.HasPrefix(txt, "[#") {
		return txt
	}
	tag := ""
	if entry.WorkerRef != nil && strings.TrimSpace(entry.WorkerRef.Name) != "" {
		tag = strings.TrimSpace(entry.WorkerRef.Name)
	} else if strings.HasPrefix(entry.Thread, "wecom:") {
		if entry.Re != nil && entry.Re.Worker != "" {
			tag = entry.Re.Worker
		} else if entry.Ref != "" && !strings.Contains(entry.Ref, ":") {
			tag = entry.Ref
		}
	}
	if tag != "" {
		return "[#" + tag + "] " + txt
	}
	return txt
}

func (b *Bus) sendReplyAttachments(ctx context.Context, chatID string, entry *model.Entry, attachments []core.AttachmentFile, allowLongFallback bool) []core.AttachmentDelivery {
	if entry == nil {
		return nil
	}
	wecomReady := b.sendFileFn != nil && (b.wecom == nil || b.wecom.Online())

	// 1. Pre-fetched attachments validated once by Engine.ReplyDetailed (supports Hub & remote nodes).
	if len(attachments) > 0 {
		out := make([]core.AttachmentDelivery, 0, len(attachments))
		for _, att := range attachments {
			res := core.AttachmentDelivery{
				ArtifactID: att.ArtifactID,
				Path:       att.Path,
				Machine:    att.Machine,
				Name:       att.Name,
				Kind:       att.Kind,
				PreviewURL: att.ShareURL,
			}
			if !wecomReady {
				res.Status = "skipped"
				res.Error = "wecom offline or not configured"
				out = append(out, res)
				continue
			}
			if err := b.sendFileFn(ctx, chatID, att.Name, att.Data); err != nil {
				b.logger.Warn("push reply attachment to wecom failed", "path", att.Path, "machine", att.Machine, "err", err)
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					res.Status = "unknown"
				} else {
					res.Status = "failed"
				}
				res.Error = err.Error()
			} else {
				res.Status = "delivered"
			}
			out = append(out, res)
		}
		return out
	}

	// 2. Fallback for direct Bus.OnReply callers passing entry.Files without pre-fetched bytes (local Hub disk only).
	if len(entry.Files) > 0 {
		out := make([]core.AttachmentDelivery, 0, len(entry.Files))
		for _, rawPath := range entry.Files {
			res := core.AttachmentDelivery{Path: rawPath, Machine: entry.FileMachine}
			if !wecomReady {
				res.Status = "skipped"
				res.Error = "wecom offline or not configured"
				out = append(out, res)
				continue
			}
			name, data, cleanPath, err := core.ReadAttachmentFile(rawPath)
			if err != nil {
				b.logger.Warn("skip invalid reply attachment", "path", rawPath, "err", err)
				res.Status = "failed"
				res.Error = err.Error()
				out = append(out, res)
				continue
			}
			res.Path = cleanPath
			res.Name = name
			if err := b.sendFileFn(ctx, chatID, name, data); err != nil {
				b.logger.Warn("push reply attachment to wecom failed", "path", cleanPath, "err", err)
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					res.Status = "unknown"
				} else {
					res.Status = "failed"
				}
				res.Error = err.Error()
			} else {
				res.Status = "delivered"
			}
			out = append(out, res)
		}
		return out
	}

	// 3. If plain reply body itself is very long (>1800 runes), attach as an offline-readable .md file.
	if allowLongFallback && wecomReady && len([]rune(entry.Text)) > 1800 {
		name := "corgi-report-" + time.Now().Format("150405") + ".md"
		if err := b.sendFileFn(ctx, chatID, name, []byte(entry.Text)); err != nil {
			b.logger.Warn("push long reply attachment to wecom failed", "err", err)
		}
	}
	return nil
}

// FlushDue delivers coalesced Outbox messages whose buffer window has elapsed (or all when force=true).
// Calls are serialized via flushMu so a pre-restart flush always waits for any in-flight periodic send.
func (b *Bus) FlushDue(ctx context.Context, force bool) {
	_ = b.flushInternal(ctx, force)
}

// FlushBeforeRestart waits for any in-flight outbox send to finish and flushes all pending batches.
func (b *Bus) FlushBeforeRestart(ctx context.Context) error {
	return b.flushInternal(ctx, true)
}

func (b *Bus) flushInternal(ctx context.Context, force bool) error {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()

	if !b.canSendMarkdown() {
		return nil
	}

	b.mu.Lock()
	chatIDs := make([]string, 0, len(b.outbox))
	for chatID, items := range b.outbox {
		if len(items) == 0 {
			delete(b.outbox, chatID)
			continue
		}
		chatIDs = append(chatIDs, chatID)
	}
	b.mu.Unlock()

	var firstErr error
	for _, chatID := range chatIDs {
		cLock := b.chatSendLock(chatID)
		cLock.Lock()
		err := b.drainChatOutboxLocked(ctx, chatID, force)
		cLock.Unlock()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// drainChatOutboxLocked drains due FIFO outbox entries for a single recipient.
// Caller must hold b.chatSendLock(chatID) and MUST NOT hold b.mu.
func (b *Bus) drainChatOutboxLocked(ctx context.Context, chatID string, force bool) error {
	now := time.Now()
	for {
		if !b.canSendMarkdown() {
			return nil
		}
		b.mu.Lock()
		items := b.outbox[chatID]
		if len(items) == 0 {
			delete(b.outbox, chatID)
			b.mu.Unlock()
			return nil
		}
		head := items[0]
		if !force && !head.immediate && now.Sub(head.at) < b.flushDelay {
			b.mu.Unlock()
			return nil
		}

		// Coalesce consecutive plain paced items into a single markdown batch (legacy behavior);
		// send immediate / file / recovered-stream items strictly one-by-one in FIFO order.
		var batch []outboxItem
		if !head.immediate && !head.hasFiles && !head.useACK {
			cut := 1
			for cut < len(items) && !items[cut].immediate && !items[cut].hasFiles && !items[cut].useACK {
				cut++
			}
			batch = append([]outboxItem(nil), items[:cut]...)
			items = items[cut:]
		} else {
			batch = []outboxItem{head}
			items = items[1:]
		}
		if len(items) == 0 {
			delete(b.outbox, chatID)
		} else {
			b.outbox[chatID] = items
		}
		send := b.sendFn
		b.mu.Unlock()

		if len(batch) == 1 && (batch[0].immediate || batch[0].hasFiles || batch[0].useACK) {
			it := batch[0]
			var sendErr error
			if strings.TrimSpace(it.text) != "" {
				if it.useACK || it.hasFiles {
					status, errMsg := b.sendACKMarkdown(ctx, chatID, it.text)
					if status == "failed" || status == "skipped" {
						sendErr = errors.New(errMsg)
					}
					// status == "delivered" or "unknown": never replay already-written body.
				} else {
					sendErr = send(ctx, chatID, it.text)
				}
			}
			if sendErr != nil {
				b.logger.Warn("flush immediate wecom reply failed", "chat_id", chatID, "err", sendErr)
				if !isUnknownDeliveryErr(sendErr) {
					if !b.canSendMarkdown() {
						// Offline mid-flush: keep at head without burning retry budget.
						b.enqueueOutboxHead(chatID, it)
						return sendErr
					}
					if it.attempts < 3 {
						it.attempts++
						b.enqueueOutboxHead(chatID, it)
					}
				}
				return sendErr
			}
			if it.entry != nil && (it.hasFiles || it.allowLongFile) {
				_ = b.sendReplyAttachments(ctx, chatID, it.entry, it.attachments, it.allowLongFile)
			}
			continue
		}

		parts := make([]string, 0, len(batch))
		for _, it := range batch {
			if t := strings.TrimSpace(it.text); t != "" {
				parts = append(parts, t)
			}
		}
		if len(parts) == 0 {
			continue
		}
		if err := send(ctx, chatID, strings.Join(parts, "\n\n---\n\n")); err != nil {
			b.logger.Warn("flush outbox markdown failed", "chat_id", chatID, "err", err)
			if !isUnknownDeliveryErr(err) {
				if !b.canSendMarkdown() {
					b.enqueueOutboxHead(chatID, batch...)
					return err
				}
				var retryable []outboxItem
				for _, it := range batch {
					if it.attempts < 1 {
						it.attempts++
						retryable = append(retryable, it)
					}
				}
				if len(retryable) > 0 {
					b.enqueueOutboxHead(chatID, retryable...)
				}
			}
			return err
		}
	}
}

func (b *Bus) OnActionCreated(ctx context.Context, action *model.Action) {
	if action == nil || b.wecom == nil || !b.wecom.Online() {
		return
	}
	if suppress, _ := b.wecomDeliveryState(action.Thread, time.Now()); suppress {
		return
	}
	chatID := threadToWeComChat(action.Thread)
	if err := b.wecom.SendActionCard(ctx, chatID, action); err != nil {
		b.logger.Warn("push worker action card to wecom failed", "action", action.ID, "err", err)
		return
	}
	action.PushedToWeCom = true
	if b.engine != nil {
		_ = b.engine.MarkActionPushedToWeCom(ctx, action.ID)
	}
}

// OnActionResolved synchronizes non-WeCom action resolutions (Web, CLI, Router, dismiss, snooze)
// back to WeCom via a compact status receipt ONLY when the original card was actually pushed to WeCom.
func (b *Bus) OnActionResolved(ctx context.Context, action *model.Action) {
	if action == nil || !action.PushedToWeCom || strings.EqualFold(strings.TrimSpace(action.ResolvedBy), "wecom") {
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

	if b.handleWeComControlOrDirect(ctx, msg, thread, text) {
		return
	}
	b.clearPendingWebQuote(thread)

	replyChatID := msg.ChatID
	if replyChatID == "" {
		replyChatID = msg.FromUser
	}
	st := b.startRouterStream(ctx, thread, replyChatID, msg.ReqID)
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
		if st != nil {
			b.clearRouterStreamIfMatch(st)
			_, _ = st.seal(ctx, "", "接收消息失败："+err.Error())
		}
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

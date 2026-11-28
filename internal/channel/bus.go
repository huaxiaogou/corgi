package channel

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
)

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

	mu                 sync.Mutex
	flushMu            sync.Mutex
	chatSendMu         map[string]*sync.Mutex
	subs               map[chan struct{}]struct{}
	outbox             map[string][]outboxItem
	routerStreams      map[string][]*activeWeComStream
	seenWeComMsgs      map[string]time.Time
	pendingWebQuote    map[string]string
	pendingFocusNotice map[string]string
	wecomCardStates    map[string]*wecomDecisionCardState
	webSeenAt          time.Time
	wecomActiveAt      time.Time
}

// NewBus wires the Super-Agent Engine to Web SSE subscribers and the WeCom client.
func NewBus(eng *core.Engine, wc *wecom.Client, logger *slog.Logger) *Bus {
	if logger == nil {
		logger = slog.Default()
	}
	b := &Bus{
		engine:             eng,
		wecom:              wc,
		logger:             logger,
		flushDelay:         20 * time.Second,
		chatSendMu:         map[string]*sync.Mutex{},
		subs:               map[chan struct{}]struct{}{},
		outbox:             map[string][]outboxItem{},
		routerStreams:      map[string][]*activeWeComStream{},
		seenWeComMsgs:      map[string]time.Time{},
		pendingWebQuote:    map[string]string{},
		pendingFocusNotice: map[string]string{},
		wecomCardStates:    map[string]*wecomDecisionCardState{},
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

func (b *Bus) effectivePublicURL() string {
	if b == nil || b.engine == nil {
		return ""
	}
	return b.engine.EffectivePublicURL()
}

func formatWeComReply(entry *model.Entry, publicURL string) string {
	if entry == nil {
		return ""
	}
	return formatMarkdownForWeCom(entry.Text, publicURL)
}

func threadToWeComChat(thread string) string {
	if chat, ok := strings.CutPrefix(thread, "wecom:"); ok {
		return chat
	}
	return ""
}

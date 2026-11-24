package wecom

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/kiperjing/corgi/internal/model"
)

const DefaultGatewayURL = "wss://openws.work.weixin.qq.com"

// IncomingMessage is a normalized allowlisted WeCom user message (single or group chat).
type IncomingMessage struct {
	ReqID       string
	MsgID       string
	ChatType    string // single | group
	ChatID      string // group chatid when ChatType == "group", else FromUser
	FromUser    string
	Text        string
	QuoteText   string
	Attachments []MediaRef
}

// CardClickEvent is a normalized allowlisted template card button click.
type CardClickEvent struct {
	ReqID        string
	ChatID       string
	FromUser     string
	ActionID     string
	ChoiceKey    string
	ResponseCode string
}

// Config configures the WeCom Smart Bot WebSocket client.
type Config struct {
	GatewayURL string
	BotID      string
	Secret     string
	AllowUsers []string // strict sender allowlist; empty drops all group messages for safety
	DefaultTo  string   // default single-chat userid for proactive notifications
}

// Handlers receives validated inbound messages and card interactions.
type Handlers struct {
	OnMessage   func(ctx context.Context, msg IncomingMessage)
	OnCardClick func(ctx context.Context, ev CardClickEvent)
}

// Client manages the outbound WeCom Smart Bot WebSocket long connection.
type Client struct {
	cfg      Config
	handlers Handlers
	logger   *slog.Logger

	mu             sync.Mutex
	conn           *websocket.Conn
	online         atomic.Bool
	reqSeq         atomic.Uint64
	lastErr        string
	lastSingleUser string
	wake           chan struct{}
	pendingMu      sync.Mutex
	pending        map[string]chan wsResp
}

// NewClient creates a WeCom Smart Bot WebSocket client.
func NewClient(cfg Config, handlers Handlers, logger *slog.Logger) *Client {
	if strings.TrimSpace(cfg.GatewayURL) == "" {
		cfg.GatewayURL = DefaultGatewayURL
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{cfg: cfg, handlers: handlers, logger: logger, wake: make(chan struct{}, 1)}
}

// Online reports whether the WebSocket session is currently subscribed and healthy.
func (c *Client) Online() bool { return c.online.Load() }

// LastError returns the most recent connection or authentication error, or empty when healthy/disabled.
func (c *Client) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

// IsAllowed checks whether userID is permitted to trigger Corgi or resolve actions.
func (c *Client) IsAllowed(userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	c.mu.Lock()
	allow := append([]string(nil), c.cfg.AllowUsers...)
	c.mu.Unlock()
	return slices.Contains(allow, userID)
}

// UpdateConfig hot-reloads credentials or allowlist without recreating the Client.
func (c *Client) UpdateConfig(cfg Config) {
	if strings.TrimSpace(cfg.GatewayURL) == "" {
		cfg.GatewayURL = DefaultGatewayURL
	}
	cfg.BotID = strings.TrimSpace(cfg.BotID)
	cfg.Secret = strings.TrimSpace(cfg.Secret)
	cfg.DefaultTo = strings.TrimSpace(cfg.DefaultTo)
	c.mu.Lock()
	c.cfg = cfg
	c.lastErr = ""
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "reconfigure")
	}
	if c.wake != nil {
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

// Run maintains the long connection with exponential backoff reconnect until ctx is canceled.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		c.mu.Lock()
		cfg := c.cfg
		c.mu.Unlock()
		if cfg.BotID == "" || cfg.Secret == "" {
			c.online.Store(false)
			c.mu.Lock()
			c.lastErr = ""
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
				backoff = time.Second
				continue
			case <-time.After(3 * time.Second):
				continue
			}
		}

		start := time.Now()
		err := c.runOnce(ctx, cfg)
		c.online.Store(false)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.mu.Lock()
			c.lastErr = err.Error()
			c.mu.Unlock()
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		c.logger.Warn("wecom websocket disconnected, reconnecting", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			backoff = time.Second
			continue
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) runOnce(ctx context.Context, cfg Config) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, cfg.GatewayURL, newWSDialOptions())
	if err != nil {
		return fmt.Errorf("dial %s: %w", cfg.GatewayURL, err)
	}
	conn.SetReadLimit(4 << 20)

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
		_ = conn.Close(websocket.StatusNormalClosure, "bye")
	}()

	// Authenticate & subscribe.
	subFrame := map[string]any{
		"cmd":     "aibot_subscribe",
		"headers": map[string]any{"req_id": c.nextReqID()},
		"body": map[string]any{
			"bot_id": cfg.BotID,
			"secret": cfg.Secret,
		},
	}
	if err := c.writeJSON(ctx, conn, subFrame); err != nil {
		return fmt.Errorf("send aibot_subscribe: %w", err)
	}
	c.mu.Lock()
	c.lastErr = ""
	c.mu.Unlock()
	c.online.Store(true)
	c.logger.Info("wecom smart bot websocket connected", "bot_id", cfg.BotID)

	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go func() {
		tk := time.NewTicker(30 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-tk.C:
				_ = c.writeJSON(pingCtx, conn, map[string]any{
					"cmd":     "ping",
					"headers": map[string]any{"req_id": c.nextReqID()},
				})
			}
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if err := c.handleFrameWithErr(ctx, data); err != nil {
			return err
		}
	}
}

// HandleFrame parses an inbound JSON frame from WeCom and enforces strict sender allowlisting.
func (c *Client) HandleFrame(ctx context.Context, data []byte) {
	_ = c.handleFrameWithErr(ctx, data)
}

func (c *Client) handleFrameWithErr(ctx context.Context, data []byte) error {
	var env struct {
		Cmd     string          `json:"cmd"`
		Headers map[string]any  `json:"headers"`
		ErrCode int             `json:"errcode"`
		ErrMsg  string          `json:"errmsg"`
		Body    json.RawMessage `json:"body"`
	}
	if json.Unmarshal(data, &env) != nil {
		return nil
	}
	reqID, _ := env.Headers["req_id"].(string)

	// Route RPC acknowledgements (e.g. aibot_upload_media_*) to awaiting callers.
	if reqID != "" && env.Cmd != "aibot_msg_callback" && env.Cmd != "aibot_event_callback" {
		c.pendingMu.Lock()
		ch, ok := c.pending[reqID]
		c.pendingMu.Unlock()
		if ok {
			select {
			case ch <- wsResp{ErrCode: env.ErrCode, ErrMsg: env.ErrMsg, Body: env.Body}:
			default:
			}
			return nil
		}
		if env.ErrCode != 0 {
			return fmt.Errorf("wecom errcode=%d: %s", env.ErrCode, env.ErrMsg)
		}
	}

	switch env.Cmd {
	case "aibot_msg_callback":
		msg, ok := ParseMsgCallback(env.Body)
		if !ok {
			return nil
		}
		msg.ReqID = reqID
		if !c.IsAllowed(msg.FromUser) {
			c.logger.Info("dropped wecom message from non-allowlisted user", "user", msg.FromUser, "chat_type", msg.ChatType)
			return nil
		}
		if msg.ChatType != "group" && msg.FromUser != "" {
			c.mu.Lock()
			c.lastSingleUser = msg.FromUser
			c.mu.Unlock()
		}
		if c.handlers.OnMessage != nil {
			go c.handlers.OnMessage(ctx, msg)
		}
	case "aibot_event_callback":
		ev, ok := ParseCardEventCallback(reqID, env.Body)
		if !ok {
			return nil
		}
		if !c.IsAllowed(ev.FromUser) {
			c.logger.Info("dropped wecom card click from non-allowlisted user", "user", ev.FromUser)
			return nil
		}
		if c.handlers.OnCardClick != nil {
			go c.handlers.OnCardClick(ctx, ev)
		}
	}
	return nil
}

// ParseMsgCallback extracts an IncomingMessage (text, image, file, or mixed) from aibot_msg_callback.
func ParseMsgCallback(raw json.RawMessage) (IncomingMessage, bool) {
	var b struct {
		MsgID    string `json:"msgid"`
		ChatID   string `json:"chatid"`
		ChatType string `json:"chattype"`
		MsgType  string `json:"msgtype"`
		From     struct {
			UserID string `json:"userid"`
		} `json:"from"`
		Text struct {
			Content string `json:"content"`
		} `json:"text"`
		Image *struct {
			URL    string `json:"url"`
			AESKey string `json:"aeskey"`
		} `json:"image"`
		File *struct {
			URL      string `json:"url"`
			AESKey   string `json:"aeskey"`
			Filename string `json:"filename"`
		} `json:"file"`
		Mixed *struct {
			Items []struct {
				MsgType string `json:"msgtype"`
				Text    *struct {
					Content string `json:"content"`
				} `json:"text"`
				Image *struct {
					URL    string `json:"url"`
					AESKey string `json:"aeskey"`
				} `json:"image"`
			} `json:"msg_item"`
		} `json:"mixed"`
		Quote *struct {
			MsgType string `json:"msgtype"`
			Text    struct {
				Content string `json:"content"`
			} `json:"text"`
		} `json:"quote"`
	}
	if json.Unmarshal(raw, &b) != nil || b.From.UserID == "" {
		return IncomingMessage{}, false
	}

	var texts []string
	var atts []MediaRef
	if t := stripLeadingMention(b.Text.Content); t != "" {
		texts = append(texts, t)
	}
	if b.Image != nil && b.Image.URL != "" {
		atts = append(atts, MediaRef{Type: "image", URL: b.Image.URL, AESKey: b.Image.AESKey})
	}
	if b.File != nil && b.File.URL != "" {
		atts = append(atts, MediaRef{Type: "file", URL: b.File.URL, AESKey: b.File.AESKey, Filename: b.File.Filename})
	}
	if b.Mixed != nil {
		for _, it := range b.Mixed.Items {
			if it.Text != nil {
				if t := stripLeadingMention(it.Text.Content); t != "" {
					texts = append(texts, t)
				}
			}
			if it.Image != nil && it.Image.URL != "" {
				atts = append(atts, MediaRef{Type: "image", URL: it.Image.URL, AESKey: it.Image.AESKey})
			}
		}
	}

	text := strings.TrimSpace(strings.Join(texts, "\n"))
	if text == "" && len(atts) == 0 {
		return IncomingMessage{}, false
	}
	chatID := b.ChatID
	if chatID == "" {
		chatID = b.From.UserID
	}
	quote := ""
	if b.Quote != nil {
		quote = strings.TrimSpace(b.Quote.Text.Content)
	}
	return IncomingMessage{
		MsgID:       b.MsgID,
		ChatType:    b.ChatType,
		ChatID:      chatID,
		FromUser:    b.From.UserID,
		Text:        text,
		QuoteText:   quote,
		Attachments: atts,
	}, true
}

// ParseCardEventCallback parses a button_interaction template_card click event.
func ParseCardEventCallback(reqID string, raw json.RawMessage) (CardClickEvent, bool) {
	var b struct {
		ChatID       string `json:"chatid"`
		ResponseCode string `json:"response_code"`
		From         struct {
			UserID string `json:"userid"`
		} `json:"from"`
		Event struct {
			EventType         string `json:"eventtype"`
			TemplateCardEvent *struct {
				TaskID   string `json:"task_id"`
				EventKey string `json:"event_key"`
			} `json:"template_card_event"`
		} `json:"event"`
	}
	if json.Unmarshal(raw, &b) != nil || b.Event.TemplateCardEvent == nil || b.From.UserID == "" {
		return CardClickEvent{}, false
	}
	ev := b.Event.TemplateCardEvent
	actionID, choiceKey := parseEventKey(ev.TaskID, ev.EventKey)
	if actionID == "" {
		return CardClickEvent{}, false
	}
	return CardClickEvent{
		ReqID:        reqID,
		ChatID:       b.ChatID,
		FromUser:     b.From.UserID,
		ActionID:     actionID,
		ChoiceKey:    choiceKey,
		ResponseCode: b.ResponseCode,
	}, true
}

func parseEventKey(taskID, eventKey string) (actionID, choiceKey string) {
	if a, k, ok := strings.Cut(eventKey, ":"); ok {
		return a, k
	}
	return taskID, eventKey
}

func stripLeadingMention(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "@") {
		if idx := strings.IndexAny(s, " \t\n\u2005"); idx > 0 {
			s = strings.TrimSpace(s[idx+1:])
		} else {
			break
		}
	}
	return s
}

// resolveTarget picks the outbound WeCom chatid: explicit group/chat ID -> configured DefaultTo
// -> most recent allowlisted single-chat sender -> first allowlisted user.
func (c *Client) resolveTarget(chatID string) string {
	if id := strings.TrimSpace(chatID); id != "" {
		return id
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if id := strings.TrimSpace(c.cfg.DefaultTo); id != "" {
		return id
	}
	if c.lastSingleUser != "" && slices.Contains(c.cfg.AllowUsers, c.lastSingleUser) {
		return c.lastSingleUser
	}
	for _, u := range c.cfg.AllowUsers {
		if id := strings.TrimSpace(u); id != "" {
			return id
		}
	}
	return ""
}

// SendMarkdown proactively pushes a markdown message to chatID via aibot_send_msg.
func (c *Client) SendMarkdown(ctx context.Context, chatID, markdown string) error {
	chatID = c.resolveTarget(chatID)
	if chatID == "" {
		return nil
	}
	frame := map[string]any{
		"cmd":     "aibot_send_msg",
		"headers": map[string]any{"req_id": c.nextReqID()},
		"body": map[string]any{
			"chatid":   chatID,
			"msgtype":  "markdown",
			"markdown": map[string]any{"content": markdown},
		},
	}
	return c.send(ctx, frame)
}

// BuildActionCard constructs a WeCom button_interaction template_card for an Action.
func BuildActionCard(a *model.Action) map[string]any {
	buttons := make([]map[string]any, 0, len(a.Choices))
	for i, ch := range a.Choices {
		if i >= 6 {
			break
		}
		style := 2 // secondary outline
		if i == 0 {
			style = 1 // primary highlight for recommended option
		}
		key := ch.Key
		if key == "" {
			key = fmt.Sprintf("%d", i+1)
		}
		buttons = append(buttons, map[string]any{
			"text":  ch.Label,
			"style": style,
			"key":   a.ID + ":" + key,
		})
	}
	desc := a.Summary
	if a.Command != "" {
		if desc != "" {
			desc += "\n"
		}
		desc += "`" + a.Command + "`"
	}
	return map[string]any{
		"card_type": "button_interaction",
		"source":    map[string]any{"desc": "Corgi · 待拍板"},
		"main_title": map[string]any{
			"title": a.Title,
			"desc":  desc,
		},
		"task_id":     a.ID,
		"button_list": buttons,
	}
}

// SendActionCard proactively pushes an interactive decision card via aibot_send_msg.
func (c *Client) SendActionCard(ctx context.Context, chatID string, a *model.Action) error {
	chatID = c.resolveTarget(chatID)
	if chatID == "" {
		return nil
	}
	frame := map[string]any{
		"cmd":     "aibot_send_msg",
		"headers": map[string]any{"req_id": c.nextReqID()},
		"body": map[string]any{
			"chatid":        chatID,
			"msgtype":       "template_card",
			"template_card": BuildActionCard(a),
		},
	}
	return c.send(ctx, frame)
}

// UpdateCardResolved refreshes an interacted template card in-place using aibot_respond_update_msg.
func (c *Client) UpdateCardResolved(ctx context.Context, reqID string, a *model.Action) error {
	if reqID == "" {
		return nil
	}
	frame := map[string]any{
		"cmd":     "aibot_respond_update_msg",
		"headers": map[string]any{"req_id": reqID},
		"body": map[string]any{
			"response_type": "update_template_card",
			"template_card": map[string]any{
				"card_type": "text_notice",
				"source":    map[string]any{"desc": "Corgi · 已拍板"},
				"main_title": map[string]any{
					"title": a.Title,
					"desc":  fmt.Sprintf("已选择：%s（via %s）", a.ChosenLabel, a.ResolvedBy),
				},
			},
		},
	}
	return c.send(ctx, frame)
}

func (c *Client) send(ctx context.Context, frame any) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("wecom websocket not connected")
	}
	return c.writeJSON(ctx, conn, frame)
}

func (c *Client) writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

func (c *Client) nextReqID() string {
	return fmt.Sprintf("corgi-%d-%d", time.Now().Unix(), c.reqSeq.Add(1))
}

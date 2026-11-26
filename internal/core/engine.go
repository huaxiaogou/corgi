package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

var (
	workerNameRe  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	quoteWorkerRe = regexp.MustCompile(`\[#([a-z][a-z0-9_-]{0,31})(?:\s|\]|·)`)
)

// ExtractQuoteWorkerTag parses "[#unit-tests · devbox]" from a WeCom quoted message.
func ExtractQuoteWorkerTag(quotedText string) string {
	m := quoteWorkerRe.FindStringSubmatch(quotedText)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// SpawnRequest defines parameters for launching or resuming a worker.
type SpawnRequest struct {
	Kind      string `json:"kind"`
	Machine   string `json:"machine"`
	Cwd       string `json:"cwd"`
	Task      string `json:"task"`
	Thread    string `json:"thread"`
	Name      string `json:"name"`
	By        string `json:"by"`
	Model     string `json:"model"`
	Effort    string `json:"effort"`
	Subscribe *bool  `json:"subscribe"`
	Resume    string `json:"resume"`
	Worktree  string `json:"worktree,omitempty"` // auto (default) | on | off
}

// SayRequest represents an incoming human message from Web, WeCom, or CLI.
type SayRequest struct {
	Thread        string `json:"thread"`
	Channel       string `json:"channel"` // web | wecom | cli
	From          string `json:"from"`
	Text          string `json:"text"`
	ReEntryID     string `json:"re"`
	ActionID      string `json:"action_id,omitempty"`
	PromptContext string `json:"-"`
	QuoteText     string `json:"quote_text,omitempty"`
	Machine       string `json:"machine,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Model         string `json:"model,omitempty"`
	Effort        string `json:"effort,omitempty"`
	IdemKey       string `json:"idem_key,omitempty"`
}

type screenMark struct {
	hash  string
	since time.Time
	told  bool
}

type handoverState struct {
	at      time.Time
	kind    string
	session string
	pct     int
	worked  bool
}

// Engine is the Super-Agent coordinator orchestrating Router, Workers, Actions, Memory, and Timers.
type Engine struct {
	store    *store.Store
	fleet    *fleet.Fleet
	agents   *agent.Registry
	memory   *MemoryManager
	sink     ChannelSink
	logger   *slog.Logger
	corgiDir string
	home     string
	binPath  string

	mu                      sync.Mutex
	settings                model.Settings
	policies                []model.Policy
	routerLaunching         bool
	routerFresh             bool
	routerLaunchAt          time.Time
	routerAskedAt           time.Time
	routerRepliedAt         time.Time
	routerPrevStatus        string
	routerRetries           int
	routerNudged            bool
	routerUsage             model.ContextUsage
	routerRun               *[2]string
	routerIdleGrace         time.Duration
	routerLastAlive         bool
	routerLastPane          string
	routerLastKind          string
	routerLastStatus        string
	routerLastSeenAt        time.Time
	routerWorkingAt         time.Time
	routerIdleSince         time.Time
	handover                *handoverState
	dream                   *dreamRun
	lastScreen              map[string]screenMark
	lastAnswer              map[string]time.Time
	lastTrust               map[string]time.Time
	idleCheckAt             map[string]time.Time
	lastPatrolScan          time.Time
	noAgent                 map[string]time.Time
	healingBypass           map[string]string
	bypassCheckedAt         map[string]time.Time
	bypassHealFailAt        map[string]time.Time
	bypassHealedPID         map[string]uint32
	bypassCheckInterval     time.Duration
	bypassExitPoll          time.Duration
	bypassExitTimeout       time.Duration
	bypassStartRetryTimeout time.Duration
	rereadTrigID            string

	wakeCh        chan struct{}
	queueMu       sync.Mutex
	wtMu          sync.Mutex
	pausedQueues  map[string]bool
	drainCooldown map[string]time.Time
}

// NewEngine constructs a Super-Agent Engine initialized from SQLite.
func NewEngine(ctx context.Context, st *store.Store, fl *fleet.Fleet, reg *agent.Registry, corgiDir, home, binPath string, logger *slog.Logger) (*Engine, error) {
	settings, err := st.LoadSettings(ctx, home)
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	policies, err := st.LoadPolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("load policies: %w", err)
	}
	mem := NewMemoryManager(corgiDir)
	if err := mem.Ensure(); err != nil {
		return nil, fmt.Errorf("ensure memory: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		store:            st,
		fleet:            fl,
		agents:           reg,
		memory:           mem,
		sink:             NopSink{},
		logger:           logger,
		corgiDir:         corgiDir,
		home:             home,
		binPath:          binPath,
		settings:         settings,
		policies:         policies,
		lastScreen:       map[string]screenMark{},
		lastAnswer:       map[string]time.Time{},
		lastTrust:        map[string]time.Time{},
		idleCheckAt:      map[string]time.Time{},
		noAgent:          map[string]time.Time{},
		healingBypass:    map[string]string{},
		bypassCheckedAt:  map[string]time.Time{},
		bypassHealFailAt: map[string]time.Time{},
		bypassHealedPID:  map[string]uint32{},
		wakeCh:           make(chan struct{}, 1),
		pausedQueues:     map[string]bool{},
		drainCooldown:    map[string]time.Time{},
	}, nil
}

// SetSink attaches the channel notification sink (Web SSE + WeCom).
func (e *Engine) SetSink(s ChannelSink) {
	e.mu.Lock()
	if s == nil {
		e.sink = NopSink{}
	} else {
		e.sink = s
	}
	e.mu.Unlock()
}

// SetFleet updates the underlying fleet router (used by integration tests).
func (e *Engine) SetFleet(fl *fleet.Fleet) {
	e.mu.Lock()
	e.fleet = fl
	e.mu.Unlock()
}

// Memory exposes the layered memory manager.
func (e *Engine) Memory() *MemoryManager { return e.memory }

// Settings returns a copy of current Settings.
func (e *Engine) Settings() model.Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.settings
}

// Policies returns a copy of current Policies.
func (e *Engine) Policies() []model.Policy {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]model.Policy(nil), e.policies...)
}

// SaveConfig persists updated Settings and/or Policies and refreshes Router docs.
func (e *Engine) SaveConfig(ctx context.Context, st *model.Settings, pols []model.Policy) error {
	var routerChanged bool
	e.mu.Lock()
	if st != nil {
		prev := e.settings
		ns := *st
		if agent.NormalizeKind(ns.RouterKind) == "" {
			ns.RouterKind = e.settings.RouterKind
		}
		if agent.NormalizeKind(ns.WorkerKind) == "" {
			ns.WorkerKind = e.settings.WorkerKind
		}
		if ns.ReplyChars <= 0 {
			ns.ReplyChars = 120
		}
		if ns.StallMinutes <= 0 {
			ns.StallMinutes = 12
		}
		if ns.IdlePatrolMinutes < 0 {
			ns.IdlePatrolMinutes = -1
		} else if ns.IdlePatrolMinutes == 0 {
			ns.IdlePatrolMinutes = 30
		} else if ns.IdlePatrolMinutes > 480 {
			ns.IdlePatrolMinutes = 480
		}
		ns.ActiveHours = NormalizeActiveHours(ns.ActiveHours)
		if ns.StaleArchiveDays < 0 {
			ns.StaleArchiveDays = -1
		} else if ns.StaleArchiveDays == 0 {
			ns.StaleArchiveDays = 3
		} else if ns.StaleArchiveDays > 60 {
			ns.StaleArchiveDays = 60
		}
		if ns.NotifyLevel != "normal" {
			ns.NotifyLevel = "quiet"
		}
		if ns.RotatePct > 95 || (ns.RotatePct > 0 && ns.RotatePct < 20) {
			ns.RotatePct = 0
		} else if ns.RotatePct < 0 {
			ns.RotatePct = -1
		}
		if ns.DreamAt != "" && !strings.EqualFold(ns.DreamAt, "off") {
			var dh, dm int
			if _, err := fmt.Sscanf(ns.DreamAt, "%d:%d", &dh, &dm); err != nil || dh < 0 || dh > 23 || dm < 0 || dm > 59 {
				ns.DreamAt = "04:00"
			} else {
				ns.DreamAt = fmt.Sprintf("%02d:%02d", dh, dm)
			}
		}
		if ns.WorkerCwd == "" {
			ns.WorkerCwd = e.home
		}
		routerChanged = prev.RouterKind != ns.RouterKind ||
			prev.RouterModel != ns.RouterModel ||
			prev.RouterEffort != ns.RouterEffort ||
			prev.AskPermissions != ns.AskPermissions
		if prev.AskPermissions && !ns.AskPermissions {
			clear(e.bypassCheckedAt)
			clear(e.bypassHealedPID)
			clear(e.bypassHealFailAt)
		}
		e.settings = ns
	}
	if pols != nil {
		valid := make([]model.Policy, 0, len(pols))
		for _, p := range pols {
			src := strings.TrimSpace(p.Source)
			if src != "" && (p.Action == "respond" || p.Action == "candidate" || p.Action == "ignore") {
				p.Source = src
				valid = append(valid, p)
			}
		}
		e.policies = valid
	}
	curSettings := e.settings
	curPolicies := append([]model.Policy(nil), e.policies...)
	sink := e.sink
	e.mu.Unlock()

	if st != nil {
		if err := e.store.SaveSettings(ctx, curSettings); err != nil {
			return err
		}
		if docChanged, err := e.memory.WriteRouterDocsChanged(e.binPath, curSettings); err == nil && docChanged && !routerChanged {
			e.queueRouterDocReread(ctx)
		}
	}
	if pols != nil {
		if err := e.store.SavePolicies(ctx, curPolicies); err != nil {
			return err
		}
	}
	if routerChanged && e.fleet != nil {
		go func() {
			rctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = e.RestartRouter(rctx)
		}()
	}
	sink.OnStateChanged()
	return nil
}

// Say ingests a human message from Web, WeCom, or CLI.
// If QuoteText contains "[#worker]" and that worker is active, it routes directly to the worker.
func (e *Engine) Say(ctx context.Context, req SayRequest) (*model.Entry, *model.Trigger, error) {
	return e.sayWithSteer(ctx, req, false)
}

func (e *Engine) sayWithSteer(ctx context.Context, req SayRequest, steer bool) (*model.Entry, *model.Trigger, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, nil, fmt.Errorf("消息内容不能为空")
	}
	thread := strings.TrimSpace(req.Thread)
	if thread == "" {
		thread = model.MainThread
	}
	channel := req.Channel
	if channel == "" {
		channel = "web"
	}

	var quote *model.Quote
	if req.ReEntryID != "" {
		if prev, err := e.store.GetEntry(ctx, req.ReEntryID); err == nil {
			who := "Corgi"
			if prev.Role == "user" {
				who = "你"
			}
			qText := oneLineClip(prev.Text, 240)
			if prev.Role == "user" && prev.ActionID != "" && prev.Re != nil && strings.TrimSpace(prev.Re.Text) != "" {
				qText = oneLineClip("拍板「"+prev.Text+"」｜原问："+prev.Re.Text, 260)
			}
			quote = &model.Quote{Entry: prev.ID, Who: who, Text: qText}
		}
	} else if req.QuoteText != "" {
		quote = &model.Quote{Who: "引用", Text: oneLineClip(req.QuoteText, 240)}
	}

	eid, eseq, err := e.store.NextID(ctx, "e")
	if err != nil {
		return nil, nil, err
	}
	entry := &model.Entry{
		ID:       eid,
		Seq:      eseq,
		Role:     "user",
		Thread:   thread,
		Channel:  channel,
		Text:     text,
		At:       time.Now(),
		Re:       quote,
		ActionID: strings.TrimSpace(req.ActionID),
	}

	// Build prompt payload enriched with one-off dispatch overrides or quote context.
	promptText := text
	if strings.TrimSpace(req.PromptContext) != "" {
		promptText = strings.TrimSpace(req.PromptContext) + "\n" + promptText
	} else if quote != nil {
		promptText = fmt.Sprintf("[In reply to: %q]\n%s", quote.Text, promptText)
	}
	if hint := e.buildDispatchHint(req); hint != "" {
		promptText += "\n\n" + hint
	}

	// Fast-path quote routing: "[#worker]" directs follow-up straight to an active worker.
	directTarget := ""
	if tag := ExtractQuoteWorkerTag(req.QuoteText); tag != "" {
		if w, err := e.store.GetWorker(ctx, tag); err == nil && !w.Ended {
			directTarget = w.Name
		}
	}

	source := "chat"
	if channel == "wecom" {
		source = "wecom"
	}
	trig, err := e.Ingest(ctx, source, thread, req.From, promptText, req.IdemKey, directTarget, entry.ID)
	if err != nil {
		return nil, nil, err
	}
	if steer && trig != nil {
		trig.Steer = true
		_ = e.store.UpdateTrigger(ctx, trig)
	}
	if err := e.store.InsertEntry(ctx, entry); err != nil {
		return nil, nil, err
	}
	if recent, _, err := e.store.ListEntries(ctx, "", 30); err == nil {
		_ = e.memory.WriteMessages(recent)
	}
	e.touchThread(ctx, thread, channel, "")
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnUserMessage(ctx, entry)
	sink.OnStateChanged()
	return entry, trig, nil
}

func (e *Engine) buildDispatchHint(req SayRequest) string {
	st := e.Settings()
	var hints []string
	if m := strings.TrimSpace(req.Machine); m != "" {
		resolved := e.fleet.ResolveMachine(m)
		if resolved != st.WorkerMachine {
			name := resolved
			if name == "" {
				name = "hub"
			}
			hints = append(hints, "--machine "+name)
		}
	}
	if k := agent.NormalizeKind(req.Kind); k != "" && k != agent.NormalizeKind(st.WorkerKind) {
		hints = append(hints, "--kind "+k)
	}
	if m := strings.TrimSpace(req.Model); m != "" && agent.ModelNameRe.MatchString(m) && m != st.WorkerModel {
		hints = append(hints, "--model "+m)
	}
	if ef := strings.TrimSpace(req.Effort); ef != "" && agent.ModelNameRe.MatchString(ef) && ef != st.WorkerEffort {
		hints = append(hints, "--effort "+ef)
	}
	if len(hints) == 0 {
		return ""
	}
	return "[The human picked where this runs: spawn with " + strings.Join(hints, " ") + "]"
}

// ReplyRequest describes a Corgi outbound reply with optional quick-choice buttons, review check, or digest replacement.
type ReplyRequest struct {
	Thread   string
	Text     string
	ReID     string
	Options  []string
	Check    bool
	Digest   bool
	Worker   string
	Category string
}

// Reply records Corgi's outbound message and creates a synchronized Action if options/check are set.
func (e *Engine) Reply(ctx context.Context, thread, text, reID string, options []string, check bool) (*model.Entry, *model.Action, string, error) {
	return e.ReplyWithMeta(ctx, ReplyRequest{
		Thread:  thread,
		Text:    text,
		ReID:    reID,
		Options: options,
		Check:   check,
	})
}

// ReplyWithMeta records Corgi's outbound message and creates a synchronized Action,
// replacing any prior pending patrol digest card when req.Digest is true.
func (e *Engine) ReplyWithMeta(ctx context.Context, req ReplyRequest) (*model.Entry, *model.Action, string, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, nil, "", fmt.Errorf("empty reply")
	}
	thread := req.Thread
	if thread == "" {
		thread = model.MainThread
	}
	opts := cleanOptions(req.Options)
	hasOpts := len(opts) > 0

	var quote *model.Quote
	if req.ReID != "" {
		if prev, err := e.store.GetEntry(ctx, req.ReID); err == nil {
			who := "你"
			if prev.Role != "user" {
				who = prev.Role
			}
			quote = &model.Quote{Entry: prev.ID, Who: who, Text: oneLineClip(prev.Text, 120)}
		} else if tr, err := e.store.GetTrigger(ctx, req.ReID); err == nil {
			quote = &model.Quote{Who: tr.From, Text: oneLineClip(tr.Text, 120)}
		}
	}

	eid, eseq, err := e.store.NextID(ctx, "e")
	if err != nil {
		return nil, nil, "", err
	}
	now := time.Now()
	entry := &model.Entry{
		ID:      eid,
		Seq:     eseq,
		Role:    "corgi",
		Thread:  thread,
		Channel: "corgi",
		Text:    text,
		At:      now,
		Re:      quote,
		Options: opts,
		Check:   req.Check && hasOpts,
	}

	var action *model.Action
	if hasOpts {
		aid, _, err := e.store.NextID(ctx, "a")
		if err != nil {
			return nil, nil, "", err
		}
		kind := model.ActionKindRouterChoice
		choices := make([]model.ActionChoice, 0, len(opts))
		for i, label := range opts {
			choices = append(choices, model.ActionChoice{
				Key:   strconv.Itoa(i + 1),
				Label: label,
			})
		}
		if req.Digest {
			kind = model.ActionKindPatrolDigest
			_ = e.store.ExpireActionsByKind(ctx, thread, model.ActionKindPatrolDigest)
		}
		cat := strings.TrimSpace(req.Category)
		if cat == "" {
			if req.Digest {
				cat = "patrol"
			} else {
				cat = "decision"
			}
		}
		action = &model.Action{
			ID:           aid,
			Kind:         kind,
			Thread:       thread,
			EntryID:      entry.ID,
			TargetWorker: strings.TrimSpace(req.Worker),
			Category:     cat,
			Title:        oneLineClip(text, 64),
			Summary:      text,
			Choices:      choices,
			Status:       model.ActionPending,
			CreatedAt:    now,
		}
		entry.ActionID = aid
		if err := e.store.InsertAction(ctx, action); err != nil {
			return nil, nil, "", err
		}
	}

	if err := e.store.InsertEntry(ctx, entry); err != nil {
		return nil, nil, "", err
	}
	if recent, _, err := e.store.ListEntries(ctx, "", 30); err == nil {
		_ = e.memory.WriteMessages(recent)
	}

	e.mu.Lock()
	limit := e.settings.ReplyChars
	e.routerAskedAt = time.Time{}
	e.routerRepliedAt = now
	e.routerRetries = 0
	e.routerNudged = false
	sink := e.sink
	e.mu.Unlock()

	var warns []string
	if !hasOpts && (req.Check || req.Digest) {
		flagName := "--check"
		if req.Check && req.Digest {
			flagName = "--check/--digest"
		} else if req.Digest {
			flagName = "--digest"
		}
		warns = append(warns, fmt.Sprintf("ignored %s without --opt (sent as plain reply; pass 2-4 --opt buttons for choices or an explicit --opt for confirmation)", flagName))
	}
	if limit > 0 && len([]rune(text)) > limit {
		warns = append(warns, fmt.Sprintf("reply is %d chars (usual budget ≤%d; ok if detail or list was requested)", len([]rune(text)), limit))
	}
	warn := strings.Join(warns, "; ")
	sink.OnReply(ctx, entry, action)
	sink.OnStateChanged()
	return entry, action, warn, nil
}

// Ingest stores a Trigger after applying deterministic fast-path policies.
func (e *Engine) Ingest(ctx context.Context, source, thread, from, text, idemKey, target, ref string) (*model.Trigger, error) {
	if thread == "" {
		thread = model.MainThread
	}
	pol := e.policyFor(source)
	decision := pol.Action
	if len(pol.Allow) > 0 && !slices.Contains(pol.Allow, from) {
		decision = "ignore"
	}
	tid, seq, err := e.store.NextID(ctx, "t")
	if err != nil {
		return nil, err
	}
	t := &model.Trigger{
		ID:      tid,
		Seq:     seq,
		Source:  source,
		Thread:  thread,
		From:    from,
		Text:    text,
		At:      time.Now(),
		Policy:  decision,
		Status:  "queued",
		IdemKey: idemKey,
		Ref:     ref,
	}
	if decision == "ignore" {
		t.Status = "ignored"
	} else if target != "" {
		t.Targets = []string{target}
	}
	if err := e.store.InsertTrigger(ctx, t); err != nil {
		if errors.Is(err, store.ErrDuplicateTrigger) {
			return nil, nil
		}
		return nil, err
	}
	e.touchThread(ctx, thread, "", "")
	e.notifyChanged()
	return t, nil
}

func (e *Engine) policyFor(source string) model.Policy {
	switch source {
	case "chat", "wecom", "worker", "timer", "system", "router":
		return model.Policy{Source: source, Action: "respond"}
	}
	e.mu.Lock()
	pols := append([]model.Policy(nil), e.policies...)
	e.mu.Unlock()
	var wildcard *model.Policy
	for i, p := range pols {
		if p.Source == source {
			return p
		}
		if strings.HasSuffix(p.Source, "*") && strings.HasPrefix(source, strings.TrimSuffix(p.Source, "*")) {
			wildcard = &pols[i]
		}
	}
	if wildcard != nil {
		return *wildcard
	}
	return model.Policy{Source: source, Action: "candidate"}
}

func (e *Engine) queueRouterDocReread(ctx context.Context) {
	e.mu.Lock()
	prevID := e.rereadTrigID
	e.mu.Unlock()
	if prevID != "" {
		if prev, err := e.store.GetTrigger(ctx, prevID); err == nil && prev.Status == "queued" {
			return
		}
	}
	trig, err := e.Ingest(ctx, "system", model.MainThread, "corgi",
		"CORGI.md changed (new Corgi build or updated settings). Re-read CORGI.md now and follow it from here on. Nothing to tell the human.",
		"", model.RouterName, "")
	if err == nil && trig != nil {
		e.mu.Lock()
		e.rereadTrigID = trig.ID
		e.mu.Unlock()
	}
}

func (e *Engine) rotatePctLocked() int {
	if e.settings.RotatePct < 0 {
		return 0
	}
	if e.settings.RotatePct == 0 {
		return model.DefaultRotatePct
	}
	return e.settings.RotatePct
}

func (e *Engine) addEvent(ctx context.Context, thread, text, ref, tone string) {
	e.addEventWithAction(ctx, thread, text, ref, tone, "")
}

func (e *Engine) addEventWithAction(ctx context.Context, thread, text, ref, tone, actionID string) {
	id, seq, err := e.store.NextID(ctx, "e")
	if err != nil {
		return
	}
	_ = e.store.InsertEntry(ctx, &model.Entry{
		ID: id, Seq: seq, Role: "event", Thread: orStr(thread, model.MainThread),
		Channel: "system", Text: text, At: time.Now(), Ref: ref, Tone: tone, ActionID: strings.TrimSpace(actionID),
	})
}

func (e *Engine) touchThread(ctx context.Context, id, channel, chatID string) {
	th, err := e.store.GetThread(ctx, id)
	if err != nil {
		th = &model.Thread{ID: id, Channel: channel, ChatID: chatID, Subscribers: []string{}}
	}
	if channel != "" {
		th.Channel = channel
	}
	if chatID != "" {
		th.ChatID = chatID
	}
	th.LastAt = time.Now()
	_ = e.store.UpsertThread(ctx, th)
}

func (e *Engine) notifyChanged() {
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnStateChanged()
}

func cleanOptions(raw []string) []string {
	var out []string
	for _, s := range raw {
		s = oneLineClip(s, 80)
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
			if len(out) >= 6 {
				break
			}
		}
	}
	return out
}

func removeStr(list []string, v string) []string {
	out := make([]string, 0, len(list))
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func orStr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func machineTag(m string, local bool) string {
	if local || m == "" {
		return ""
	}
	return " · " + m
}

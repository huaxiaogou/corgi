package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
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
}

// SayRequest represents an incoming human message from Web, WeCom, or CLI.
type SayRequest struct {
	Thread    string `json:"thread"`
	Channel   string `json:"channel"` // web | wecom | cli
	From      string `json:"from"`
	Text      string `json:"text"`
	ReEntryID string `json:"re"`
	QuoteText string `json:"quote_text,omitempty"`
	Machine   string `json:"machine,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Model     string `json:"model,omitempty"`
	Effort    string `json:"effort,omitempty"`
	IdemKey   string `json:"idem_key,omitempty"`
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

	mu               sync.Mutex
	settings         model.Settings
	policies         []model.Policy
	routerLaunching  bool
	routerFresh      bool
	routerLaunchAt   time.Time
	routerAskedAt    time.Time
	routerPrevStatus string
	routerRetries    int
	routerNudged     bool
	routerUsage      model.ContextUsage
	routerRun        *[2]string
	handover         *handoverState
	dream            *dreamRun
	lastScreen       map[string]screenMark
	lastAnswer       map[string]time.Time
	lastTrust        map[string]time.Time
	noAgent          map[string]time.Time
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
		store:      st,
		fleet:      fl,
		agents:     reg,
		memory:     mem,
		sink:       NopSink{},
		logger:     logger,
		corgiDir:   corgiDir,
		home:       home,
		binPath:    binPath,
		settings:   settings,
		policies:   policies,
		lastScreen: map[string]screenMark{},
		lastAnswer: map[string]time.Time{},
		lastTrust:  map[string]time.Time{},
		noAgent:    map[string]time.Time{},
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
		_ = e.memory.WriteRouterDocs(e.binPath, curSettings)
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
			quote = &model.Quote{Entry: prev.ID, Who: who, Text: oneLineClip(prev.Text, 120)}
		}
	} else if req.QuoteText != "" {
		quote = &model.Quote{Who: "引用", Text: oneLineClip(req.QuoteText, 120)}
	}

	eid, eseq, err := e.store.NextID(ctx, "e")
	if err != nil {
		return nil, nil, err
	}
	entry := &model.Entry{
		ID:      eid,
		Seq:     eseq,
		Role:    "user",
		Thread:  thread,
		Channel: channel,
		Text:    text,
		At:      time.Now(),
		Re:      quote,
	}

	// Build prompt payload enriched with one-off dispatch overrides or quote context.
	promptText := text
	if quote != nil {
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
	if err := e.store.InsertEntry(ctx, entry); err != nil {
		return nil, nil, err
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

// Reply records Corgi's outbound message and creates a synchronized Action if options/check are set.
func (e *Engine) Reply(ctx context.Context, thread, text, reID string, options []string, check bool) (*model.Entry, *model.Action, string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil, "", fmt.Errorf("empty reply")
	}
	if thread == "" {
		thread = model.MainThread
	}
	opts := cleanOptions(options)

	var quote *model.Quote
	if reID != "" {
		if prev, err := e.store.GetEntry(ctx, reID); err == nil {
			who := "你"
			if prev.Role != "user" {
				who = prev.Role
			}
			quote = &model.Quote{Entry: prev.ID, Who: who, Text: oneLineClip(prev.Text, 120)}
		} else if tr, err := e.store.GetTrigger(ctx, reID); err == nil {
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
		Check:   check,
	}

	var action *model.Action
	if len(opts) > 0 || check {
		aid, _, err := e.store.NextID(ctx, "a")
		if err != nil {
			return nil, nil, "", err
		}
		kind := model.ActionKindRouterCheck
		var choices []model.ActionChoice
		if len(opts) > 0 {
			kind = model.ActionKindRouterChoice
			for i, label := range opts {
				choices = append(choices, model.ActionChoice{
					Key:   strconv.Itoa(i + 1),
					Label: label,
				})
			}
		} else {
			choices = []model.ActionChoice{{Key: "1", Label: "已确认"}}
		}
		action = &model.Action{
			ID:        aid,
			Kind:      kind,
			Thread:    thread,
			EntryID:   entry.ID,
			Title:     oneLineClip(text, 64),
			Summary:   text,
			Choices:   choices,
			Status:    model.ActionPending,
			CreatedAt: now,
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
	e.routerRetries = 0
	e.routerNudged = false
	sink := e.sink
	e.mu.Unlock()

	var warn string
	if limit > 0 && len([]rune(text)) > limit {
		warn = fmt.Sprintf("reply is %d chars (usual budget ≤%d; ok if detail or list was requested)", len([]rune(text)), limit)
	}
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

// Spawn launches (or resumes) an isolated worker in herdr and records it in SQLite.

func (e *Engine) AddTimer(ctx context.Context, spec, text, thread, target, owner string) (*model.Timer, error) {
	next, err := ParseTimerSpec(spec, time.Now())
	if err != nil {
		return nil, err
	}
	id, _, err := e.store.NextID(ctx, "tm")
	if err != nil {
		return nil, err
	}
	tm := &model.Timer{
		ID:      id,
		Spec:    strings.TrimSpace(spec),
		Thread:  orStr(thread, model.MainThread),
		Target:  strings.TrimSpace(target),
		Text:    strings.TrimSpace(text),
		NextRun: next,
		Enabled: true,
		Owner:   owner,
	}
	if err := e.store.UpsertTimer(ctx, tm); err != nil {
		return nil, err
	}
	e.notifyChanged()
	return tm, nil
}

// Show expands any ID (trigger, entry, thread, or worker) for `corgi show` (Chain of Recall).
func (e *Engine) Show(ctx context.Context, id string) (any, error) {
	if t, err := e.store.GetTrigger(ctx, id); err == nil {
		return t, nil
	}
	if en, err := e.store.GetEntry(ctx, id); err == nil {
		return en, nil
	}
	if w, err := e.store.GetWorker(ctx, id); err == nil {
		trigs, _ := e.store.ListWorkerTriggers(ctx, w.Name, 15)
		return map[string]any{"worker": w, "triggers": trigs}, nil
	}
	if th, err := e.store.GetThread(ctx, id); err == nil {
		trigs, _ := e.store.ListThreadTriggers(ctx, id, 30)
		return map[string]any{"thread": th, "triggers": trigs}, nil
	}
	return nil, store.ErrNotFound
}

// MemorySnapshot returns the memory files, dream journals, budget metrics, and dream schedule.
func (e *Engine) MemorySnapshot(ctx context.Context, displayDir string) MemorySnapshot {
	snap := MemorySnapshot{
		Dir:         displayDir,
		Files:       e.memory.ListFiles(),
		Dreams:      e.memory.ListDreams(),
		UpFrontSize: e.memory.UpFrontSize(),
		MsgSize:     e.memory.MessagesSize(),
		Budget:      UpFrontBudgetBytes,
		Dreaming:    e.Dreaming(),
	}
	if last := e.LastDream(ctx); !last.IsZero() {
		snap.LastDream = &last
	}
	if next := e.NextDream(ctx, time.Now()); !next.IsZero() {
		snap.NextDream = &next
	}
	return snap
}

// Snapshot builds the SuperView pushed to Web/PWA clients.
func (e *Engine) Snapshot(ctx context.Context, thread string) model.SuperView {
	entries, earlier, _ := e.store.ListEntries(ctx, thread, 200)
	actions, _ := e.store.ListPendingActions(ctx)
	pinned, _ := e.store.ListPinnedEntries(ctx, 50)
	workers, _ := e.store.ListWorkers(ctx)
	threads, _ := e.store.ListThreads(ctx, 100)
	queued, _ := e.store.ListQueuedTriggers(ctx)

	var activeWorkers []*model.Worker
	for _, w := range workers {
		if !w.Ended || time.Since(w.CreatedAt) < 12*time.Hour {
			activeWorkers = append(activeWorkers, w)
		}
	}
	sort.Slice(activeWorkers, func(i, j int) bool { return activeWorkers[i].CreatedAt.After(activeWorkers[j].CreatedAt) })

	qRouter := 0
	for _, t := range queued {
		if len(t.Targets) == 0 || slices.Contains(t.Targets, model.RouterName) {
			qRouter++
		}
	}

	e.mu.Lock()
	st := e.settings
	rv := model.RouterView{
		Kind:     st.RouterKind,
		Model:    st.RouterModel,
		Effort:   st.RouterEffort,
		Status:   "asleep",
		Context:  e.routerUsage.Pct(),
		RotateAt: e.rotatePctLocked(),
		Handover: e.handover != nil,
		Dreaming: e.dream != nil,
	}
	if e.routerLaunching {
		rv.Status = "starting"
	}
	if e.dream != nil {
		rv.Status = "dreaming"
	}
	askedAt := e.routerAskedAt
	routerRun := e.routerRun
	sink := e.sink
	e.mu.Unlock()

	if live, err := e.fleet.Agents(ctx); err == nil {
		for _, a := range live {
			if a.Name != nil && *a.Name == model.RouterName {
				rv.Alive = true
				rv.PaneID = a.PaneID
				rv.Status = a.AgentStatus
				if a.Agent != nil && *a.Agent != "" {
					rv.Kind = *a.Agent
				}
				if routerRun != nil {
					rv.Model, rv.Effort = routerRun[0], routerRun[1]
					rv.Pending = *routerRun != [2]string{st.RouterModel, st.RouterEffort} ||
						agent.NormalizeKind(rv.Kind) != agent.NormalizeKind(st.RouterKind)
				}
				if rv.Status == "working" && !askedAt.IsZero() {
					for i := len(entries) - 1; i >= 0 && !entries[i].At.Before(askedAt); i-- {
						if r := entries[i].Role; r == "corgi" || r == "user" {
							rv.Replied = r == "corgi"
							break
						}
					}
				}
				break
			}
		}
	}

	var follow *model.FollowView
	if timers, err := e.store.ListTimers(ctx); err == nil {
		for _, tm := range timers {
			if tm.Enabled && !tm.NextRun.IsZero() {
				follow = &model.FollowView{Next: tm.NextRun, NextText: oneLineClip(tm.Text, 80)}
				break // ListTimers is ordered by next_run ASC
			}
		}
	}

	return model.SuperView{
		Router:       rv,
		Entries:      entries,
		Earlier:      earlier,
		Actions:      actions,
		Pinned:       pinned,
		Workers:      activeWorkers,
		Threads:      threads,
		Queued:       len(queued),
		QueuedRouter: qRouter,
		Paused:       st.Paused,
		WeComOnline:  sink.WeComOnline(),
		Follow:       follow,
	}
}

func (e *Engine) rotatePctLocked() int {
	if e.settings.RotatePct < 0 {
		return 0
	}
	if e.settings.RotatePct == 0 {
		return 60
	}
	return e.settings.RotatePct
}

func (e *Engine) addEvent(ctx context.Context, thread, text, ref, tone string) {
	id, seq, err := e.store.NextID(ctx, "e")
	if err != nil {
		return
	}
	_ = e.store.InsertEntry(ctx, &model.Entry{
		ID: id, Seq: seq, Role: "event", Thread: orStr(thread, model.MainThread),
		Channel: "system", Text: text, At: time.Now(), Ref: ref, Tone: tone,
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

func machineTag(m string) string {
	if m == "" {
		return ""
	}
	return " · " + m
}

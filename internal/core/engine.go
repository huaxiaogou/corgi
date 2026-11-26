package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	WorkID    string `json:"work_id,omitempty"`
	Ephemeral bool   `json:"ephemeral,omitempty"`
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
	ClientMsgID   string `json:"client_msg_id,omitempty"`
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
	version  string

	mu                      sync.Mutex
	settings                model.Settings
	policies                []model.Policy
	flagPublicURLs          []string
	artifactHMACSecret      []byte
	routerLaunching         bool
	routerDispatching       bool
	hubUpdating             bool
	autoUpdater             AutoUpdater
	lastAutoUpdateCheck     time.Time
	routerFresh             bool
	routerLaunchAt          time.Time
	routerAskedAt           time.Time
	routerRepliedAt         time.Time
	routerPrevStatus        string
	routerRetries           int
	routerRetryPending      bool
	routerRetryDelays       []time.Duration
	routerMinSilentAge      time.Duration
	routerLastErr           string
	routerLastErrAt         time.Time
	routerErrDedupWindow    time.Duration
	routerNudged            bool
	routerStallAlerted      bool
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
	lastPendingExpire       time.Time
	runCtx                  context.Context
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
	var savedRouterRun *[2]string
	if rawRun, ok, err := st.GetKV(ctx, "router_run"); err == nil && ok && strings.TrimSpace(rawRun) != "" {
		var rr [2]string
		if json.Unmarshal([]byte(rawRun), &rr) == nil {
			savedRouterRun = &rr
		}
	}
	eng := &Engine{
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
		routerRun:        savedRouterRun,
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
	}
	eng.loadActiveDreamMarker(ctx)
	return eng, nil
}

// SetAutoUpdater registers the optional overnight auto-update scheduler hook.
func (e *Engine) SetAutoUpdater(u AutoUpdater) {
	e.mu.Lock()
	e.autoUpdater = u
	e.mu.Unlock()
}

// SetVersion records the running Hub binary version for post-upgrade confirmations.
func (e *Engine) SetVersion(v string) {
	e.mu.Lock()
	e.version = strings.TrimSpace(v)
	e.mu.Unlock()
}

// PostEvent appends a concise system event entry to a thread and notifies subscribers.
func (e *Engine) PostEvent(ctx context.Context, thread, text, ref, tone string) {
	if thread == "" {
		thread = model.MainThread
	}
	e.addEvent(ctx, thread, text, ref, tone)
	e.notifyChanged()
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
	var (
		routerChanged bool
		nextSettings  model.Settings
		nextPolicies  []model.Policy
	)
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
			ns.ReplyChars = model.DefaultReplyChars
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
		normPubURL, urlErr := NormalizePublicURL(ns.PublicURL)
		if urlErr != nil {
			e.mu.Unlock()
			return urlErr
		}
		ns.PublicURL = normPubURL
		routerChanged = prev.RouterKind != ns.RouterKind ||
			prev.RouterModel != ns.RouterModel ||
			prev.RouterEffort != ns.RouterEffort ||
			prev.AskPermissions != ns.AskPermissions
		nextSettings = ns
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
		nextPolicies = valid
	}
	sink := e.sink
	e.mu.Unlock()

	if st != nil {
		if err := e.store.SaveSettings(ctx, nextSettings); err != nil {
			return err
		}
	}
	if pols != nil {
		if err := e.store.SavePolicies(ctx, nextPolicies); err != nil {
			return err
		}
	}

	e.mu.Lock()
	if st != nil {
		if e.settings.AskPermissions && !nextSettings.AskPermissions {
			clear(e.bypassCheckedAt)
			clear(e.bypassHealedPID)
			clear(e.bypassHealFailAt)
		}
		e.settings = nextSettings
	}
	if pols != nil {
		e.policies = nextPolicies
	}
	curSettings := e.settings
	e.mu.Unlock()

	if st != nil {
		if docChanged, err := e.memory.WriteRouterDocsChanged(e.binPath, curSettings); err == nil && docChanged && !routerChanged {
			e.queueRouterDocReread(ctx)
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

func normalizeSayIdemKey(req SayRequest) string {
	if k := strings.TrimSpace(req.IdemKey); k != "" {
		return k
	}
	if cid := strings.TrimSpace(req.ClientMsgID); cid != "" {
		return "client:" + cid
	}
	return ""
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
	idemKey := normalizeSayIdemKey(req)

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
	tid, tseq, err := e.store.NextID(ctx, "t")
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	entry := &model.Entry{
		ID:       eid,
		Seq:      eseq,
		Role:     "user",
		Thread:   thread,
		Channel:  channel,
		Text:     text,
		At:       now,
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
	pol := e.policyFor(source)
	decision := pol.Action
	if len(pol.Allow) > 0 && !slices.Contains(pol.Allow, req.From) {
		decision = "ignore"
	}
	trig := &model.Trigger{
		ID:      tid,
		Seq:     tseq,
		Source:  source,
		Thread:  thread,
		From:    req.From,
		Text:    promptText,
		At:      now,
		Policy:  decision,
		Status:  "queued",
		IdemKey: idemKey,
		Ref:     entry.ID,
		Steer:   steer,
	}
	if decision == "ignore" {
		trig.Status = "ignored"
	} else if directTarget != "" {
		trig.Targets = []string{directTarget}
	}

	savedEntry, savedTrig, dup, err := e.store.CommitSayEntryAndTrigger(ctx, entry, trig)
	if err != nil {
		return nil, nil, err
	}
	if dup {
		return savedEntry, savedTrig, nil
	}

	if strings.TrimSpace(req.ActionID) == "" {
		e.autoResolvePendingOnUserReply(ctx, thread, req.ReEntryID, req.QuoteText, channel, text)
	}
	if recent, _, err := e.store.ListEntries(ctx, "", 30); err == nil {
		_ = e.memory.WriteMessages(recent)
	}
	e.touchThread(ctx, thread, channel, "")
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnUserMessage(ctx, savedEntry)
	sink.OnStateChanged()
	return savedEntry, savedTrig, nil
}

// MarkActionPushedToWeCom persists that a pending Action card was pushed to WeCom.
func (e *Engine) MarkActionPushedToWeCom(ctx context.Context, actionID string) error {
	return e.store.MarkActionPushedToWeCom(ctx, actionID)
}

// autoResolvePendingOnUserReply resolves pending actions in thread when the human replies in chat:
// - Explicit quoted entry (reEntryID != "") resolves only the matching action (including router_check).
// - WeCom quoted text with unknown entry ID (reEntryID == "" && quoteText != "") never guesses.
// - Unquoted follow-up replies only auto-resolve router_choice / patrol_digest (never router_check or worker_dialog).
func (e *Engine) autoResolvePendingOnUserReply(ctx context.Context, thread, reEntryID, quoteText, channel, text string) {
	reID := strings.TrimSpace(reEntryID)
	if reID == "" && strings.TrimSpace(quoteText) != "" {
		return
	}
	pending, err := e.store.ListPendingActions(ctx)
	if err != nil || len(pending) == 0 {
		return
	}
	var candidates []*model.Action
	for _, a := range pending {
		if a.Thread != thread || a.Kind == model.ActionKindWorkerDialog {
			continue
		}
		if reID == "" && a.Kind == model.ActionKindRouterCheck {
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return
	}

	entries, _, _ := e.store.ListEntries(ctx, thread, 60)
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	label := oneLineClip(text, 120)

	for _, a := range candidates {
		matched := false
		if reID != "" {
			matched = a.EntryID != "" && a.EntryID == reID
		} else {
			matched = !hasLaterCorgiEntry(entries, a)
		}
		if !matched {
			continue
		}
		if resolved, err := e.store.ResolveAction(ctx, a.ID, channel, "reply", label); err == nil {
			sink.OnActionResolved(ctx, resolved)
		}
	}
}

func hasLaterCorgiEntry(entries []*model.Entry, a *model.Action) bool {
	if a.EntryID != "" {
		found := false
		for _, en := range entries {
			if en.ID == a.EntryID {
				found = true
				continue
			}
			if found && en.Role == "corgi" {
				return true
			}
		}
		if found {
			return false
		}
	}
	for _, en := range entries {
		if en.Role == "corgi" && en.At.After(a.CreatedAt) {
			return true
		}
	}
	return false
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

// ReplyRequest describes a Corgi outbound reply with optional quick-choice buttons,
// review check, digest replacement, work item linkage, or explicit file attachments.
type ReplyRequest struct {
	Thread   string
	Text     string
	ReID     string
	Options  []string
	Files    []string
	Machine  string
	WorkID   string
	Check    bool
	Digest   bool
	Worker   string
	Category string
}

// ReplyResult carries the persisted entry, optional action, warning string, and per-file delivery outcomes.
type ReplyResult struct {
	Entry    *model.Entry
	Action   *model.Action
	Warning  string
	Delivery ReplyDelivery
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
	res, err := e.ReplyDetailed(ctx, req)
	if err != nil {
		return nil, nil, "", err
	}
	return res.Entry, res.Action, res.Warning, nil
}

// ReplyDetailed validates and fetches any explicit attachments once on the selected machine before
// persisting the entry/action, passes the exact fetched bytes to ChannelSink, records confirmed
// delivery evidence on req.WorkID when set, and returns per-file delivery outcomes.
func (e *Engine) ReplyDetailed(ctx context.Context, req ReplyRequest) (*ReplyResult, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, fmt.Errorf("empty reply")
	}
	opts, err := cleanOptions(req.Options)
	if err != nil {
		return nil, err
	}
	thread := req.Thread
	if thread == "" {
		thread = model.MainThread
	}
	workID := strings.TrimSpace(req.WorkID)
	if workID != "" {
		wi, wErr := e.store.GetWorkItem(ctx, workID)
		if wErr != nil {
			return nil, fmt.Errorf("invalid --work %q: %w", workID, wErr)
		}
		wiThread := wi.Thread
		if wiThread == "" {
			wiThread = model.MainThread
		}
		if wiThread != thread {
			return nil, fmt.Errorf("--work %q belongs to thread %q, not %q", workID, wiThread, thread)
		}
	}
	fetchedFiles, files, fileMachine, err := e.readReplyAttachments(ctx, req.Machine, req.Files)
	if err != nil {
		if strings.TrimSpace(req.Machine) == "" && errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("validate reply attachments (checked Hub local disk; if the file lives on another machine pass --machine <M>): %w", err)
		}
		return nil, fmt.Errorf("validate reply attachments: %w", err)
	}
	hasOpts := len(opts) > 0
	wantCheck := !hasOpts && req.Check

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
		return nil, err
	}
	now := time.Now()
	artifactRecs, artifactSummaries, cleanupBlobs, err := e.stageArtifacts(ctx, eid, thread, workID, fetchedFiles, now)
	if err != nil {
		return nil, fmt.Errorf("stage reply artifacts: %w", err)
	}

	entry := &model.Entry{
		ID:          eid,
		Seq:         eseq,
		Role:        "corgi",
		Thread:      thread,
		Channel:     "corgi",
		Text:        text,
		At:          now,
		Re:          quote,
		Options:     opts,
		Files:       files,
		FileMachine: fileMachine,
		Artifacts:   artifactSummaries,
		WorkItemID:  workID,
		Check:       req.Check,
	}

	var action *model.Action
	var expireDigestKind string
	if hasOpts || wantCheck {
		aid, _, err := e.store.NextID(ctx, "a")
		if err != nil {
			cleanupBlobs()
			return nil, err
		}
		kind := model.ActionKindRouterChoice
		var choices []model.ActionChoice
		if hasOpts {
			choices = make([]model.ActionChoice, 0, len(opts))
			for i, label := range opts {
				choices = append(choices, model.ActionChoice{
					Key:   strconv.Itoa(i + 1),
					Label: label,
				})
			}
			if req.Digest {
				kind = model.ActionKindPatrolDigest
				expireDigestKind = model.ActionKindPatrolDigest
			}
		} else {
			kind = model.ActionKindRouterCheck
			if req.Digest {
				expireDigestKind = model.ActionKindPatrolDigest
			}
		}
		cat := strings.TrimSpace(req.Category)
		if cat == "" {
			switch {
			case req.Digest:
				cat = "patrol"
			case wantCheck:
				cat = "check"
			default:
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
	}

	if err := e.store.CommitReplyTx(ctx, entry, action, expireDigestKind, artifactRecs); err != nil {
		cleanupBlobs()
		return nil, err
	}
	if recent, _, err := e.store.ListEntries(ctx, "", 30); err == nil {
		_ = e.memory.WriteMessages(recent)
	}

	e.mu.Lock()
	limit := e.settings.ReplyChars
	e.routerAskedAt = time.Time{}
	e.routerRepliedAt = now
	e.routerRetries = 0
	e.routerRetryPending = false
	e.routerNudged = false
	e.routerStallAlerted = false
	sink := e.sink
	e.mu.Unlock()

	// Publish committed entry & Hub snapshots to Web SSE immediately before slow channel I/O.
	sink.OnStateChanged()

	var warns []string
	if !hasOpts && !wantCheck && req.Digest {
		warns = append(warns, "ignored --digest without --opt or --check (sent as plain reply; pass 2-4 --opt buttons for a decision or --check for a lightweight review reminder)")
	}
	if len(req.Files) == 0 {
		if p := mentionsDeliverablePath(text); p != "" {
			warns = append(warns, fmt.Sprintf("reply mentions %s without --file (pass --file %q [--machine M] to publish Hub preview link & WeCom attachment)", p, p))
		}
	}
	if limit > 0 {
		if n := ReadableReplyLen(text); n > limit {
			warns = append(warns, fmt.Sprintf("reply is %d units (usual budget ≤%d; ok if detail or list was requested)", n, limit))
		}
	}
	delivery := sink.OnReply(ctx, entry, action, fetchedFiles)
	if delivery.LinkStatus != "" && delivery.LinkStatus != "delivered" {
		detail := delivery.LinkStatus
		if delivery.LinkError != "" {
			detail += ": " + delivery.LinkError
		}
		warns = append(warns, "wecom preview link "+detail)
	}
	for _, fd := range delivery.Files {
		if fd.Status == "delivered" && workID != "" {
			_, _ = e.store.RecordWorkItemArtifact(ctx, workID, model.WorkArtifact{
				ArtifactID:  fd.ArtifactID,
				Name:        fd.Name,
				Kind:        fd.Kind,
				Size:        int64(len(findFetchedBytes(fetchedFiles, fd.ArtifactID))),
				Path:        fd.Path,
				Machine:     fd.Machine,
				EntryID:     entry.ID,
				DeliveredAt: time.Now(),
			})
		} else if fd.Status != "delivered" {
			detail := fd.Status
			if fd.Error != "" {
				detail += ": " + fd.Error
			}
			warns = append(warns, fmt.Sprintf("attachment %s %s", fd.Path, detail))
		}
	}
	sink.OnStateChanged()
	if len(artifactRecs) > 0 {
		go e.SweepArtifacts(context.Background())
	}
	return &ReplyResult{
		Entry:    entry,
		Action:   action,
		Warning:  strings.Join(warns, "; "),
		Delivery: delivery,
	}, nil
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

const maxOptionRunes = 240

func cleanOptions(raw []string) ([]string, error) {
	var out []string
	for i, s := range raw {
		s = strings.Join(strings.Fields(s), " ")
		if s == "" {
			continue
		}
		if n := len([]rune(s)); n > maxOptionRunes {
			return nil, fmt.Errorf("option %d exceeds %d characters (%d)", i+1, maxOptionRunes, n)
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
			if len(out) >= 6 {
				break
			}
		}
	}
	return out, nil
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

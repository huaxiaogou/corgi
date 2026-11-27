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
	Kind            string `json:"kind"`
	Machine         string `json:"machine"`
	Cwd             string `json:"cwd"`
	Task            string `json:"task"`
	Thread          string `json:"thread"`
	Name            string `json:"name"`
	By              string `json:"by"`
	Model           string `json:"model"`
	Effort          string `json:"effort"`
	Subscribe       *bool  `json:"subscribe"`
	Resume          string `json:"resume"`
	Worktree        string `json:"worktree,omitempty"` // auto (default) | on | off
	WorkID          string `json:"work_id,omitempty"`
	WorkTitle       string `json:"work_title,omitempty"`
	ProjectID       string `json:"project_id,omitempty"`
	OriginalRequest string `json:"original_request,omitempty"`
	Constraint      string `json:"constraint,omitempty"`
	Ephemeral       bool   `json:"ephemeral,omitempty"`
	Mode            string `json:"mode,omitempty"` // readonly | prototype | implement (optional)
}

// SayRequest represents an incoming human message from Web, WeCom, or CLI.
type SayRequest struct {
	Thread        string           `json:"thread"`
	Channel       string           `json:"channel"` // web | wecom | cli
	From          string           `json:"from"`
	Text          string           `json:"text"`
	ReEntryID     string           `json:"re"`
	ActionID      string           `json:"action_id,omitempty"`
	PromptContext string           `json:"-"`
	QuoteText     string           `json:"quote_text,omitempty"`
	Machine       string           `json:"machine,omitempty"`
	Kind          string           `json:"kind,omitempty"`
	Model         string           `json:"model,omitempty"`
	Effort        string           `json:"effort,omitempty"`
	ProjectID     string           `json:"project_id,omitempty"`
	Media         []model.MediaRef `json:"media,omitempty"`
	IdemKey       string           `json:"idem_key,omitempty"`
	ClientMsgID   string           `json:"client_msg_id,omitempty"`
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
	routerTrueIdleSince     time.Time
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

	// In-process native API Router runtime state (protected by mu except apiSwitchMu).
	apiSwitchMu            sync.Mutex
	apiTurn                *apiRouterTurn
	apiTurnGen             uint64
	apiConfigEpoch         uint64
	apiBackoffUntil        time.Time
	apiFailCount           int
	apiLastFailTriggerID   string
	apiSameTriggerFails    int
	apiAuthPaused          bool
	apiLastErr             string
	apiAllowPrivateForTest bool

	// Go-driven auto-verify bounce state for implement worktree workers (protected by mu).
	autoVerify         map[string]*workerAutoVerify
	autoVerifyReleased map[string]bool
	autoVerifyFn       func(ctx context.Context, req VerifyWorkerRequest) (*VerifyWorkerResponse, error)

	wakeCh                     chan struct{}
	queueMu                    sync.Mutex
	wtMu                       sync.Mutex
	pausedQueues               map[string]bool
	drainCooldown              map[string]time.Time
	directTurns                map[string][]*pendingDirectTurn
	directBgNotes              []string
	watchdogMarks              map[string]*watchdogMark
	constraintRemindAt         map[string]time.Time
	workerTurns                map[string]*workerTurnWatermark
	workerStartupProbeDelay    time.Duration
	workerStartupProbeInterval time.Duration
	workerStartupCheckWindow   time.Duration
}

// NewEngine constructs a Super-Agent Engine initialized from SQLite.
func NewEngine(ctx context.Context, st *store.Store, fl *fleet.Fleet, reg *agent.Registry, corgiDir, home, binPath string, logger *slog.Logger) (*Engine, error) {
	settings, err := st.LoadSettings(ctx, home)
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	NormalizeDispatchConfig(&settings)
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
		store:              st,
		fleet:              fl,
		agents:             reg,
		memory:             mem,
		sink:               NopSink{},
		logger:             logger,
		corgiDir:           corgiDir,
		home:               home,
		binPath:            binPath,
		settings:           settings,
		policies:           policies,
		routerRun:          savedRouterRun,
		lastScreen:         map[string]screenMark{},
		lastAnswer:         map[string]time.Time{},
		lastTrust:          map[string]time.Time{},
		idleCheckAt:        map[string]time.Time{},
		noAgent:            map[string]time.Time{},
		healingBypass:      map[string]string{},
		bypassCheckedAt:    map[string]time.Time{},
		bypassHealFailAt:   map[string]time.Time{},
		bypassHealedPID:    map[string]uint32{},
		wakeCh:             make(chan struct{}, 1),
		pausedQueues:       map[string]bool{},
		drainCooldown:      map[string]time.Time{},
		directTurns:        map[string][]*pendingDirectTurn{},
		watchdogMarks:      map[string]*watchdogMark{},
		constraintRemindAt: map[string]time.Time{},
		workerTurns:        map[string]*workerTurnWatermark{},
		autoVerify:         map[string]*workerAutoVerify{},
		autoVerifyReleased: map[string]bool{},
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
		routerChanged    bool
		apiConfigChanged bool
		prevMode         string
		nextMode         string
		nextSettings     model.Settings
		nextPolicies     []model.Policy
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
		NormalizeDispatchConfig(&ns)
		if err := normalizeRouterAPISettings(&ns, e.apiAllowPrivateForTest); err != nil {
			e.mu.Unlock()
			return err
		}
		prevMode = prev.EffectiveRouterMode()
		nextMode = ns.EffectiveRouterMode()
		apiConfigChanged = prevMode != nextMode ||
			prev.RouterAPIProvider != ns.RouterAPIProvider ||
			prev.RouterAPIBaseURL != ns.RouterAPIBaseURL ||
			prev.RouterAPIModel != ns.RouterAPIModel ||
			prev.RouterAPIEffort != ns.RouterAPIEffort
		routerChanged = nextMode == "pty" && (prev.RouterKind != ns.RouterKind ||
			prev.RouterModel != ns.RouterModel ||
			prev.RouterEffort != ns.RouterEffort ||
			prev.AskPermissions != ns.AskPermissions)
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

	if st != nil && prevMode != nextMode {
		e.apiSwitchMu.Lock()
		defer e.apiSwitchMu.Unlock()
		if err := e.prepareRouterModeSwitch(ctx, prevMode, nextMode); err != nil {
			return err
		}
	}

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
		if apiConfigChanged {
			e.apiConfigEpoch++
			e.apiBackoffUntil = time.Time{}
			e.apiFailCount = 0
			e.apiLastFailTriggerID = ""
			e.apiSameTriggerFails = 0
			e.apiAuthPaused = false
			e.apiLastErr = ""
		}
	}
	if pols != nil {
		e.policies = nextPolicies
	}
	curSettings := e.settings
	e.mu.Unlock()

	if st != nil {
		if curSettings.EffectiveRouterMode() == "pty" {
			if docChanged, err := e.memory.WriteRouterDocsChanged(e.binPath, curSettings); err == nil && docChanged && !routerChanged {
				e.queueRouterDocReread(ctx)
			}
		}
	}
	if routerChanged && e.fleet != nil {
		go func() {
			rctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = e.RestartRouter(rctx)
		}()
	}
	if apiConfigChanged {
		e.wake()
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
		Media:    append([]model.MediaRef(nil), req.Media...),
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
		Media:   append([]model.MediaRef(nil), req.Media...),
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

// autoResolvePendingOnUserReply resolves a pending non-dialog action ONLY when the human
// explicitly quotes that action's entry (or another entry in its reply chain).
// Plain unquoted chat messages never auto-resolve unanswered pending decision cards.
func (e *Engine) autoResolvePendingOnUserReply(ctx context.Context, thread, reEntryID, _ string, channel, text string) {
	reID := strings.TrimSpace(reEntryID)
	if reID == "" {
		return
	}
	pending, err := e.store.ListPendingActions(ctx)
	if err != nil || len(pending) == 0 {
		return
	}
	var candidates []*model.Action
	for _, a := range pending {
		if a.Thread != thread || a.Kind == model.ActionKindWorkerDialog || strings.TrimSpace(a.EntryID) == "" {
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return
	}

	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	label := oneLineClip(text, 120)

	for _, a := range candidates {
		if !e.entryInReplyChain(ctx, reID, a.EntryID) {
			continue
		}
		if resolved, err := e.store.ResolveAction(ctx, a.ID, channel, "reply", label); err == nil {
			sink.OnActionResolved(ctx, resolved)
		}
	}
}

// entryInReplyChain reports whether fromID equals targetID or reaches targetID by following
// Entry.Re.Entry parent pointers (bounded to 16 hops to prevent cycles).
func (e *Engine) entryInReplyChain(ctx context.Context, fromID, targetID string) bool {
	cur := strings.TrimSpace(fromID)
	target := strings.TrimSpace(targetID)
	if cur == "" || target == "" {
		return false
	}
	seen := make(map[string]bool, 4)
	for hops := 0; cur != "" && hops < 16; hops++ {
		if cur == target {
			return true
		}
		if seen[cur] {
			return false
		}
		seen[cur] = true
		en, err := e.store.GetEntry(ctx, cur)
		if err != nil || en == nil || en.Re == nil {
			return false
		}
		cur = strings.TrimSpace(en.Re.Entry)
	}
	return false
}

func (e *Engine) buildDispatchHint(req SayRequest) string {
	st := e.Settings()
	apiMode := st.EffectiveRouterMode() == "api"
	var hints []string
	addHint := func(flag, key, val string) {
		if apiMode {
			hints = append(hints, key+"="+val)
		} else {
			hints = append(hints, flag+" "+val)
		}
	}
	effMachine := st.WorkerMachine
	if m := strings.TrimSpace(req.Machine); m != "" {
		resolved := m
		if e.fleet != nil {
			resolved = e.fleet.ResolveMachine(m)
		}
		effMachine = resolved
		if resolved != st.WorkerMachine {
			name := resolved
			if name == "" {
				name = "hub"
			}
			addHint("--machine", "machine", name)
		}
	}
	effKind := agent.NormalizeKind(st.WorkerKind)
	if k := agent.NormalizeKind(req.Kind); k != "" {
		effKind = k
		if k != agent.NormalizeKind(st.WorkerKind) {
			addHint("--kind", "kind", k)
		}
	}
	baseModel, baseEffort, _ := ResolveWorkerModel(st, effMachine, effKind, "", "")
	if m := strings.TrimSpace(req.Model); m != "" && agent.ModelNameRe.MatchString(m) && m != baseModel {
		addHint("--model", "model", m)
	}
	if ef := strings.TrimSpace(req.Effort); ef != "" && agent.ModelNameRe.MatchString(ef) && ef != baseEffort {
		addHint("--effort", "effort", ef)
	}
	if pid := strings.TrimSpace(req.ProjectID); pid != "" {
		if proj := ResolveProject(st.Projects, pid); proj != nil {
			addHint("--project", "project", proj.ID)
			if p := ProjectPathForMachine(proj, effMachine); p != "" {
				addHint("--cwd", "cwd", p)
			}
		} else {
			addHint("--project", "project", pid)
		}
	}
	if len(hints) == 0 {
		return ""
	}
	if apiMode {
		return "[用户本次指定运行参数（对应 dispatch_work 参数）: " + strings.Join(hints, " ") + "]"
	}
	return "[The human picked where this runs: spawn with " + strings.Join(hints, " ") + "]"
}

// ReplyRequest describes a Corgi outbound reply with optional quick-choice buttons,
// review check, digest replacement, work item linkage, or explicit file attachments.
type ReplyRequest struct {
	Thread            string
	Text              string
	ReID              string
	Options           []string
	StructuredChoices []model.ActionChoice
	Mode              string // single (default) | multi
	ConfirmLabel      string
	Files             []string
	Machine           string
	WorkID            string
	Check             bool
	Digest            bool
	Worker            string
	Category          string
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
	structuredChoices, opts, err := normalizeStructuredChoices(req.StructuredChoices, req.Options)
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

	var (
		quote       *model.Quote
		quoteWorker string
		reEntry     *model.Entry
		reTrigger   *model.Trigger
	)
	if req.ReID != "" {
		if prev, err := e.store.GetEntry(ctx, req.ReID); err == nil {
			reEntry = prev
			who := "你"
			if prev.Role != "user" {
				who = prev.Role
			}
			quote = &model.Quote{Entry: prev.ID, Who: who, Text: oneLineClip(prev.Text, 120)}
			if prev.Role == "event" && prev.Ref != "" && prev.Ref != model.RouterName {
				quoteWorker = prev.Ref
			}
		} else if tr, err := e.store.GetTrigger(ctx, req.ReID); err == nil {
			reTrigger = tr
			quote = &model.Quote{Who: tr.From, Text: oneLineClip(tr.Text, 120)}
			if tr.Source == "worker" && tr.From != "" {
				quoteWorker = tr.From
			}
		}
	}
	// Resolve WorkItem provenance in strict order: explicit --work -> referenced Entry/Trigger -> related worker.
	if workID == "" {
		candidateWI := ""
		if reEntry != nil && strings.TrimSpace(reEntry.WorkItemID) != "" {
			candidateWI = strings.TrimSpace(reEntry.WorkItemID)
		} else if reTrigger != nil && strings.TrimSpace(reTrigger.WorkItemID) != "" {
			candidateWI = strings.TrimSpace(reTrigger.WorkItemID)
		}
		if candidateWI != "" {
			if wi, wErr := e.store.GetWorkItem(ctx, candidateWI); wErr == nil && wi.ClosedAt == nil && (wi.Thread == "" || wi.Thread == thread) {
				workID = wi.ID
			}
		}
	}

	// Resolve bound worker instance (name + machine + session) so delivery cards link directly
	// to the exact Worker that produced the report/artifact, and auto-attach its freshly captured
	// full report snapshot when Router did not pass explicit --file paths.
	relatedWorkerName := strings.TrimSpace(req.Worker)
	if relatedWorkerName == "" {
		relatedWorkerName = quoteWorker
	}
	var relatedWorker *model.Worker
	if relatedWorkerName != "" {
		if wk, wErr := e.store.GetWorker(ctx, relatedWorkerName); wErr == nil {
			relatedWorker = wk
		}
	} else if workID != "" {
		if wi, wErr := e.store.GetWorkItem(ctx, workID); wErr == nil && len(wi.Workers) > 0 {
			for i := len(wi.Workers) - 1; i >= 0; i-- {
				if wk, gErr := e.store.GetWorker(ctx, wi.Workers[i]); gErr == nil {
					relatedWorker = wk
					break
				}
			}
		}
	}
	var workerRef *model.EntryWorkerRef
	if relatedWorker != nil {
		if workID == "" && relatedWorker.WorkItemID != "" {
			if wi, wErr := e.store.GetWorkItem(ctx, relatedWorker.WorkItemID); wErr == nil && wi.ClosedAt == nil && (wi.Thread == "" || wi.Thread == thread) {
				workID = wi.ID
			}
		}
		workerRef = &model.EntryWorkerRef{
			Name:       relatedWorker.Name,
			Machine:    relatedWorker.Machine,
			Session:    relatedWorker.Session,
			Kind:       relatedWorker.Kind,
			Model:      relatedWorker.Model,
			Effort:     relatedWorker.Effort,
			WorkItemID: relatedWorker.WorkItemID,
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

	// Chain of Recall: auto-attach a captured report snapshot ONLY when `--re <report_trigger_id>`
	// explicitly references a same-thread worker report trigger carrying an ArtifactID (and no
	// explicit `--file` was provided). Plain `--worker W` binds WorkerRef / card metadata only so
	// routine status confirmations never leak an older report snapshot into file-delivery flows.
	var autoAttachedReportID string
	var targetReportArtifactID string
	if len(fetchedFiles) == 0 && reTrigger != nil && reTrigger.Source == "worker" &&
		strings.TrimSpace(reTrigger.ArtifactID) != "" &&
		(reTrigger.Thread == "" || reTrigger.Thread == thread) &&
		(strings.TrimSpace(req.Worker) == "" || strings.EqualFold(strings.TrimSpace(req.Worker), strings.TrimSpace(reTrigger.From))) {
		targetReportArtifactID = strings.TrimSpace(reTrigger.ArtifactID)
	}
	if targetReportArtifactID != "" {
		if art, data, rErr := e.ReadArtifactForConsole(ctx, targetReportArtifactID); rErr == nil {
			autoAttachedReportID = art.ID
			artifactSummaries = append(artifactSummaries, model.ArtifactSummary{
				ID:   art.ID,
				Name: art.Name,
				Kind: art.Kind,
				Size: art.Size,
			})
			fetchedFiles = append(fetchedFiles, AttachmentFile{
				ArtifactID: art.ID,
				Path:       art.Name,
				Machine:    art.SourceMachine,
				Name:       art.Name,
				Kind:       art.Kind,
				ShareURL:   e.BuildPreviewURL(ctx, art.ID),
				Data:       data,
			})
			unlinkedEntry := art.EntryID == "" || strings.HasPrefix(art.EntryID, "standalone:")
			if unlinkedEntry || (workID != "" && art.WorkItemID == "") {
				if unlinkedEntry {
					art.EntryID = eid
				}
				if workID != "" && art.WorkItemID == "" {
					art.WorkItemID = workID
				}
				_ = e.store.InsertArtifact(ctx, art)
			}
		}
	}

	hasResolvedAttachment := len(fetchedFiles) > 0 || len(artifactSummaries) > 0 || len(files) > 0
	if !hasResolvedAttachment && claimsAttachmentIncluded(text) {
		cleanupBlobs()
		return nil, fmt.Errorf("reply claims an attachment is included, but 0 files or report snapshots were attached; pass --file /abs/path [--machine M] or --re <report_tID> --worker <W>, or rephrase without claiming an attachment")
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
		WorkerRef:   workerRef,
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
		actionMode := ""
		if hasOpts {
			choices = structuredChoices
			if strings.EqualFold(strings.TrimSpace(req.Mode), model.ActionModeMulti) {
				actionMode = model.ActionModeMulti
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
		confirmLabel := strings.TrimSpace(req.ConfirmLabel)
		if len([]rune(confirmLabel)) > 16 {
			confirmLabel = oneLineClip(confirmLabel, 16)
		}
		action = &model.Action{
			ID:           aid,
			Kind:         kind,
			Thread:       thread,
			EntryID:      entry.ID,
			WorkItemID:   workID,
			TargetWorker: strings.TrimSpace(req.Worker),
			Category:     cat,
			Mode:         actionMode,
			ConfirmLabel: confirmLabel,
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
	if autoAttachedReportID != "" && relatedWorker != nil {
		relatedWorker.LastReportAttachedID = autoAttachedReportID
		_ = e.store.UpsertWorker(ctx, relatedWorker)
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

// TriggerMeta carries durable provenance fields persisted atomically on the initial Trigger insert.
type TriggerMeta struct {
	WorkItemID string
	ArtifactID string
}

// Ingest stores a Trigger after applying deterministic fast-path policies.
func (e *Engine) Ingest(ctx context.Context, source, thread, from, text, idemKey, target, ref string) (*model.Trigger, error) {
	return e.IngestWithMeta(ctx, source, thread, from, text, idemKey, target, ref, TriggerMeta{})
}

// IngestWithMeta atomically persists a Trigger together with its WorkItemID and ArtifactID on first
// insert so Router never observes a partially populated event before a follow-up update runs.
func (e *Engine) IngestWithMeta(ctx context.Context, source, thread, from, text, idemKey, target, ref string, meta TriggerMeta) (*model.Trigger, error) {
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
		ID:         tid,
		Seq:        seq,
		Source:     source,
		Thread:     thread,
		From:       from,
		Text:       text,
		At:         time.Now(),
		Policy:     decision,
		Status:     "queued",
		IdemKey:    idemKey,
		Ref:        ref,
		WorkItemID: strings.TrimSpace(meta.WorkItemID),
		ArtifactID: strings.TrimSpace(meta.ArtifactID),
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

var (
	inlineCodeSpanRe   = regexp.MustCompile("(?s)```.*?```|`[^`\n]+`")
	attachmentClaimRe  = regexp.MustCompile(`(?i)(已重新附上|已附上|已作为附件|作为附件(?:补发|发出|发送|交付|附上)|附件已(?:发|附上|补发|送达)|附在附件|详见附件|见附件|请查收附件|已附带(?:完整|文件|报告|方案|原型)|attached (?:below|here|the file|the report)|see attached|file attached)`)
	attachmentNegateRe = regexp.MustCompile(`(?i)(未附上|没附上|没有附上|未能附上|暂未附上|无法附上|未附带|无附件|漏发|没发出来|未发送附件|不用附|无需附|需要我|需要附|要不要|是否|如需|吗\s*$|not attached|didn'?t attach|no attachment|without attachment|\?\s*$|？\s*$)`)
	clauseSplitterRe   = regexp.MustCompile(`[。！!；;\r\n]+`)
)

// claimsAttachmentIncluded reports whether reply prose affirmatively promises that a file or
// report attachment is included in THIS reply (ignoring code spans, negations, and questions).
func claimsAttachmentIncluded(text string) bool {
	cleaned := inlineCodeSpanRe.ReplaceAllString(text, " ")
	for _, clause := range clauseSplitterRe.Split(cleaned, -1) {
		c := strings.TrimSpace(clause)
		if c == "" {
			continue
		}
		if attachmentNegateRe.MatchString(c) {
			continue
		}
		if attachmentClaimRe.MatchString(c) {
			return true
		}
	}
	return false
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
			if len(out) >= maxActionOptions {
				return nil, fmt.Errorf("too many options (max %d supported without truncation)", maxActionOptions)
			}
			out = append(out, s)
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

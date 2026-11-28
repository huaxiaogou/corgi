package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

var workerNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

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

type screenMark struct {
	hash  string
	since time.Time
	told  bool
}

// Engine is the Super-Agent coordinator orchestrating Router, Workers, Actions, Memory, and Timers.
type Engine struct {
	store    *store.Store
	fleet    *fleet.Fleet
	agents   *agent.Registry
	memory   *MemoryManager
	llm      *LLMClient
	sink     ChannelSink
	logger   *slog.Logger
	corgiDir string
	home     string
	binPath  string
	version  string

	mu                  sync.Mutex
	settings            model.Settings
	policies            []model.Policy
	flagPublicURLs      []string
	artifactHMACSecret  []byte
	routerDispatching   bool
	hubUpdating         bool
	autoUpdater         AutoUpdater
	lastAutoUpdateCheck time.Time
	dream               *dreamRun
	lastScreen          map[string]screenMark
	lastAnswer          map[string]time.Time
	lastTrust           map[string]time.Time
	idleCheckAt         map[string]time.Time
	lastPatrolScan      time.Time
	lastPendingExpire   time.Time
	runCtx              context.Context
	noAgent             map[string]time.Time

	// In-process native API Router runtime state (protected by mu).
	router routerState

	wakeCh                     chan struct{}
	configWriteSem             chan struct{}
	onSettingsApplied          func(model.Settings)
	queueMu                    sync.Mutex
	wtMu                       sync.Mutex
	pausedQueues               map[string]bool
	drainCooldown              map[string]time.Time
	directTurns                map[string][]*pendingDirectTurn
	directBgNotes              []string
	deliveringWorkers          map[string]bool
	watchdogMarks              map[string]*watchdogMark
	constraintRemindAt         map[string]time.Time
	workerTurns                map[string]*workerTurnWatermark
	workerStartupProbeDelay    time.Duration
	workerStartupProbeInterval time.Duration
	workerStartupCheckWindow   time.Duration
	workerTurnSettleDelay      time.Duration

	// Dedicated sub-states with independent mutexes (never nested with e.mu).
	bypass bypassHealState
	gc     workerGCState
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
	eng := &Engine{
		store:              st,
		fleet:              fl,
		agents:             reg,
		memory:             mem,
		llm:                NewLLMClient(),
		sink:               NopSink{},
		logger:             logger,
		corgiDir:           corgiDir,
		home:               home,
		binPath:            binPath,
		settings:           settings,
		policies:           policies,
		lastScreen:         map[string]screenMark{},
		lastAnswer:         map[string]time.Time{},
		lastTrust:          map[string]time.Time{},
		idleCheckAt:        map[string]time.Time{},
		noAgent:            map[string]time.Time{},
		wakeCh:             make(chan struct{}, 1),
		configWriteSem:     make(chan struct{}, 1),
		pausedQueues:       map[string]bool{},
		drainCooldown:      map[string]time.Time{},
		directTurns:        map[string][]*pendingDirectTurn{},
		deliveringWorkers:  map[string]bool{},
		watchdogMarks:      map[string]*watchdogMark{},
		constraintRemindAt: map[string]time.Time{},
		workerTurns:        map[string]*workerTurnWatermark{},
		bypass:             newBypassHealState(),
		gc:                 newWorkerGCState(time.Now()),
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

// Settings returns a deep copy of current Settings.
func (e *Engine) Settings() model.Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	return CloneSettings(e.settings)
}

// Policies returns a deep copy of current Policies.
func (e *Engine) Policies() []model.Policy {
	e.mu.Lock()
	defer e.mu.Unlock()
	return ClonePolicies(e.policies)
}

// MarkActionPushedToWeCom persists that a pending Action card was pushed to WeCom.
func (e *Engine) MarkActionPushedToWeCom(ctx context.Context, actionID string) error {
	return e.store.MarkActionPushedToWeCom(ctx, actionID)
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

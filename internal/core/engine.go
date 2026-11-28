package core

import (
	"context"
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
	workerNameRe              = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	quoteDirectMarkerRe       = regexp.MustCompile(`↳\s*(?:直聊中|直聊|单次直问)`)
	quoteDirectParenWorkerRe  = regexp.MustCompile(`[（(]#([a-z][a-z0-9_-]{0,31})(?:\s|·|[）)]|\.|…|$)`)
	quoteDirectInlineWorkerRe = regexp.MustCompile(`↳\s*(?:直聊中|直聊|单次直问)\s*·\s*#([a-z][a-z0-9_-]{0,31})(?:[^a-z0-9_-]|$)`)
)

// ExtractQuoteWorkerTag parses the target worker handle strictly from dedicated WeCom
// direct-chat markers ("↳ 直聊", "↳ 直聊中", "↳ 单次直问"). Legacy "[#worker]" tags on
// Corgi reports are intentionally ignored so quoting Corgi never leaks straight to a worker.
func ExtractQuoteWorkerTag(quotedText string) string {
	for _, line := range strings.Split(quotedText, "\n") {
		if !quoteDirectMarkerRe.MatchString(line) {
			continue
		}
		if m := quoteDirectParenWorkerRe.FindStringSubmatch(line); len(m) >= 2 {
			return m[1]
		}
		if m := quoteDirectInlineWorkerRe.FindStringSubmatch(line); len(m) >= 2 {
			return m[1]
		}
	}
	return ""
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

	mu                      sync.Mutex
	settings                model.Settings
	policies                []model.Policy
	flagPublicURLs          []string
	artifactHMACSecret      []byte
	routerDispatching       bool
	hubUpdating             bool
	autoUpdater             AutoUpdater
	lastAutoUpdateCheck     time.Time
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
	watchdogMarks              map[string]*watchdogMark
	constraintRemindAt         map[string]time.Time
	workerTurns                map[string]*workerTurnWatermark
	workerStartupProbeDelay    time.Duration
	workerStartupProbeInterval time.Duration
	workerStartupCheckWindow   time.Duration
	workerTurnSettleDelay      time.Duration

	// Universal Worker Process Hibernation GC runtime state (protected by mu).
	startedAt                time.Time
	lastGCScan               time.Time
	gcRunning                bool
	gcClosing                map[string]bool
	gcClosingPane            map[string]string
	workerOps                map[string]int
	workerActivity           map[string]time.Time
	gcFailBackoff            map[string]time.Time
	recentlyClosedPanes      map[string]time.Time
	workerGCInterval         time.Duration
	workerGCGracePeriod      time.Duration
	workerGCActivityCooldown time.Duration
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
		store:               st,
		fleet:               fl,
		agents:              reg,
		memory:              mem,
		llm:                 NewLLMClient(),
		sink:                NopSink{},
		logger:              logger,
		corgiDir:            corgiDir,
		home:                home,
		binPath:             binPath,
		settings:            settings,
		policies:            policies,
		lastScreen:          map[string]screenMark{},
		lastAnswer:          map[string]time.Time{},
		lastTrust:           map[string]time.Time{},
		idleCheckAt:         map[string]time.Time{},
		noAgent:             map[string]time.Time{},
		healingBypass:       map[string]string{},
		bypassCheckedAt:     map[string]time.Time{},
		bypassHealFailAt:    map[string]time.Time{},
		bypassHealedPID:     map[string]uint32{},
		wakeCh:              make(chan struct{}, 1),
		configWriteSem:      make(chan struct{}, 1),
		pausedQueues:        map[string]bool{},
		drainCooldown:       map[string]time.Time{},
		directTurns:         map[string][]*pendingDirectTurn{},
		watchdogMarks:       map[string]*watchdogMark{},
		constraintRemindAt:  map[string]time.Time{},
		workerTurns:         map[string]*workerTurnWatermark{},
		startedAt:           time.Now(),
		gcClosing:           map[string]bool{},
		gcClosingPane:       map[string]string{},
		workerOps:           map[string]int{},
		workerActivity:      map[string]time.Time{},
		gcFailBackoff:       map[string]time.Time{},
		recentlyClosedPanes: map[string]time.Time{},
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

	// Build prompt payload enriched with quote context; one-off dispatch overrides are persisted structurally on Trigger.
	promptText := text
	if strings.TrimSpace(req.PromptContext) != "" {
		promptText = strings.TrimSpace(req.PromptContext) + "\n" + promptText
	} else if quote != nil {
		promptText = fmt.Sprintf("[In reply to: %q]\n%s", quote.Text, promptText)
	}

	// Fast-path quote routing: dedicated direct-chat markers ("↳ 直聊" / "↳ 直聊中" / "↳ 单次直问")
	// direct follow-up straight to an active worker.
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
		ID:       tid,
		Seq:      tseq,
		Source:   source,
		Thread:   thread,
		From:     req.From,
		Text:     promptText,
		At:       now,
		Policy:   decision,
		Status:   "queued",
		IdemKey:  idemKey,
		Ref:      entry.ID,
		Steer:    steer,
		Media:    append([]model.MediaRef(nil), req.Media...),
		Dispatch: e.buildDispatchOverride(req),
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

func (e *Engine) buildDispatchOverride(req SayRequest) *model.DispatchOverride {
	st := e.Settings()
	var ov model.DispatchOverride
	if m := strings.TrimSpace(req.Machine); m != "" {
		resolved := m
		if e.fleet != nil {
			resolved = e.fleet.ResolveMachine(m)
		}
		ov.Machine = orStr(resolved, "hub")
	}
	if k := agent.NormalizeKind(req.Kind); k != "" {
		ov.Kind = k
	}
	if m := strings.TrimSpace(req.Model); m != "" && agent.ModelNameRe.MatchString(m) {
		ov.Model = m
	}
	if ef := strings.TrimSpace(req.Effort); ef != "" && agent.ModelNameRe.MatchString(ef) {
		ov.Effort = ef
	}
	if pid := strings.TrimSpace(req.ProjectID); pid != "" {
		if proj := ResolveProject(st.Projects, pid); proj != nil {
			ov.ProjectID = proj.ID
		} else {
			ov.ProjectID = pid
		}
	}
	if ov == (model.DispatchOverride{}) {
		return nil
	}
	return &ov
}

// ReplyRequest describes a Corgi outbound reply with optional quick-choice buttons,
// review check, digest replacement, work item linkage, or explicit file attachments.
type ReplyRequest struct {
	Thread            string
	Title             string // optional short decision card title (≤20 chars plain text)
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
		actionTitle := oneLineClip(strings.TrimSpace(req.Title), 64)
		if actionTitle == "" {
			actionTitle = oneLineClip(text, 64)
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
			Title:        actionTitle,
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

	e.mu.Lock()
	limit := e.settings.ReplyChars
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

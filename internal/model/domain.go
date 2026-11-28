package model

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	MainThread          = "chat:main"
	RouterName          = "corgi"
	RouterLabel         = "corgi · router"
	DreamName           = "corgi-dream"
	DreamLabel          = "corgi · dream"
	DefaultReplyChars   = 200
	PendingActionMaxAge = 7 * 24 * time.Hour
)

// Quote references an earlier message, worker report, or timer that a reply answers.
type Quote struct {
	Entry  string `json:"entry,omitempty"`
	Worker string `json:"worker,omitempty"`
	Who    string `json:"who"`
	Text   string `json:"text"`
}

// EntryWorkerRef binds a Corgi reply or delivery card to the exact worker instance that produced it
// (machine + session) so follow-up jump/resume never collides if a worker name is later reused.
type EntryWorkerRef struct {
	Name       string `json:"name"`
	Machine    string `json:"machine,omitempty"`
	Session    string `json:"session,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	WorkItemID string `json:"work_item_id,omitempty"`
}

// MediaRef identifies a trusted server-recorded uploaded or WeCom-downloaded media file
// attached to a specific human message.
type MediaRef struct {
	Path    string `json:"path"`
	Machine string `json:"machine,omitempty"`
	Name    string `json:"name,omitempty"`
	MIME    string `json:"mime,omitempty"`
	Size    int64  `json:"size,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// Entry is one human-facing message or event in a conversation thread.
type Entry struct {
	ID          string            `json:"id"`
	Seq         int64             `json:"seq"`
	Role        string            `json:"role"` // user | corgi | event
	Thread      string            `json:"thread"`
	Channel     string            `json:"channel,omitempty"` // web | wecom | cli | system
	Text        string            `json:"text"`
	At          time.Time         `json:"at"`
	Ref         string            `json:"ref,omitempty"`
	Tone        string            `json:"tone,omitempty"` // spawn | done | blocked | info | error
	Re          *Quote            `json:"re,omitempty"`
	ActionID    string            `json:"action_id,omitempty"`
	Options     []string          `json:"options,omitempty"`
	Files       []string          `json:"files,omitempty"`
	FileMachine string            `json:"file_machine,omitempty"`
	Media       []MediaRef        `json:"media,omitempty"`
	Artifacts   []ArtifactSummary `json:"artifacts,omitempty"`
	WorkerRef   *EntryWorkerRef   `json:"worker_ref,omitempty"`
	WorkItemID  string            `json:"work_item_id,omitempty"`
	Check       bool              `json:"check,omitempty"`
	Pinned      bool              `json:"pinned,omitempty"`
	InPlace     bool              `json:"in_place,omitempty"`
}

// ArtifactSummary is the compact, UI-facing descriptor for a Hub-hosted immutable deliverable snapshot.
// ShareToken and Expired are populated dynamically at read/snapshot time and never stored in reply text.
type ArtifactSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"` // html | image | markdown | text | binary
	Size       int64  `json:"size"`
	ShareToken string `json:"share_token,omitempty"`
	Expired    bool   `json:"expired,omitempty"`
}

// Artifact is the durable Hub metadata record for one immutable blob stored under ~/.corgi/artifacts/.
type Artifact struct {
	ID             string     `json:"id"`
	EntryID        string     `json:"entry_id"`
	Ordinal        int        `json:"ordinal"`
	Thread         string     `json:"thread,omitempty"`
	WorkItemID     string     `json:"work_item_id,omitempty"`
	Name           string     `json:"name"`
	Kind           string     `json:"kind"` // html | image | markdown | text | binary
	MIME           string     `json:"mime,omitempty"`
	Size           int64      `json:"size"`
	SourcePath     string     `json:"source_path,omitempty"`
	SourceMachine  string     `json:"source_machine,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	ShareExpiresAt time.Time  `json:"share_expires_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
}

// WorkArtifact records a file deliverable explicitly confirmed as delivered for a WorkItem.
type WorkArtifact struct {
	ArtifactID  string    `json:"artifact_id,omitempty"`
	Name        string    `json:"name,omitempty"`
	Kind        string    `json:"kind,omitempty"`
	Size        int64     `json:"size,omitempty"`
	Path        string    `json:"path"`
	Machine     string    `json:"machine,omitempty"`
	EntryID     string    `json:"entry_id,omitempty"`
	DeliveredAt time.Time `json:"delivered_at,omitempty"`
}

// WorkItem is a Hub-owned durable ledger record for an ongoing user project or deliverable.
// ClosedAt == nil indicates the item is still open.
type WorkItem struct {
	ID        string            `json:"id"`
	Thread    string            `json:"thread"`
	Title     string            `json:"title"`
	Scope     string            `json:"scope,omitempty"` // "" | readonly | prototype | implement
	NextStep  string            `json:"next_step,omitempty"`
	Workers   []string          `json:"workers,omitempty"`
	Branches  []string          `json:"branches,omitempty"`
	Artifacts []WorkArtifact    `json:"artifacts,omitempty"`
	Snapshots []ArtifactSummary `json:"snapshots,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	ClosedAt  *time.Time        `json:"closed_at,omitempty"`
	CloseNote string            `json:"close_note,omitempty"`
}

// SearchHit is one matched user/corgi entry from full-text search.
type SearchHit struct {
	ID      string    `json:"id"`
	Role    string    `json:"role"`
	Thread  string    `json:"thread"`
	At      time.Time `json:"at"`
	Snippet string    `json:"snippet"`
}

// SearchResult is the payload for conversation history search.
type SearchResult struct {
	Query   string      `json:"query"`
	Results []SearchHit `json:"results"`
	Total   int         `json:"total"`
	HasMore bool        `json:"has_more"`
}

// DispatchOverride stores one-off per-message worker dispatch parameters selected in Web Composer.
type DispatchOverride struct {
	Machine   string `json:"machine,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Model     string `json:"model,omitempty"`
	Effort    string `json:"effort,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

// Trigger is one durable event in the Super-Agent inbox.
type Trigger struct {
	ID         string            `json:"id"`
	Seq        int64             `json:"seq"`
	Source     string            `json:"source"` // chat | wecom | timer | worker | router | system
	Thread     string            `json:"thread"`
	From       string            `json:"from,omitempty"`
	Text       string            `json:"text"`
	At         time.Time         `json:"at"`
	Policy     string            `json:"policy"` // respond | candidate | ignore
	Status     string            `json:"status"` // queued | delivered | ignored
	Targets    []string          `json:"targets,omitempty"`
	IdemKey    string            `json:"idem_key,omitempty"`
	Ref        string            `json:"ref,omitempty"`
	Steer      bool              `json:"steer,omitempty"`
	ArtifactID string            `json:"artifact_id,omitempty"`
	WorkItemID string            `json:"work_item_id,omitempty"`
	Media      []MediaRef        `json:"media,omitempty"`
	Dispatch   *DispatchOverride `json:"dispatch,omitempty"`
}

// Thread groups triggers and worker subscriptions for a conversation or WeCom group topic.
type Thread struct {
	ID          string    `json:"id"`
	Title       string    `json:"title,omitempty"`
	Channel     string    `json:"channel,omitempty"` // web | wecom
	ChatID      string    `json:"chat_id,omitempty"` // WeCom group chatid or user id
	Subscribers []string  `json:"subscribers"`
	LastAt      time.Time `json:"last_at"`
}

// Worker represents a managed coding agent spawned by Corgi.
type Worker struct {
	Name                 string     `json:"name"`
	Kind                 string     `json:"kind"` // claude | codex | agy | codebuddy
	Machine              string     `json:"machine,omitempty"`
	Cwd                  string     `json:"cwd"`
	Task                 string     `json:"task"`
	Thread               string     `json:"thread"`
	PaneID               string     `json:"pane_id,omitempty"`
	WorkspaceID          string     `json:"workspace_id,omitempty"`
	CreatedBy            string     `json:"created_by"`
	CreatedAt            time.Time  `json:"created_at"`
	Model                string     `json:"model,omitempty"`
	Effort               string     `json:"effort,omitempty"`
	ModelSource          string     `json:"model_source,omitempty"` // explicit | machine_override | kind_default | cli_default
	Run                  *[2]string `json:"run,omitempty"`
	Title                string     `json:"title,omitempty"`
	Status               string     `json:"status"` // starting | working | waiting | idle | done | blocked | gone | failed
	FaultError           string     `json:"fault_error,omitempty"`
	FaultPaneID          string     `json:"fault_pane_id,omitempty"`
	FaultAt              *time.Time `json:"fault_at,omitempty"`
	HadTurn              bool       `json:"had_turn,omitempty"`
	StartupFailed        bool       `json:"startup_failed,omitempty"`
	Hibernating          bool       `json:"hibernating,omitempty"`
	LastReport           string     `json:"last_report,omitempty"`
	LastReportArtifactID string     `json:"last_report_artifact_id,omitempty"`
	LastReportAttachedID string     `json:"last_report_attached_id,omitempty"`
	LastReportHash       string     `json:"last_report_hash,omitempty"`
	ReportAt             time.Time  `json:"report_at,omitempty"`
	Ended                bool       `json:"ended,omitempty"`
	Session              string     `json:"session,omitempty"`
	ProjectID            string     `json:"project_id,omitempty"`
	ProjectName          string     `json:"project_name,omitempty"`
	RepoRoot             string     `json:"repo_root,omitempty"`
	RepoCommonDir        string     `json:"repo_common_dir,omitempty"`
	WorktreePath         string     `json:"worktree_path,omitempty"`
	WorktreeBranch       string     `json:"worktree_branch,omitempty"`
	WorktreeBase         string     `json:"worktree_base,omitempty"`
	IdleSince            *time.Time `json:"idle_since,omitempty"`
	IdlePatrolAt         *time.Time `json:"idle_patrol_at,omitempty"`
	IdlePatrolCount      int        `json:"idle_patrol_count,omitempty"`
	Keep                 bool       `json:"keep,omitempty"`
	SnoozeUntil          *time.Time `json:"snooze_until,omitempty"`
	WorkItemID           string     `json:"work_item_id,omitempty"`
	Constraint           string     `json:"constraint,omitempty"`
	Mode                 string     `json:"mode,omitempty"`                 // readonly | prototype | implement
	ReadonlyEnforcement  string     `json:"readonly_enforcement,omitempty"` // hard_sandbox | permission_mode | soft_prompt
}

const (
	ActionKindRouterChoice  = "router_choice"
	ActionKindRouterCheck   = "router_check"
	ActionKindWorkerDialog  = "worker_dialog"
	ActionKindPatrolDigest  = "patrol_digest"
	ActionKindConfigConfirm = "config_confirm"

	ActionPending  = "pending"
	ActionResolved = "resolved"
	ActionExpired  = "expired"
	ActionSnoozed  = "snoozed"

	ActionModeSingle = "single"
	ActionModeMulti  = "multi"
)

// ActionChoice is one selectable button on a unified decision card.
type ActionChoice struct {
	Key         string `json:"key"` // "1", "2", "esc", etc.
	Label       string `json:"label"`
	Detail      string `json:"detail,omitempty"`
	Value       string `json:"value,omitempty"` // raw input/cursor index when answering a terminal dialog
	Recommended bool   `json:"recommended,omitempty"`
}

// Action unifies router quick-choice cards, review checks, and blocked worker dialogs
// so Web/PWA and WeCom template cards stay strictly synchronized without double-submission.
type Action struct {
	ID              string         `json:"id"`
	Kind            string         `json:"kind"`
	Thread          string         `json:"thread"`
	EntryID         string         `json:"entry_id,omitempty"`
	WorkItemID      string         `json:"work_item_id,omitempty"`
	TargetWorker    string         `json:"target_worker,omitempty"`
	PaneID          string         `json:"pane_id,omitempty"`
	Title           string         `json:"title"`
	Summary         string         `json:"summary,omitempty"`
	Command         string         `json:"command,omitempty"`
	Mode            string         `json:"mode,omitempty"` // single (default) | multi
	ConfirmLabel    string         `json:"confirm_label,omitempty"`
	Choices         []ActionChoice `json:"choices,omitempty"`
	Status          string         `json:"status"`                     // pending | resolved | expired | snoozed
	Category        string         `json:"category,omitempty"`         // blocked | worktree | decision | stale
	ResolvedBy      string         `json:"resolved_by,omitempty"`      // web | wecom | router | cli | dismiss | expired
	ResolvedChannel string         `json:"resolved_channel,omitempty"` // web | wecom | router | cli
	ExpiredReason   string         `json:"expired_reason,omitempty"`
	ChosenKey       string         `json:"chosen_key,omitempty"`
	ChosenKeys      []string       `json:"chosen_keys,omitempty"`
	ChosenLabel     string         `json:"chosen_label,omitempty"`
	WeComRespCode   string         `json:"wecom_resp_code,omitempty"`
	PushedToWeCom   bool           `json:"pushed_to_wecom,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	ResolvedAt      *time.Time     `json:"resolved_at,omitempty"`
	SnoozeUntil     *time.Time     `json:"snooze_until,omitempty"`
}

// IsMulti reports whether this Action allows selecting multiple options before confirming.
func (a *Action) IsMulti() bool {
	if a == nil || a.Kind == ActionKindWorkerDialog || a.Kind == ActionKindRouterCheck || a.Kind == ActionKindConfigConfirm {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(a.Mode), ActionModeMulti)
}

// IsFastSingle reports whether this Action is a 1-2 choice single-select card that supports
// instant 1-tap resolution without an explicit submit step.
func (a *Action) IsFastSingle() bool {
	if a == nil || a.IsMulti() {
		return false
	}
	return len(a.Choices) <= 2
}

// IsDecision reports whether this Action represents an actual human decision or blocking terminal
// approval (has selectable choices or is a worker_dialog). Lightweight `--check` review reminders
// (ActionKindRouterCheck) return false so they deliver as plain text in WeCom and never inflate
// decision counters or block real decision queues.
func (a *Action) IsDecision() bool {
	if a == nil {
		return false
	}
	if a.Kind == ActionKindWorkerDialog || a.Kind == ActionKindConfigConfirm {
		return true
	}
	if a.Kind == ActionKindRouterCheck {
		return false
	}
	return len(a.Choices) > 0
}

// Timer schedules a one-shot or recurring event into a thread or directly to a worker.
type Timer struct {
	ID      string     `json:"id"`
	Spec    string     `json:"spec"` // in 30m | every 2h | daily 09:00 | weekdays 09:00 | weekly mon,thu 09:00 | at 2026-11-25 09:00
	Thread  string     `json:"thread"`
	Target  string     `json:"target,omitempty"`
	Text    string     `json:"text"`
	NextRun time.Time  `json:"next_run"`
	LastRun *time.Time `json:"last_run,omitempty"`
	Enabled bool       `json:"enabled"`
	Owner   string     `json:"owner,omitempty"`
}

// Policy is the deterministic fast-path filter for an external trigger source.
type Policy struct {
	Source string   `json:"source"`
	Action string   `json:"action"` // respond | candidate | ignore
	Allow  []string `json:"allow,omitempty"`
}

// ModelEffortSpec configures a default model and optional reasoning effort for one agent CLI kind.
type ModelEffortSpec struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// ProjectWorkspace maps one logical project (and its aliases/note) to repository root paths per machine.
// Paths keys use "hub" for the local Hub machine and the machine ID for connected remote nodes.
type ProjectWorkspace struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Aliases []string          `json:"aliases,omitempty"`
	Note    string            `json:"note,omitempty"`
	Paths   map[string]string `json:"paths,omitempty"`
}

// RouterAPISecret stores the API Router credential separately from public Settings,
// bound to the exact HTTPS origin it was configured for.
type RouterAPISecret struct {
	APIKey      string    `json:"api_key"`
	BoundOrigin string    `json:"bound_origin"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Settings holds durable Corgi configuration.
type Settings struct {
	RouterAPIProvider       string                                `json:"router_api_provider,omitempty"` // tokenhub | openai
	RouterAPIBaseURL        string                                `json:"router_api_base_url,omitempty"`
	RouterAPIModel          string                                `json:"router_api_model,omitempty"`
	RouterAPIEffort         string                                `json:"router_api_effort,omitempty"`
	WorkerKind              string                                `json:"worker_kind"`
	WorkerModel             string                                `json:"worker_model,omitempty"`
	WorkerEffort            string                                `json:"worker_effort,omitempty"`
	KindDefaults            map[string]ModelEffortSpec            `json:"kind_defaults,omitempty"`
	MachineKindOverrides    map[string]map[string]ModelEffortSpec `json:"machine_kind_overrides,omitempty"`
	Projects                []ProjectWorkspace                    `json:"projects,omitempty"`
	WorkerCwd               string                                `json:"worker_cwd"`
	WorkerMachine           string                                `json:"worker_machine,omitempty"`
	ReplyChars              int                                   `json:"reply_chars"`
	StallMinutes            int                                   `json:"stall_minutes"`
	IdlePatrolMinutes       int                                   `json:"idle_patrol_minutes,omitempty"`         // 0 = default 30, -1 = off
	ActiveHours             string                                `json:"active_hours,omitempty"`                // "" = default "09:00-22:30", "24h" = always
	StaleArchiveDays        int                                   `json:"stale_archive_days,omitempty"`          // 0 = default 3, -1 = off
	WorkerIdleGCMinutes     int                                   `json:"worker_idle_gc_minutes,omitempty"`      // 0 = default 120, -1 = off (disables all auto worker GC)
	WorkerMaxLivePerMachine int                                   `json:"worker_max_live_per_machine,omitempty"` // 0 = default 6, -1 = unlimited
	MorningDigest           *bool                                 `json:"morning_digest,omitempty"`              // nil = default true
	DreamAt                 string                                `json:"dream_at,omitempty"`                    // "HH:MM", "" = "04:00", "off" = disabled
	AutoUpdate              bool                                  `json:"auto_update,omitempty"`                 // opt-in overnight idle auto-update (default false)
	PublicURL               string                                `json:"public_url,omitempty"`                  // optional canonical Hub base URL for outbound preview links
	NotifyLevel             string                                `json:"notify_level,omitempty"`                // quiet | normal
	AskPermissions          bool                                  `json:"ask_permissions,omitempty"`
	Paused                  bool                                  `json:"paused,omitempty"`
	WeComEnabled            bool                                  `json:"wecom_enabled,omitempty"`
	WeComBotID              string                                `json:"wecom_bot_id,omitempty"`
	WeComSecret             string                                `json:"wecom_secret,omitempty"`
	WeComAllowUsers         []string                              `json:"wecom_allow_users,omitempty"`
	WeComDefaultTo          string                                `json:"wecom_default_to,omitempty"` // default single-chat userid for proactive push
}

// MorningDigestEnabled reports whether daily morning summary digests are active (defaults to true).
func (s Settings) MorningDigestEnabled() bool {
	return s.MorningDigest == nil || *s.MorningDigest
}

// EffectiveIdlePatrolMinutes returns configured first-patrol threshold (default 30, <=0 means disabled).
func (s Settings) EffectiveIdlePatrolMinutes() int {
	if s.IdlePatrolMinutes < 0 {
		return 0
	}
	if s.IdlePatrolMinutes == 0 {
		return 30
	}
	return s.IdlePatrolMinutes
}

// EffectiveStaleArchiveDays returns configured stale-session prompt threshold in days (default 3, <=0 means disabled).
func (s Settings) EffectiveStaleArchiveDays() int {
	if s.StaleArchiveDays < 0 {
		return 0
	}
	if s.StaleArchiveDays == 0 {
		return 3
	}
	return s.StaleArchiveDays
}

// EffectiveWorkerIdleGCMinutes returns the idle hibernation threshold in minutes (default 120, <=0 disables all auto GC).
func (s Settings) EffectiveWorkerIdleGCMinutes() int {
	if s.WorkerIdleGCMinutes < 0 {
		return 0
	}
	if s.WorkerIdleGCMinutes == 0 {
		return 120
	}
	return s.WorkerIdleGCMinutes
}

// EffectiveWorkerMaxLivePerMachine returns the per-machine soft cap on live Corgi-dispatched workers (default 6, <=0 means unlimited).
func (s Settings) EffectiveWorkerMaxLivePerMachine() int {
	if s.WorkerMaxLivePerMachine < 0 {
		return 0
	}
	if s.WorkerMaxLivePerMachine == 0 {
		return 6
	}
	return s.WorkerMaxLivePerMachine
}

// EffectiveActiveHours returns normalized active hours (defaults to "09:00-22:30").
func (s Settings) EffectiveActiveHours() string {
	if s.ActiveHours == "" {
		return "09:00-22:30"
	}
	return s.ActiveHours
}

// DefaultSettings returns sane defaults for a fresh installation.
func DefaultSettings(_ string) Settings {
	on := true
	return Settings{
		RouterAPIProvider:       "tokenhub",
		RouterAPIModel:          "hy4",
		RouterAPIEffort:         "high",
		WorkerKind:              "claude",
		WorkerCwd:               "~",
		ReplyChars:              DefaultReplyChars,
		StallMinutes:            12,
		IdlePatrolMinutes:       30,
		ActiveHours:             "09:00-22:30",
		StaleArchiveDays:        3,
		WorkerIdleGCMinutes:     120,
		WorkerMaxLivePerMachine: 6,
		MorningDigest:           &on,
		DreamAt:                 "04:00",
		NotifyLevel:             "quiet",
	}
}

// DefaultPolicies returns safe fast-path policies for external sources.
func DefaultPolicies() []Policy {
	return []Policy{
		{Source: "wecom", Action: "respond"},
		{Source: "webhook:*", Action: "candidate"},
	}
}

// Device is a paired browser/phone or remote node computer.
// TokenHash stores only the SHA-256 hex digest; plaintext tokens are never persisted.
type Device struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // browser | machine
	Name      string    `json:"name"`
	OS        string    `json:"os,omitempty"`
	TokenHash string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
}

// PathInspectResult reports local/remote filesystem inspection details for a candidate project directory.
type PathInspectResult struct {
	Exists       bool   `json:"exists"`
	IsDir        bool   `json:"is_dir"`
	IsGit        bool   `json:"is_git"`
	ResolvedPath string `json:"resolved_path,omitempty"`
	DisplayPath  string `json:"display_path,omitempty"`
}

// ConfigProposal stores a pending L2 dangerous configuration change awaiting explicit human confirmation.
type ConfigProposal struct {
	ActionID   string          `json:"action_id"`
	Op         string          `json:"op"` // delete_project | remove_project_path | delete_policy | disable_ask_permissions
	Summary    string          `json:"summary"`
	BeforeHash string          `json:"before_hash"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
	ExpiresAt  time.Time       `json:"expires_at"`
}

// ConfigAuditEntry is one redacted configuration change summary kept in the bounded KV ring.
type ConfigAuditEntry struct {
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"`
	Action  string    `json:"action"`
	Summary string    `json:"summary"`
}

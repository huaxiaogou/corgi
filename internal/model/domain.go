package model

import "time"

const (
	MainThread  = "chat:main"
	RouterName  = "corgi"
	RouterLabel = "corgi · router"
	DreamName   = "corgi-dream"
	DreamLabel  = "corgi · dream"
)

// Quote references an earlier message, worker report, or timer that a reply answers.
type Quote struct {
	Entry  string `json:"entry,omitempty"`
	Worker string `json:"worker,omitempty"`
	Who    string `json:"who"`
	Text   string `json:"text"`
}

// Entry is one human-facing message or event in a conversation thread.
type Entry struct {
	ID       string    `json:"id"`
	Seq      int64     `json:"seq"`
	Role     string    `json:"role"` // user | corgi | event
	Thread   string    `json:"thread"`
	Channel  string    `json:"channel,omitempty"` // web | wecom | cli | system
	Text     string    `json:"text"`
	At       time.Time `json:"at"`
	Ref      string    `json:"ref,omitempty"`
	Tone     string    `json:"tone,omitempty"` // spawn | done | blocked | info | error
	Re       *Quote    `json:"re,omitempty"`
	ActionID string    `json:"action_id,omitempty"`
	Options  []string  `json:"options,omitempty"`
	Check    bool      `json:"check,omitempty"`
	Pinned   bool      `json:"pinned,omitempty"`
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

// Trigger is one durable event in the Super-Agent inbox.
type Trigger struct {
	ID      string    `json:"id"`
	Seq     int64     `json:"seq"`
	Source  string    `json:"source"` // chat | wecom | timer | worker | router | system
	Thread  string    `json:"thread"`
	From    string    `json:"from,omitempty"`
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
	Policy  string    `json:"policy"` // respond | candidate | ignore
	Status  string    `json:"status"` // queued | delivered | ignored
	Targets []string  `json:"targets,omitempty"`
	IdemKey string    `json:"idem_key,omitempty"`
	Ref     string    `json:"ref,omitempty"`
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
	Name        string     `json:"name"`
	Kind        string     `json:"kind"` // claude | codex | agy | codebuddy
	Machine     string     `json:"machine,omitempty"`
	Cwd         string     `json:"cwd"`
	Task        string     `json:"task"`
	Thread      string     `json:"thread"`
	PaneID      string     `json:"pane_id,omitempty"`
	WorkspaceID string     `json:"workspace_id,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	Model       string     `json:"model,omitempty"`
	Effort      string     `json:"effort,omitempty"`
	Run         *[2]string `json:"run,omitempty"`
	Title       string     `json:"title,omitempty"`
	Status      string     `json:"status"` // starting | working | waiting | idle | done | blocked | gone | failed
	LastReport  string     `json:"last_report,omitempty"`
	ReportAt    time.Time  `json:"report_at,omitempty"`
	Ended       bool       `json:"ended,omitempty"`
	Session     string     `json:"session,omitempty"`
}

const (
	ActionKindRouterChoice = "router_choice"
	ActionKindRouterCheck  = "router_check"
	ActionKindWorkerDialog = "worker_dialog"

	ActionPending  = "pending"
	ActionResolved = "resolved"
	ActionExpired  = "expired"
)

// ActionChoice is one selectable button on a unified decision card.
type ActionChoice struct {
	Key    string `json:"key"` // "1", "2", "esc", etc.
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
	Value  string `json:"value,omitempty"` // raw input/cursor index when answering a terminal dialog
}

// Action unifies router quick-choice cards, review checks, and blocked worker dialogs
// so Web/PWA and WeCom template cards stay strictly synchronized without double-submission.
type Action struct {
	ID            string         `json:"id"`
	Kind          string         `json:"kind"`
	Thread        string         `json:"thread"`
	EntryID       string         `json:"entry_id,omitempty"`
	TargetWorker  string         `json:"target_worker,omitempty"`
	PaneID        string         `json:"pane_id,omitempty"`
	Title         string         `json:"title"`
	Summary       string         `json:"summary,omitempty"`
	Command       string         `json:"command,omitempty"`
	Choices       []ActionChoice `json:"choices,omitempty"`
	Status        string         `json:"status"`                // pending | resolved | expired
	ResolvedBy    string         `json:"resolved_by,omitempty"` // web | wecom | router | cli
	ChosenKey     string         `json:"chosen_key,omitempty"`
	ChosenLabel   string         `json:"chosen_label,omitempty"`
	WeComRespCode string         `json:"wecom_resp_code,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	ResolvedAt    *time.Time     `json:"resolved_at,omitempty"`
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

// Settings holds durable Corgi configuration.
type Settings struct {
	RouterKind      string   `json:"router_kind"`
	RouterModel     string   `json:"router_model,omitempty"`
	RouterEffort    string   `json:"router_effort,omitempty"`
	WorkerKind      string   `json:"worker_kind"`
	WorkerModel     string   `json:"worker_model,omitempty"`
	WorkerEffort    string   `json:"worker_effort,omitempty"`
	WorkerCwd       string   `json:"worker_cwd"`
	WorkerMachine   string   `json:"worker_machine,omitempty"`
	ReplyChars      int      `json:"reply_chars"`
	StallMinutes    int      `json:"stall_minutes"`
	RotatePct       int      `json:"rotate_pct,omitempty"`   // 0 = default 60, -1 = never
	DreamAt         string   `json:"dream_at,omitempty"`     // "HH:MM", "" = "04:00", "off" = disabled
	NotifyLevel     string   `json:"notify_level,omitempty"` // quiet | normal
	AskPermissions  bool     `json:"ask_permissions,omitempty"`
	Paused          bool     `json:"paused,omitempty"`
	WeComEnabled    bool     `json:"wecom_enabled,omitempty"`
	WeComBotID      string   `json:"wecom_bot_id,omitempty"`
	WeComSecret     string   `json:"wecom_secret,omitempty"`
	WeComAllowUsers []string `json:"wecom_allow_users,omitempty"`
	WeComDefaultTo  string   `json:"wecom_default_to,omitempty"` // default single-chat userid for proactive push
}

// DefaultSettings returns sane defaults for a fresh installation.
func DefaultSettings(_ string) Settings {
	return Settings{
		RouterKind:   "claude",
		RouterEffort: "low",
		WorkerKind:   "claude",
		WorkerCwd:    "~",
		ReplyChars:   120,
		StallMinutes: 12,
		RotatePct:    60,
		DreamAt:      "04:00",
		NotifyLevel:  "quiet",
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

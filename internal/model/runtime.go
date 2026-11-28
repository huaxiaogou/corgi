package model

import "time"

// Machine represents either the local hub host or a connected remote node.
type Machine struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	OS      string    `json:"os,omitempty"`
	Version string    `json:"version,omitempty"`
	Agents  []string  `json:"agents"`
	Home    string    `json:"home,omitempty"`
	Local   bool      `json:"local"`
	Online  bool      `json:"online"`
	Since   time.Time `json:"since,omitempty"`
}

// Workspace is one herdr workspace.
type Workspace struct {
	ID          string `json:"workspace_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"`
	AgentStatus string `json:"agent_status"`
	PaneCount   int    `json:"pane_count"`
	Focused     bool   `json:"focused"`
}

// Pane is one herdr terminal pane.
type Pane struct {
	ID            string  `json:"pane_id"`
	WorkspaceID   string  `json:"workspace_id"`
	TabID         string  `json:"tab_id"`
	Agent         *string `json:"agent"`
	DisplayAgent  *string `json:"display_agent"`
	AgentStatus   string  `json:"agent_status"`
	Cwd           *string `json:"cwd"`
	ForegroundCwd *string `json:"foreground_cwd"`
	Title         *string `json:"terminal_title_stripped"`
	Revision      uint64  `json:"revision"`
	Focused       bool    `json:"focused"`
}

// AgentSession identifies the agent's on-disk conversation reported by herdr.
type AgentSession struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Agent is one managed coding agent reported by herdr.
type Agent struct {
	Name          *string       `json:"name"`
	PaneID        string        `json:"pane_id"`
	WorkspaceID   string        `json:"workspace_id"`
	Agent         *string       `json:"agent"`
	DisplayAgent  *string       `json:"display_agent"`
	AgentStatus   string        `json:"agent_status"`
	Cwd           *string       `json:"cwd"`
	Ready         bool          `json:"interactive_ready"`
	LaunchPending bool          `json:"launch_pending"`
	StateSeq      uint64        `json:"state_change_seq"`
	TerminalTitle string        `json:"terminal_title_stripped"`
	Session       *AgentSession `json:"agent_session"`
}

// ReadResult is returned by herdr pane.read.
type ReadResult struct {
	Text      string `json:"text"`
	Revision  uint64 `json:"revision"`
	Truncated bool   `json:"truncated"`
}

// Rect describes terminal dimensions.
type Rect struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// AgentView is the UI-facing representation of a live pane (agent or shell).
type AgentView struct {
	PaneID              string     `json:"pane_id"`
	WorkspaceID         string     `json:"workspace_id"`
	WorkspaceLabel      string     `json:"workspace_label"`
	Name                string     `json:"name"`
	Kind                string     `json:"kind"`
	Status              string     `json:"status"`
	Cwd                 string     `json:"cwd"`
	Title               string     `json:"title"`
	Topic               string     `json:"topic,omitempty"`
	Session             string     `json:"session,omitempty"`
	IsAgent             bool       `json:"is_agent"`
	Machine             string     `json:"machine"`
	Run                 *[2]string `json:"run,omitempty"`
	RepoRoot            string     `json:"repo_root,omitempty"`
	WorktreeBranch      string     `json:"worktree_branch,omitempty"`
	IdleSince           *time.Time `json:"idle_since,omitempty"`
	Keep                bool       `json:"keep,omitempty"`
	CreatedBy           string     `json:"created_by,omitempty"`
	Constraint          string     `json:"constraint,omitempty"`
	Mode                string     `json:"mode,omitempty"`
	ReadonlyEnforcement string     `json:"readonly_enforcement,omitempty"`
}

// QueuedPrompt is one durable human prompt waiting for a busy agent pane or main Corgi chat thread.
type QueuedPrompt struct {
	ID          string     `json:"id"`
	Seq         int64      `json:"seq,omitempty"`
	Scope       string     `json:"scope"` // pane_id (for single agent) or "router:<thread>" (for main Corgi chat)
	PaneID      string     `json:"pane_id,omitempty"`
	Thread      string     `json:"thread,omitempty"`
	Text        string     `json:"text"`
	ReEntryID   string     `json:"re,omitempty"`
	InPlace     bool       `json:"in_place,omitempty"`
	Machine     string     `json:"machine,omitempty"`
	Kind        string     `json:"kind,omitempty"`
	Model       string     `json:"model,omitempty"`
	Effort      string     `json:"effort,omitempty"`
	ProjectID   string     `json:"project_id,omitempty"`
	Media       []MediaRef `json:"media,omitempty"`
	ClientMsgID string     `json:"client_msg_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// PaneState is the aggregated fleet pane snapshot pushed over SSE.
type PaneState struct {
	OK            bool                       `json:"ok"`
	Error         string                     `json:"error,omitempty"`
	Agents        []AgentView                `json:"agents"`
	Shells        []AgentView                `json:"shells"`
	Machines      []Machine                  `json:"machines"`
	QueuedPrompts map[string][]*QueuedPrompt `json:"queued_prompts,omitempty"`
	PausedQueues  map[string]bool            `json:"paused_queues,omitempty"`
}

// ModelOpt describes one selectable model exposed by an agent CLI.
// Note: Efforts omits omitempty so nil ("inherit catalog default") and [] ("explicitly no effort") serialize distinctly.
type ModelOpt struct {
	ID      string   `json:"id"`
	Label   string   `json:"label,omitempty"`
	Note    string   `json:"note,omitempty"`
	Efforts []string `json:"efforts"`
}

// ModelCatalog lists discovered models and thinking efforts for an agent kind.
type ModelCatalog struct {
	Kind          string     `json:"kind"`
	Models        []ModelOpt `json:"models"`
	Efforts       []string   `json:"efforts"`
	Default       string     `json:"default,omitempty"`
	DefaultEffort string     `json:"default_effort,omitempty"`
}

// ContextUsage reports token fill of an agent's context window.
type ContextUsage struct {
	Used   int `json:"used"`
	Window int `json:"window"`
}

// Pct returns integer usage percentage [0..100+].
func (u ContextUsage) Pct() int {
	if u.Window <= 0 || u.Used <= 0 {
		return 0
	}
	return u.Used * 100 / u.Window
}

// SubagentCall carries structured metadata for subagent/swarm delegation tool calls.
type SubagentCall struct {
	Type   string `json:"type,omitempty"`
	Role   string `json:"role,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	Model  string `json:"model,omitempty"`
}

// Message is a structured turn parsed from an agent's JSONL transcript.
type Message struct {
	Kind     string        `json:"kind"` // user | assistant | thinking | tool | ask
	Text     string        `json:"text,omitempty"`
	Name     string        `json:"name,omitempty"`
	Options  []string      `json:"options,omitempty"`
	Input    string        `json:"input,omitempty"`
	Detail   string        `json:"detail,omitempty"`
	Subagent *SubagentCall `json:"subagent,omitempty"`
	Output   string        `json:"output,omitempty"`
	Error    bool          `json:"error,omitempty"`
	At       int64         `json:"-"` // byte offset in transcript log for pagination
}

// MsgPatch delivers an incremental transcript update over SSE.
type MsgPatch struct {
	Base    int        `json:"base"`
	From    int        `json:"from"`
	Msgs    []*Message `json:"msgs"`
	Earlier int64      `json:"earlier,omitempty"`
}

// PaneSession carries per-pane topic title, session ID, and launch model/effort from a machine.
type PaneSession struct {
	Title   string     `json:"title,omitempty"`
	Session string     `json:"session,omitempty"`
	Run     *[2]string `json:"run,omitempty"`
}

// Choice is one selectable option parsed from a terminal confirmation dialog.
type Choice struct {
	Key    string   `json:"key"`
	Label  string   `json:"label"`
	Keys   []string `json:"keys,omitempty"` // explicit key sequence (e.g. ["down","right","enter"]) when menu requires cursor navigation
	Cursor int      `json:"cursor,omitempty"`
	Active bool     `json:"active,omitempty"`
}

// Approval is a parsed interactive dialog blocking a terminal agent.
type Approval struct {
	Title   string   `json:"title"`
	Detail  string   `json:"detail,omitempty"`
	Command string   `json:"command,omitempty"`
	Choices []Choice `json:"choices"`
	Style   string   `json:"style"` // number | cursor
	Raw     string   `json:"raw,omitempty"`
}

// ToolStats summarizes recent tool results within the current worker turn for thrash detection.
type ToolStats struct {
	N              int   `json:"n"`
	Failed         int   `json:"failed"`
	TrailingFailed int   `json:"trailing_failed"`
	LastFailureAt  int64 `json:"last_failure_at,omitempty"`
	LastUserAt     int64 `json:"last_user_at,omitempty"`
}

// EndedSession remembers a closed agent session for review or --resume.
type EndedSession struct {
	ID                  string    `json:"id"`
	Kind                string    `json:"kind"`
	Machine             string    `json:"machine,omitempty"`
	Cwd                 string    `json:"cwd"`
	Title               string    `json:"title,omitempty"`
	Name                string    `json:"name,omitempty"`
	Session             string    `json:"session"`
	WorkItemID          string    `json:"work_item_id,omitempty"`
	Model               string    `json:"model,omitempty"`
	Effort              string    `json:"effort,omitempty"`
	Constraint          string    `json:"constraint,omitempty"`
	Mode                string    `json:"mode,omitempty"`
	ReadonlyEnforcement string    `json:"readonly_enforcement,omitempty"`
	CreatedBy           string    `json:"created_by,omitempty"`
	Thread              string    `json:"thread,omitempty"`
	Keep                bool      `json:"keep,omitempty"`
	RepoRoot            string    `json:"repo_root,omitempty"`
	RepoCommonDir       string    `json:"repo_common_dir,omitempty"`
	WorktreePath        string    `json:"worktree_path,omitempty"`
	WorktreeBranch      string    `json:"worktree_branch,omitempty"`
	WorktreeBase        string    `json:"worktree_base,omitempty"`
	Subscribers         []string  `json:"subscribers,omitempty"`
	EndedAt             time.Time `json:"ended_at"`
}

// RouterView summarizes the live state of Corgi's native in-process API Router.
type RouterView struct {
	Alive    bool   `json:"alive"`
	Status   string `json:"status"`
	Kind     string `json:"kind"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	Dreaming bool   `json:"dreaming,omitempty"`
	Replied  bool   `json:"replied,omitempty"`
}

// FollowView summarizes the next upcoming self-wakeup timer for the UI header.
type FollowView struct {
	Next     time.Time `json:"next"`
	NextText string    `json:"next_text"`
}

// WeComFocusView exposes an active (non-expired) WeCom direct-chat focus state to Web clients.
type WeComFocusView struct {
	Key        string    `json:"key"`
	Scope      string    `json:"scope"` // single | group
	Worker     string    `json:"worker"`
	Title      string    `json:"title,omitempty"`
	Machine    string    `json:"machine,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	Session    string    `json:"session,omitempty"`
	WorkItemID string    `json:"work_item_id,omitempty"`
	EnteredAt  time.Time `json:"entered_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// WorkerDispatchDefaults exposes non-secret worker dispatch settings and projects directly in SuperView
// so the chat composer always displays the exact active global default in real time.
type WorkerDispatchDefaults struct {
	WorkerMachine        string                                `json:"worker_machine,omitempty"`
	WorkerKind           string                                `json:"worker_kind"`
	WorkerModel          string                                `json:"worker_model,omitempty"`
	WorkerEffort         string                                `json:"worker_effort,omitempty"`
	WorkerCwd            string                                `json:"worker_cwd,omitempty"`
	KindDefaults         map[string]ModelEffortSpec            `json:"kind_defaults,omitempty"`
	MachineKindOverrides map[string]map[string]ModelEffortSpec `json:"machine_kind_overrides,omitempty"`
	Projects             []ProjectWorkspace                    `json:"projects,omitempty"`
}

// SuperView is the snapshot of Super-Agent state sent to Web clients.
type SuperView struct {
	Router           RouterView              `json:"router"`
	DispatchDefaults *WorkerDispatchDefaults `json:"dispatch_defaults,omitempty"`
	Entries          []*Entry                `json:"entries"`
	Earlier          bool                    `json:"earlier,omitempty"`
	Actions          []*Action               `json:"actions"`
	ActivityNotices  []*Action               `json:"activity_notices,omitempty"`
	ResolvedActions  map[string]*Action      `json:"resolved_actions,omitempty"`
	SnoozedActions   int                     `json:"snoozed_actions,omitempty"`
	Pinned           []*Entry                `json:"pinned"`
	Workers          []*Worker               `json:"workers"`
	WorkItems        []*WorkItem             `json:"work_items"`
	ClosedWorkCount  int                     `json:"closed_work_count,omitempty"`
	Threads          []*Thread               `json:"threads"`
	Queued           int                     `json:"queued"`
	QueuedRouter     int                     `json:"queued_router"`
	QueuedChat       []*QueuedPrompt         `json:"queued_chat,omitempty"`
	ChatQueuePaused  bool                    `json:"chat_queue_paused,omitempty"`
	Paused           bool                    `json:"paused"`
	WeComOnline      bool                    `json:"wecom_online"`
	WeComFocus       *WeComFocusView         `json:"wecom_focus,omitempty"`
	Follow           *FollowView             `json:"follow,omitempty"`
}

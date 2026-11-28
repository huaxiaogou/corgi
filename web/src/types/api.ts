export interface Quote {
  entry?: string;
  worker?: string;
  who: string;
  text: string;
}

export interface ArtifactSummary {
  id: string;
  name: string;
  kind: "html" | "image" | "markdown" | "text" | "binary" | string;
  size: number;
  share_token?: string;
  expired?: boolean;
}

export interface EntryWorkerRef {
  name: string;
  machine?: string;
  session?: string;
  kind?: string;
  model?: string;
  effort?: string;
  work_item_id?: string;
}

export type FileDrawerTarget =
  | { artifactId: string; name: string; kind: string; shareToken?: string; workerRef?: EntryWorkerRef }
  | { path: string; machine?: string };

export interface Entry {
  id: string;
  seq: number;
  role: "user" | "corgi" | "event";
  thread: string;
  channel?: string;
  text: string;
  at: string;
  ref?: string;
  tone?: string;
  re?: Quote;
  action_id?: string;
  options?: string[];
  files?: string[];
  file_machine?: string;
  artifacts?: ArtifactSummary[];
  worker_ref?: EntryWorkerRef;
  work_item_id?: string;
  check?: boolean;
  pinned?: boolean;
  in_place?: boolean;
}

export interface ActionChoice {
  key: string;
  label: string;
  detail?: string;
  value?: string;
  recommended?: boolean;
}

export interface Action {
  id: string;
  kind: "router_choice" | "router_check" | "worker_dialog" | "patrol_digest";
  thread: string;
  entry_id?: string;
  work_item_id?: string;
  target_worker?: string;
  pane_id?: string;
  title: string;
  summary?: string;
  command?: string;
  mode?: "single" | "multi";
  confirm_label?: string;
  choices?: ActionChoice[];
  status: "pending" | "resolved" | "expired" | "snoozed";
  category?: "blocked" | "worktree" | "decision" | "stale" | "check" | "fyi" | string;
  resolved_by?: string;
  resolved_channel?: string;
  chosen_key?: string;
  chosen_keys?: string[];
  chosen_label?: string;
  pushed_to_wecom?: boolean;
  created_at: string;
  resolved_at?: string;
  snooze_until?: string;
}

export interface Worker {
  name: string;
  kind: string;
  machine?: string;
  cwd: string;
  task: string;
  thread: string;
  pane_id?: string;
  workspace_id?: string;
  created_by?: string;
  created_at: string;
  model?: string;
  effort?: string;
  model_source?: "explicit" | "machine_override" | "kind_default" | "cli_default" | string;
  run?: [string, string];
  title?: string;
  status: string;
  fault_error?: string;
  last_report?: string;
  last_report_artifact_id?: string;
  ended?: boolean;
  session?: string;
  project_id?: string;
  project_name?: string;
  repo_root?: string;
  worktree_path?: string;
  worktree_branch?: string;
  idle_since?: string;
  keep?: boolean;
  snooze_until?: string;
  work_item_id?: string;
  constraint?: string;
  mode?: "readonly" | "prototype" | "implement" | string;
  readonly_enforcement?: "hard_sandbox" | "permission_mode" | "soft_prompt" | string;
}

export interface WorkArtifact {
  artifact_id?: string;
  name?: string;
  kind?: string;
  size?: number;
  path: string;
  machine?: string;
  entry_id?: string;
  delivered_at?: string;
}

export interface WorkItem {
  id: string;
  thread: string;
  title: string;
  next_step?: string;
  latest_update?: string;
  latest_update_at?: string;
  latest_update_entry_id?: string;
  workers?: string[];
  branches?: string[];
  artifacts?: WorkArtifact[];
  snapshots?: ArtifactSummary[];
  created_at: string;
  updated_at: string;
  closed_at?: string;
  close_note?: string;
}

export interface Thread {
  id: string;
  title?: string;
  channel?: string;
  chat_id?: string;
  subscribers: string[];
  last_at: string;
}

export interface MediaRef {
  path: string;
  machine?: string;
  name?: string;
  mime?: string;
  size?: number;
}

export interface RouterContextView {
  epoch: number;
  active_turns: number;
  est_tokens: number;
  last_prompt_tokens?: number;
  last_cached_tokens?: number;
  budget_tokens: number;
  compaction_count?: number;
  compacting?: boolean;
}

export interface RouterContextGroup {
  work_item_id: string;
  title: string;
  closed?: boolean;
  turn_count: number;
  est_tokens: number;
}

export interface RouterContextTurnPreview {
  id: string;
  seq: number;
  kind: "turn" | "summary" | string;
  source?: string;
  work_item_id?: string;
  work_item_ids?: string[];
  work_item_title?: string;
  user_preview: string;
  assistant_preview: string;
  est_tokens: number;
  created_at: string;
}

export interface RouterContextInspect {
  thread: string;
  summary: RouterContextView;
  groups: RouterContextGroup[];
  turns: RouterContextTurnPreview[];
}

export interface RouterContextPruneResult {
  ok: boolean;
  mode: string;
  dropped_turns?: number;
  freed_est_tokens?: number;
  compacting?: boolean;
  message: string;
  summary: RouterContextView;
}

export interface RouterLiveView {
  active: boolean;
  thread?: string;
  trigger_id?: string;
  progress?: string[];
  draft_text?: string;
  thinking_since?: string;
}

export interface RouterView {
  alive: boolean;
  status: string;
  kind: string;
  model?: string;
  effort?: string;
  dreaming?: boolean;
  replied?: boolean;
  live?: RouterLiveView;
  context?: RouterContextView;
}

export interface FollowView {
  next: string;
  next_text: string;
}

export interface WeComFocusView {
  key: string;
  scope: "single" | "group" | string;
  worker: string;
  title?: string;
  machine?: string;
  kind?: string;
  session?: string;
  work_item_id?: string;
  entered_at: string;
  updated_at: string;
  expires_at: string;
}

export interface QueuedPrompt {
  id: string;
  seq?: number;
  scope?: string;
  pane_id?: string;
  thread?: string;
  text: string;
  re?: string;
  in_place?: boolean;
  machine?: string;
  kind?: string;
  model?: string;
  effort?: string;
  created_at: string;
}

export interface ModelEffortSpec {
  model?: string;
  effort?: string;
}

export interface ProjectWorkspace {
  id: string;
  name: string;
  aliases?: string[];
  note?: string;
  paths?: Record<string, string>;
}

export interface WorkerDispatchDefaults {
  worker_machine?: string;
  worker_kind: string;
  worker_model?: string;
  worker_effort?: string;
  worker_cwd?: string;
  kind_defaults?: Record<string, ModelEffortSpec>;
  machine_kind_overrides?: Record<string, Record<string, ModelEffortSpec>>;
  projects?: ProjectWorkspace[];
}

export interface SuperView {
  router: RouterView;
  dispatch_defaults?: WorkerDispatchDefaults;
  entries: Entry[];
  earlier?: boolean;
  actions: Action[];
  activity_notices?: Action[];
  resolved_actions?: Record<string, Action>;
  snoozed_actions?: number;
  pinned: Entry[];
  workers: Worker[];
  work_items?: WorkItem[];
  closed_work_count?: number;
  threads: Thread[];
  queued: number;
  queued_router: number;
  queued_chat?: QueuedPrompt[];
  chat_queue_paused?: boolean;
  paused: boolean;
  wecom_online: boolean;
  wecom_focus?: WeComFocusView;
  follow?: FollowView;
}

export interface Machine {
  id: string;
  name: string;
  os?: string;
  version?: string;
  agents: string[];
  home?: string;
  local: boolean;
  online: boolean;
}

export interface AgentView {
  pane_id: string;
  workspace_id: string;
  workspace_label: string;
  name: string;
  kind: string;
  status: string;
  cwd: string;
  title: string;
  topic?: string;
  session?: string;
  is_agent: boolean;
  machine: string;
  run?: [string, string];
  repo_root?: string;
  worktree_branch?: string;
  idle_since?: string;
  keep?: boolean;
  created_by?: string;
  constraint?: string;
  mode?: "readonly" | "prototype" | "implement" | string;
  readonly_enforcement?: "hard_sandbox" | "permission_mode" | "soft_prompt" | string;
}

export interface PaneState {
  ok: boolean;
  error?: string;
  agents: AgentView[];
  shells: AgentView[];
  machines: Machine[];
  queued_prompts?: Record<string, QueuedPrompt[]>;
  paused_queues?: Record<string, boolean>;
}

export interface ModelCatalog {
  kind: string;
  models: { id: string; label?: string; note?: string; efforts?: string[] | null }[];
  efforts: string[];
  default?: string;
  default_effort?: string;
}

export interface Timer {
  id: string;
  spec: string;
  thread: string;
  target?: string;
  text: string;
  next_run: string;
  enabled: boolean;
}

export interface Policy {
  source: string;
  action: "respond" | "candidate" | "ignore";
  allow?: string[];
}

export interface DispatchOverride {
  machine?: string;
  kind?: string;
  model?: string;
  effort?: string;
  project_id?: string;
}

export interface TriggerView {
  id: string;
  source: string;
  thread: string;
  from?: string;
  text: string;
  at: string;
  policy: string;
  status: string;
  dispatch?: DispatchOverride;
}

export interface RouterAPISecretStatus {
  has_router_api_key: boolean;
  router_api_key_masked?: string;
  router_api_key_bound_origin?: string;
  router_api_key_origin_match?: boolean;
}

export interface Settings {
  router_api_provider?: "tokenhub" | "openai" | string;
  router_api_base_url?: string;
  router_api_model?: string;
  router_api_effort?: string;
  router_context_budget_tokens?: number;
  worker_kind: string;
  worker_model?: string;
  worker_effort?: string;
  kind_defaults?: Record<string, ModelEffortSpec>;
  machine_kind_overrides?: Record<string, Record<string, ModelEffortSpec>>;
  projects?: ProjectWorkspace[];
  worker_cwd: string;
  worker_machine?: string;
  reply_chars: number;
  stall_minutes: number;
  idle_patrol_minutes?: number;
  active_hours?: string;
  stale_archive_days?: number;
  worker_idle_gc_minutes?: number;
  worker_max_live_per_machine?: number;
  morning_digest?: boolean;
  dream_at?: string;
  auto_update?: boolean;
  public_url?: string;
  notify_level?: "quiet" | "normal";
  ask_permissions?: boolean;
  paused?: boolean;
  wecom_enabled?: boolean;
  wecom_bot_id?: string;
  wecom_secret?: string;
  wecom_allow_users?: string[];
  wecom_default_to?: string;
}

export interface PendingWeComUser {
  user_id: string;
  preview?: string;
  at: string;
}

export interface MemoryFile {
  name: string;
  text: string;
  size: number;
  mod: string;
}

export interface MemorySnapshot {
  dir: string;
  files: MemoryFile[];
  dreams: MemoryFile[];
  up_front: number;
  budget: number;
  dreaming?: boolean;
  last_dream?: string;
  next_dream?: string;
}

export interface DeviceView {
  id: string;
  kind: "browser" | "machine";
  name: string;
  os?: string;
  version?: string;
  home?: string;
  created_at: string;
  last_seen: string;
  online: boolean;
  current: boolean;
  agents?: string[];
}

export interface VersionMachine {
  id: string;
  name: string;
  version?: string;
  os?: string;
  local: boolean;
  online: boolean;
}

export interface FleetUpdateView {
  busy: boolean;
  scope?: string;
  phase: "idle" | "updating" | "reconnecting" | "restarting" | "done" | "failed";
  target?: string;
  total: number;
  index: number;
  current_id?: string;
  current_name?: string;
  completed_ids?: string[];
  pending_ids?: string[];
  failed_id?: string;
  failed_name?: string;
  error?: string;
  errors?: Record<string, string>;
}

export interface VersionStatus {
  version: string;
  latest: string;
  outdated: { id: string; name: string; version: string }[];
  machines?: VersionMachine[];
  update?: FleetUpdateView;
}

export interface MsgPatch {
  base: number;
  from: number;
  msgs: ChatMessage[];
  earlier?: number;
}

export interface SubagentCall {
  type?: string;
  role?: string;
  prompt?: string;
  model?: string;
}

export interface ChatMessage {
  kind: "user" | "assistant" | "thinking" | "tool" | "ask";
  text?: string;
  name?: string;
  options?: string[];
  input?: string;
  detail?: string;
  subagent?: SubagentCall;
  output?: string;
  error?: boolean;
}

export interface Approval {
  title: string;
  detail?: string;
  command?: string;
  choices: { key: string; label: string; keys?: string[]; cursor?: number; active?: boolean }[];
  style: string;
}

export interface EndedSession {
  id: string;
  kind: string;
  machine?: string;
  cwd: string;
  title?: string;
  name?: string;
  session: string;
  mode?: "readonly" | "prototype" | "implement" | string;
  readonly_enforcement?: "hard_sandbox" | "permission_mode" | "soft_prompt" | string;
  ended_at: string;
}

export interface FileResponse {
  kind: "text" | "binary";
  path: string;
  name: string;
  size: number;
  truncated: boolean;
  content: string;
}

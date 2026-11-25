export interface Quote {
  entry?: string;
  worker?: string;
  who: string;
  text: string;
}

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
  check?: boolean;
  pinned?: boolean;
}

export interface ActionChoice {
  key: string;
  label: string;
  detail?: string;
  value?: string;
}

export interface Action {
  id: string;
  kind: "router_choice" | "router_check" | "worker_dialog" | "patrol_digest";
  thread: string;
  entry_id?: string;
  target_worker?: string;
  pane_id?: string;
  title: string;
  summary?: string;
  command?: string;
  choices?: ActionChoice[];
  status: "pending" | "resolved" | "expired" | "snoozed";
  category?: "blocked" | "worktree" | "decision" | "stale" | string;
  resolved_by?: string;
  chosen_key?: string;
  chosen_label?: string;
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
  run?: [string, string];
  title?: string;
  status: string;
  last_report?: string;
  ended?: boolean;
  session?: string;
  repo_root?: string;
  worktree_path?: string;
  worktree_branch?: string;
  idle_since?: string;
  keep?: boolean;
  snooze_until?: string;
}

export interface Thread {
  id: string;
  title?: string;
  channel?: string;
  chat_id?: string;
  subscribers: string[];
  last_at: string;
}

export interface RouterView {
  alive: boolean;
  status: string;
  pane_id?: string;
  kind: string;
  model?: string;
  effort?: string;
  context?: number;
  rotate_at: number;
  handover?: boolean;
  dreaming?: boolean;
  replied?: boolean;
  pending?: boolean;
}

export interface FollowView {
  next: string;
  next_text: string;
}

export interface QueuedPrompt {
  id: string;
  seq?: number;
  scope?: string;
  pane_id?: string;
  thread?: string;
  text: string;
  re?: string;
  machine?: string;
  kind?: string;
  model?: string;
  effort?: string;
  created_at: string;
}

export interface SuperView {
  router: RouterView;
  entries: Entry[];
  earlier?: boolean;
  actions: Action[];
  resolved_actions?: Record<string, Action>;
  snoozed_actions?: number;
  pinned: Entry[];
  workers: Worker[];
  threads: Thread[];
  queued: number;
  queued_router: number;
  queued_chat?: QueuedPrompt[];
  chat_queue_paused?: boolean;
  paused: boolean;
  wecom_online: boolean;
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

export interface TriggerView {
  id: string;
  source: string;
  thread: string;
  from?: string;
  text: string;
  at: string;
  policy: string;
  status: string;
}

export interface Settings {
  router_kind: string;
  router_model?: string;
  router_effort?: string;
  worker_kind: string;
  worker_model?: string;
  worker_effort?: string;
  worker_cwd: string;
  worker_machine?: string;
  reply_chars: number;
  stall_minutes: number;
  idle_patrol_minutes?: number;
  active_hours?: string;
  stale_archive_days?: number;
  morning_digest?: boolean;
  rotate_pct?: number;
  dream_at?: string;
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
  messages: number;
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

export interface VersionStatus {
  version: string;
  latest: string;
  outdated: { id: string; name: string; version: string }[];
}

export interface MsgPatch {
  base: number;
  from: number;
  msgs: ChatMessage[];
  earlier?: number;
}

export interface ChatMessage {
  kind: "user" | "assistant" | "tool" | "ask";
  text?: string;
  name?: string;
  options?: string[];
  input?: string;
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

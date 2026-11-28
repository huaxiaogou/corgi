import type { ArtifactSummary, Entry, PaneState, SuperView, WorkItem } from "../types/api";

export const SNAPSHOT_PREFIX = "corgi.snap.";
export const SNAPSHOT_SCHEMA_VERSION = 1;
export const SNAPSHOT_TTL_MS = 30 * 24 * 60 * 60 * 1000;
export const SNAPSHOT_MAX_BYTES = 200 * 1024;
export const ENTRY_TEXT_MAX_BYTES = 24 * 1024;

export interface StorageLike {
  readonly length: number;
  key(index: number): string | null;
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

export interface SnapshotPayload {
  superView: SuperView | null;
  paneState: PaneState | null;
}

interface StoredSnapshot extends SnapshotPayload {
  schemaVersion: number;
  savedAt: number;
}

interface CompactLimits {
  entries: number;
  textBytes: number;
  actions: number;
  workItems: number;
  workers: number;
  agents: number;
  shells: number;
  pinned: number;
}

const COMPACT_TIERS: CompactLimits[] = [
  { entries: 45, textBytes: ENTRY_TEXT_MAX_BYTES, actions: 40, workItems: 30, workers: 36, agents: 50, shells: 20, pinned: 30 },
  { entries: 30, textBytes: 8 * 1024, actions: 30, workItems: 20, workers: 24, agents: 35, shells: 15, pinned: 15 },
  { entries: 18, textBytes: 4 * 1024, actions: 20, workItems: 12, workers: 16, agents: 20, shells: 10, pinned: 8 },
  { entries: 10, textBytes: 2 * 1024, actions: 12, workItems: 6, workers: 10, agents: 10, shells: 6, pinned: 4 },
  { entries: 4, textBytes: 1024, actions: 6, workItems: 0, workers: 4, agents: 4, shells: 0, pinned: 0 },
];

const encoder = new TextEncoder();

export const utf8ByteLength = (text: string): number => encoder.encode(text).length;

export function truncateUtf8(text: string, maxBytes: number): string {
  if (!text || maxBytes <= 0) return "";
  if (utf8ByteLength(text) <= maxBytes) return text;
  const budget = Math.max(0, maxBytes - utf8ByteLength("…"));
  let lo = 0;
  let hi = text.length;
  while (lo < hi) {
    const mid = Math.ceil((lo + hi) / 2);
    const prev = mid > 0 ? text.charCodeAt(mid - 1) : 0;
    const isHigh = prev >= 0xd800 && prev <= 0xdbff;
    if (utf8ByteLength(text.slice(0, isHigh ? mid - 1 : mid)) <= budget) lo = mid;
    else hi = isHigh ? mid - 2 : mid - 1;
  }
  const end = lo > 0 && text.charCodeAt(lo - 1) >= 0xd800 && text.charCodeAt(lo - 1) <= 0xdbff ? lo - 1 : lo;
  return budget > 0 ? text.slice(0, end) + "…" : "";
}

function hashOrigin(origin: string): string {
  let h = 0x811c9dc5;
  for (let i = 0; i < origin.length; i++) {
    h ^= origin.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return (h >>> 0).toString(16).padStart(8, "0");
}

export function buildSnapshotKey(origin: string, scope: string, thread: string): string {
  const safeScope = scope.replace(/[^a-zA-Z0-9_-]/g, "");
  return `${SNAPSHOT_PREFIX}v${SNAPSHOT_SCHEMA_VERSION}.${hashOrigin(origin)}.${safeScope}.${encodeURIComponent(thread || "chat:main")}`;
}

const stripArtifacts = (list?: ArtifactSummary[]): ArtifactSummary[] | undefined =>
  list?.length ? list.map(({ id, name, kind, size, expired }) => ({ id, name, kind, size, ...(expired ? { expired: true } : {}) })) : undefined;

const compactEntry = (e: Entry, textBytes: number): Entry => ({
  id: e.id, seq: e.seq, role: e.role, thread: e.thread, at: e.at,
  text: truncateUtf8(e.text ?? "", textBytes),
  ...(e.channel ? { channel: e.channel } : {}), ...(e.ref ? { ref: e.ref } : {}),
  ...(e.tone ? { tone: e.tone } : {}), ...(e.re ? { re: { ...e.re, text: truncateUtf8(e.re.text ?? "", 1024) } } : {}),
  ...(e.action_id ? { action_id: e.action_id } : {}), ...(e.options ? { options: e.options } : {}),
  ...(e.files ? { files: e.files } : {}), ...(e.file_machine ? { file_machine: e.file_machine } : {}),
  ...(e.artifacts ? { artifacts: stripArtifacts(e.artifacts) } : {}),
  ...(e.worker_ref ? { worker_ref: { ...e.worker_ref } } : {}), ...(e.work_item_id ? { work_item_id: e.work_item_id } : {}),
  ...(e.check ? { check: true } : {}), ...(e.pinned ? { pinned: true } : {}),
});

const compactWorkItem = (w: WorkItem): WorkItem => ({
  id: w.id, thread: w.thread, title: w.title, created_at: w.created_at, updated_at: w.updated_at,
  ...(w.next_step ? { next_step: w.next_step } : {}), ...(w.workers ? { workers: w.workers } : {}),
  ...(w.branches ? { branches: w.branches } : {}), ...(w.artifacts ? { artifacts: w.artifacts } : {}),
  ...(w.snapshots ? { snapshots: stripArtifacts(w.snapshots) } : {}),
  ...(w.closed_at ? { closed_at: w.closed_at } : {}), ...(w.close_note ? { close_note: w.close_note } : {}),
});

export function compactSuperView(sv: SuperView, lim: CompactLimits = COMPACT_TIERS[0]): SuperView {
  const entries = (sv.entries ?? []).slice(-lim.entries).map((e) => compactEntry(e, lim.textBytes));
  const keptActions = new Set(entries.map((e) => e.action_id).filter(Boolean));
  const resolved: Record<string, SuperView["actions"][number]> = {};
  if (sv.resolved_actions) {
    for (const [id, act] of Object.entries(sv.resolved_actions)) {
      if (keptActions.has(id)) resolved[id] = act;
    }
  }
  return {
    router: sv.router, entries,
    ...(sv.dispatch_defaults ? { dispatch_defaults: sv.dispatch_defaults } : {}),
    ...(sv.earlier ? { earlier: true } : {}),
    actions: (sv.actions ?? []).slice(0, lim.actions),
    ...(Object.keys(resolved).length > 0 ? { resolved_actions: resolved } : {}),
    ...(sv.snoozed_actions ? { snoozed_actions: sv.snoozed_actions } : {}),
    pinned: (sv.pinned ?? []).slice(0, lim.pinned).map((e) => compactEntry(e, Math.min(lim.textBytes, 2048))),
    workers: (sv.workers ?? []).slice(0, lim.workers),
    ...(sv.work_items ? { work_items: sv.work_items.slice(0, lim.workItems).map(compactWorkItem) } : {}),
    ...(sv.closed_work_count ? { closed_work_count: sv.closed_work_count } : {}),
    threads: sv.threads ?? [], queued: sv.queued ?? 0, queued_router: sv.queued_router ?? 0,
    ...(sv.queued_chat ? { queued_chat: sv.queued_chat } : {}),
    ...(sv.chat_queue_paused ? { chat_queue_paused: true } : {}),
    paused: Boolean(sv.paused), wecom_online: Boolean(sv.wecom_online),
    ...(sv.wecom_focus ? { wecom_focus: sv.wecom_focus } : {}), ...(sv.follow ? { follow: sv.follow } : {}),
  };
}

export const compactPaneState = (ps: PaneState, lim: CompactLimits = COMPACT_TIERS[0]): PaneState => ({
  ok: Boolean(ps.ok), ...(ps.error ? { error: ps.error } : {}),
  agents: (ps.agents ?? []).slice(0, lim.agents), shells: (ps.shells ?? []).slice(0, lim.shells),
  machines: ps.machines ?? [], ...(ps.queued_prompts ? { queued_prompts: ps.queued_prompts } : {}),
  ...(ps.paused_queues ? { paused_queues: ps.paused_queues } : {}),
});

function resolveStorage(storage?: StorageLike | null): StorageLike | null {
  if (storage !== undefined) return storage;
  try { return typeof window !== "undefined" ? window.localStorage : null; } catch { return null; }
}

export function readSnapshotCache(origin: string, scope: string, thread: string, storage?: StorageLike | null, now = Date.now()): SnapshotPayload | null {
  if (!scope.trim()) return null;
  const st = resolveStorage(storage);
  if (!st) return null;
  const key = buildSnapshotKey(origin, scope, thread);
  try {
    const raw = st.getItem(key);
    if (!raw) return null;
    const p = JSON.parse(raw) as Partial<StoredSnapshot>;
    if (p.schemaVersion !== SNAPSHOT_SCHEMA_VERSION || typeof p.savedAt !== "number" || now - p.savedAt > SNAPSHOT_TTL_MS || p.savedAt - now > 86_400_000) {
      st.removeItem(key);
      return null;
    }
    return { superView: p.superView ?? null, paneState: p.paneState ?? null };
  } catch {
    try { st.removeItem(key); } catch { /* ignore */ }
    return null;
  }
}

export function writeSnapshotCache(origin: string, scope: string, thread: string, payload: SnapshotPayload, storage?: StorageLike | null, now = Date.now()): boolean {
  if (!scope.trim() || (!payload.superView && !payload.paneState)) return false;
  const st = resolveStorage(storage);
  if (!st) return false;
  const key = buildSnapshotKey(origin, scope, thread);
  for (const tier of COMPACT_TIERS) {
    const rec: StoredSnapshot = {
      schemaVersion: SNAPSHOT_SCHEMA_VERSION, savedAt: now,
      superView: payload.superView ? compactSuperView(payload.superView, tier) : null,
      paneState: payload.paneState ? compactPaneState(payload.paneState, tier) : null,
    };
    const json = JSON.stringify(rec);
    if (utf8ByteLength(json) <= SNAPSHOT_MAX_BYTES) {
      try { st.setItem(key, json); return true; } catch { return false; }
    }
  }
  return false;
}

export function clearAllSnapshotCaches(storage?: StorageLike | null): void {
  const st = resolveStorage(storage);
  if (!st) return;
  try {
    const keys: string[] = [];
    for (let i = 0; i < st.length; i++) {
      const k = st.key(i);
      if (k && k.startsWith(SNAPSHOT_PREFIX)) keys.push(k);
    }
    for (const k of keys) st.removeItem(k);
  } catch { /* ignore private browsing / quota errors */ }
}

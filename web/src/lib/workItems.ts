import type { Action, WorkItem, Worker } from "../types/api";
import { isDecisionAction, isSubstantiveCheckAction } from "./chatFilter";

export type DerivedWorkStatus = "failed" | "blocked" | "working" | "review" | "idle";
export type NotebookMode = "mini" | "expanded" | "hidden";

export interface LinkedWorkerBadge {
  name: string;
  status: DerivedWorkStatus;
  ended: boolean;
  paneId?: string;
}

export interface EnrichedWorkItem {
  item: WorkItem;
  derived: DerivedWorkStatus;
  statusText: string;
  isStale: boolean;
  linkedWorkers: LinkedWorkerBadge[];
  updatedMs: number;
  createdMs: number;
}

export const MINI_MAX_ROWS = 6;
export const FILTER_BAR_THRESHOLD = 8;
export const STALE_DAYS = 7;
export const WORK_PAGE_SIZE = 15;
export const DAY_MS = 24 * 60 * 60 * 1000;

export function derivedPriority(status: DerivedWorkStatus): number {
  if (status === "failed") return 0;
  if (status === "blocked") return 1;
  if (status === "working") return 2;
  if (status === "review") return 3;
  return 4;
}

function workerDotStatus(worker: Worker | undefined, hasPendingDecision: boolean): DerivedWorkStatus {
  if (worker && !worker.ended && (worker.status === "failed" || Boolean(worker.fault_error))) return "failed";
  if (hasPendingDecision) return "blocked";
  if (!worker || worker.ended) return "idle";
  if (worker.status === "blocked") return "blocked";
  if (worker.status === "working" || worker.status === "starting" || worker.status === "waiting") return "working";
  return "idle";
}

function parseMs(iso?: string): number {
  if (!iso) return 0;
  const ms = Date.parse(iso);
  return Number.isFinite(ms) ? ms : 0;
}

export function deriveWorkItems(
  items: WorkItem[],
  workers: Worker[],
  pendingActions: Action[],
  thread: string,
  nowMs: number = Date.now()
): EnrichedWorkItem[] {
  const workerByName = new Map<string, Worker>();
  const workersByWorkId = new Map<string, Worker[]>();
  for (const w of workers) {
    workerByName.set(w.name, w);
    if (w.work_item_id) {
      const list = workersByWorkId.get(w.work_item_id);
      if (list) list.push(w);
      else workersByWorkId.set(w.work_item_id, [w]);
    }
  }

  const pendingWorkIds = new Set<string>();
  const pendingWorkers = new Set<string>();
  const reviewWorkIds = new Set<string>();
  const reviewWorkers = new Set<string>();
  for (const act of pendingActions) {
    if (act.status !== "pending") continue;
    if (isDecisionAction(act)) {
      if (act.work_item_id) pendingWorkIds.add(act.work_item_id);
      else if (act.target_worker) pendingWorkers.add(act.target_worker);
    } else if (isSubstantiveCheckAction(act)) {
      if (act.work_item_id) reviewWorkIds.add(act.work_item_id);
      else if (act.target_worker) reviewWorkers.add(act.target_worker);
    }
  }

  const out: EnrichedWorkItem[] = [];
  for (const item of items) {
    if (item.closed_at) continue;
    if (thread) {
      const itemThread = item.thread || "chat:main";
      if (itemThread !== thread) continue;
    }

    const nameSet = new Set<string>(item.workers ?? []);
    for (const w of workersByWorkId.get(item.id) ?? []) {
      nameSet.add(w.name);
    }

    let hasFailed = false;
    let hasBlocked = pendingWorkIds.has(item.id);
    let hasWorking = false;
    let hasReview = reviewWorkIds.has(item.id);
    const linkedWorkers: LinkedWorkerBadge[] = [];
    for (const name of nameSet) {
      const wk = workerByName.get(name);
      const hasAct = pendingWorkers.has(name);
      if (reviewWorkers.has(name)) hasReview = true;
      const st = workerDotStatus(wk, hasAct);
      if (st === "failed") hasFailed = true;
      else if (st === "blocked") hasBlocked = true;
      else if (st === "working") hasWorking = true;
      linkedWorkers.push({
        name,
        status: st,
        ended: wk ? Boolean(wk.ended) : true,
        paneId: wk && !wk.ended ? wk.pane_id : undefined,
      });
    }
    const hasDeliverables = (item.snapshots?.length ?? 0) > 0 || (item.artifacts?.length ?? 0) > 0;
    if (hasDeliverables) hasReview = true;

    const derived: DerivedWorkStatus = hasFailed
      ? "failed"
      : hasBlocked
        ? "blocked"
        : hasWorking
          ? "working"
          : hasReview
            ? "review"
            : "idle";
    const updatedMs = parseMs(item.updated_at);
    const createdMs = parseMs(item.created_at);
    const isStale = derived === "idle" && updatedMs > 0 && nowMs - updatedMs > STALE_DAYS * DAY_MS;
    const statusText =
      derived === "failed"
        ? "执行受阻"
        : derived === "blocked"
          ? "待拍板"
          : derived === "working"
            ? "运行中"
            : derived === "review"
              ? "待审阅"
              : isStale
                ? "搁置较久"
                : "待推进";

    out.push({
      item,
      derived,
      statusText,
      isStale,
      linkedWorkers,
      updatedMs,
      createdMs,
    });
  }

  out.sort((a, b) => {
    const pDiff = derivedPriority(a.derived) - derivedPriority(b.derived);
    if (pDiff !== 0) return pDiff;
    if (b.updatedMs !== a.updatedMs) return b.updatedMs - a.updatedMs;
    if (b.createdMs !== a.createdMs) return b.createdMs - a.createdMs;
    return b.item.id.localeCompare(a.item.id);
  });
  return out;
}

/**
 * Keeps existing item order stable while the user is hovering or editing (preventing mis-clicks),
 * while still allowing in-place dot/status updates and appending newly arriving items at the end.
 */
export function applyFrozenOrder(items: EnrichedWorkItem[], frozenIds: string[] | null): EnrichedWorkItem[] {
  if (!frozenIds || frozenIds.length === 0) return items;
  const pos = new Map(frozenIds.map((id, idx) => [id, idx]));
  return [...items].sort((a, b) => {
    const pa = pos.has(a.item.id) ? pos.get(a.item.id)! : Number.MAX_SAFE_INTEGER;
    const pb = pos.has(b.item.id) ? pos.get(b.item.id)! : Number.MAX_SAFE_INTEGER;
    if (pa !== pb) return pa - pb;
    const pDiff = derivedPriority(a.derived) - derivedPriority(b.derived);
    if (pDiff !== 0) return pDiff;
    return b.updatedMs - a.updatedMs;
  });
}

export function filterAndPartitionWorkItems(
  items: EnrichedWorkItem[],
  query: string,
  dotFilter: "all" | DerivedWorkStatus
): { active: EnrichedWorkItem[]; stale: EnrichedWorkItem[]; filteringActive: boolean } {
  const q = query.trim().toLowerCase();
  const filteringActive = q !== "" || dotFilter !== "all";
  const matched = items.filter((entry) => {
    if (dotFilter !== "all" && entry.derived !== dotFilter) return false;
    if (!q) return true;
    const hay = [
      entry.item.title,
      entry.item.next_step ?? "",
      entry.item.latest_update ?? "",
      ...(entry.item.workers ?? []),
      ...(entry.item.branches ?? []),
      ...(entry.item.artifacts ?? []).map((a) => a.path),
    ]
      .join(" ")
      .toLowerCase();
    return hay.includes(q);
  });

  if (filteringActive) {
    return { active: matched, stale: [], filteringActive: true };
  }
  return {
    active: matched.filter((x) => !x.isStale),
    stale: matched.filter((x) => x.isStale),
    filteringActive: false,
  };
}

export function formatShortAge(iso?: string, nowMs: number = Date.now()): string {
  const ms = parseMs(iso);
  if (ms <= 0) return "";
  const diff = Math.max(0, nowMs - ms);
  const mins = Math.floor(diff / 60_000);
  if (mins < 1) return "刚刚";
  if (mins < 60) return `${mins}m`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h`;
  const days = Math.floor(hours / 24);
  if (days === 1) return "昨天";
  return `${days}天前`;
}

export function artifactBasename(filePath: string): string {
  const trimmed = filePath.replace(/[\\/]+$/, "");
  const parts = trimmed.split(/[\\/]/);
  return parts[parts.length - 1] || filePath;
}

export function formatWorkItemPrompt(item: Pick<WorkItem, "id" | "title">, userText: string): string {
  const body = userText.trim();
  const tag = `[小本本 ${item.id} · ${item.title}]`;
  return body ? `${tag}\n${body}` : tag;
}

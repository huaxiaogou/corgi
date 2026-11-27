import type { Action, Entry, Worker } from "../types/api";
import { isFastSingleAction } from "./decisionCard";

const ROUTINE_EVENT_RE = /(完成了一轮|已退出|回来了|等你确认：|^已停止 )/;

/**
 * Returns true for noisy routine worker lifecycle events that should stay folded into the
 * original spawn row's live status badge rather than cluttering the main chat stream.
 */
export function isRoutineChatterEvent(
  entry: Entry,
  resolvedActions: Record<string, Action>
): boolean {
  if (entry.role !== "event") return false;
  if (entry.action_id && resolvedActions[entry.action_id]) return false;
  if (entry.tone === "error" || entry.tone === "spawn") return false;
  if (entry.ref?.startsWith("h:")) return false;
  // Keep dream/router notices visible; only fold routine worker lifecycle chatter.
  if (entry.ref === "corgi" || entry.ref === "corgi-dream") return false;
  return ROUTINE_EVENT_RE.test(entry.text);
}

/**
 * Returns true only for pending Actions that require an actual human decision or terminal approval
 * (selectable choices or blocking worker_dialog). Lightweight `--check` review reminders return
 * false so they never inflate decision badges, block WorkItems, or trigger floating decision overlays.
 */
export function isDecisionAction(action: Action | undefined | null): boolean {
  if (!action || action.status !== "pending") return false;
  if (action.kind === "worker_dialog") return true;
  if (action.kind === "router_check") return false;
  return (action.choices?.length ?? 0) > 0;
}

/**
 * Lightweight review reminders (--check) and fast 1-2 option single-select router choices render
 * inline right under their originating Corgi reply bubble. Multi-select and >2-option decisions
 * render in the structured DecisionCard with radio/checkbox selection and explicit submit.
 */
export function isInlineBubbleAction(action: Action | undefined | null): boolean {
  if (!action || action.status !== "pending") return false;
  if (action.kind === "router_check") return true;
  if (action.kind === "router_choice") return isFastSingleAction(action);
  return false;
}

export interface QuoteVisibilityState {
  lastUserId: string;
  seenSinceUser: boolean;
  lastEventRef: string;
}

/**
 * Suppresses redundant QuoteBlocks when a Corgi reply immediately follows the exact user
 * message or worker event it quotes.
 */
export function shouldHideRedundantQuote(
  entry: Entry,
  state: QuoteVisibilityState
): boolean {
  if (entry.role !== "corgi" || !entry.re) return false;
  if (entry.re.entry && entry.re.entry === state.lastUserId && !state.seenSinceUser) {
    return true;
  }
  if (entry.re.worker && entry.re.worker === state.lastEventRef) {
    return true;
  }
  return false;
}

/**
 * Derives a compact live status pill shown inline on a worker's original spawn event row.
 */
export function workerSpawnStatusBadge(
  worker: Worker | undefined
): { label: string; dot: string } {
  if (!worker || worker.ended) {
    return { label: "已结束", dot: "done" };
  }
  switch (worker.status) {
    case "blocked":
      return { label: "等你确认", dot: "blocked" };
    case "working":
    case "starting":
    case "waiting":
      return { label: "还在跑", dot: "working" };
    default:
      return { label: "待命", dot: "idle" };
  }
}

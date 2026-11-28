import type { Action } from "../types/api";
import { isDecisionAction, isFYIAction, isSubstantiveCheckAction } from "./chatFilter";

export function actionPriority(a: Action): number {
  if (a.kind === "worker_dialog" || a.category === "blocked") return 0;
  if (a.category === "worktree") return 1;
  if (a.kind === "patrol_digest" || a.kind === "router_choice") return 2;
  return 3;
}

export function actionCategoryTag(a: Action): { key: string; label: string } {
  if (a.kind === "worker_dialog" || a.category === "blocked") return { key: "blocked", label: "终端阻塞" };
  if (a.category === "worktree") return { key: "worktree", label: "未合分支" };
  if (a.kind === "patrol_digest") {
    const cat = (a.category ?? "").toLowerCase();
    if (cat === "morning") return { key: "digest", label: "晨间简报" };
    if (cat === "stale") return { key: "digest", label: "会话归档" };
    return { key: "digest", label: "待办提醒" };
  }
  if (isFYIAction(a)) return { key: "activity", label: "巡检动态" };
  if (a.kind === "router_check") return { key: "check", label: "审阅提醒" };
  return { key: "decision", label: "待拍板" };
}

export interface InboxPartition {
  pendingActions: Action[];
  pendingDecisions: Action[];
  pendingChecks: Action[];
  activityNotices: Action[];
}

/**
 * Separates blocking decisions, substantive review checks, and low-priority FYI activity notices
 * for the active thread so routine status broadcasts never inflate decision or review counts.
 */
export function partitionInboxActions(
  actions: Action[] | undefined,
  activityNotices: Action[] | undefined,
  thread: string
): InboxPartition {
  const matchesThread = (a: Action) => !thread || a.thread === thread;
  const rawActions = (actions ?? []).filter(matchesThread);
  const pendingActions = rawActions
    .filter((a) => a.status === "pending" && !isFYIAction(a))
    .sort((a, b) => actionPriority(a) - actionPriority(b));

  const pendingDecisions = pendingActions.filter(isDecisionAction);
  const pendingChecks = pendingActions.filter(isSubstantiveCheckAction);

  const seenActivity = new Set<string>();
  const notices: Action[] = [];
  for (const act of activityNotices ?? []) {
    if (!matchesThread(act) || seenActivity.has(act.id)) continue;
    seenActivity.add(act.id);
    notices.push(act);
  }
  // Defensive fallback: if any legacy/cached snapshot still carries category="fyi" in actions,
  // route it cleanly into the activity feed instead of pending review badges.
  for (const act of rawActions) {
    if (!isFYIAction(act) || seenActivity.has(act.id)) continue;
    seenActivity.add(act.id);
    notices.push(act);
  }

  return {
    pendingActions,
    pendingDecisions,
    pendingChecks,
    activityNotices: notices,
  };
}

export interface InboxSummaryMeta {
  visible: boolean;
  isBlocking: boolean;
  headline: string;
}

/**
 * Formats the top summary headline so amber attention indicators and pending numeric counts
 * only reflect real blocking decisions (or substantive checks when zero decisions exist),
 * while pure FYI activity notices stay quiet without numeric pending badges.
 */
export function formatInboxSummaryMeta(
  decisionsCount: number,
  checksCount: number,
  snoozedCount: number,
  activityCount: number
): InboxSummaryMeta {
  if (decisionsCount > 0) {
    return {
      visible: true,
      isBlocking: true,
      headline:
        checksCount > 0
          ? `待你拍板 (${decisionsCount}) · 审阅提醒 (${checksCount})`
          : `待你拍板 (${decisionsCount})`,
    };
  }
  if (checksCount > 0) {
    return {
      visible: true,
      isBlocking: false,
      headline: `审阅提醒 (${checksCount})`,
    };
  }
  if (snoozedCount > 0) {
    return {
      visible: true,
      isBlocking: false,
      headline: `已暂缓 (${snoozedCount})`,
    };
  }
  if (activityCount > 0) {
    return {
      visible: true,
      isBlocking: false,
      headline: "近期巡检动态",
    };
  }
  return { visible: false, isBlocking: false, headline: "" };
}

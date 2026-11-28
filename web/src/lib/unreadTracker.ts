import type { Entry } from "../types/api";

const STORAGE_PREFIX = "corgi.unread.readSeq.v1:";

export type JumpStage = "unread" | "bottom" | "none";

export interface UnreadSummary {
  unreadCount: number;
  firstUnreadId: string;
  firstUnreadSeq: number;
  jumpStage: JumpStage;
  windowTruncated: boolean;
}

export function readStoredReadSeq(thread: string, storage?: Pick<Storage, "getItem"> | null): number {
  const store = storage === undefined ? getSessionStorage() : storage;
  if (!store || !thread) return 0;
  try {
    const raw = store.getItem(`${STORAGE_PREFIX}${thread}`);
    if (!raw) return 0;
    const parsed = Number.parseInt(raw, 10);
    return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
  } catch {
    return 0;
  }
}

export function writeStoredReadSeq(thread: string, seq: number, storage?: Pick<Storage, "setItem"> | null): void {
  const store = storage === undefined ? getSessionStorage() : storage;
  if (!store || !thread || !Number.isFinite(seq) || seq <= 0) return;
  try {
    store.setItem(`${STORAGE_PREFIX}${thread}`, String(Math.floor(seq)));
  } catch {
    // ignore storage quota/privacy errors
  }
}

export function computeLatestSeq(entries: readonly Pick<Entry, "seq">[]): number {
  let maxSeq = 0;
  for (const en of entries) {
    if (en.seq > maxSeq) maxSeq = en.seq;
  }
  return maxSeq;
}

export function nextReadSeqOnStick(params: {
  superReady: boolean;
  currentReadSeq: number;
  latestSeq: number;
}): number {
  if (!params.superReady) return params.currentReadSeq;
  if (params.latestSeq <= 0) return params.currentReadSeq;
  return Math.max(params.currentReadSeq, params.latestSeq);
}

export function computeUnreadSummary(params: {
  entries: readonly Pick<Entry, "id" | "seq" | "role">[];
  readSeq: number;
  userScrolledUp: boolean;
  showJumpBottom: boolean;
  firstUnreadPassed?: boolean;
  hasEarlier?: boolean;
}): UnreadSummary {
  const { entries, readSeq, userScrolledUp, showJumpBottom, firstUnreadPassed = false, hasEarlier = false } = params;
  if (readSeq <= 0 || entries.length === 0) {
    return {
      unreadCount: 0,
      firstUnreadId: "",
      firstUnreadSeq: 0,
      jumpStage: userScrolledUp && showJumpBottom ? "bottom" : "none",
      windowTruncated: false,
    };
  }

  const earliestSeq = entries[0].seq;
  const windowTruncated = Boolean(hasEarlier && readSeq < earliestSeq);

  let unreadCount = 0;
  let firstUnreadId = "";
  let firstUnreadSeq = 0;
  for (const en of entries) {
    if (en.role === "corgi" && en.seq > readSeq) {
      if (unreadCount === 0) {
        firstUnreadId = en.id;
        firstUnreadSeq = en.seq;
      }
      unreadCount += 1;
    }
  }

  let jumpStage: JumpStage = "none";
  if (userScrolledUp) {
    if (unreadCount > 0 && !firstUnreadPassed) {
      jumpStage = "unread";
    } else if (showJumpBottom || unreadCount > 0) {
      jumpStage = "bottom";
    }
  }

  return {
    unreadCount,
    firstUnreadId,
    firstUnreadSeq,
    jumpStage,
    windowTruncated,
  };
}

function getSessionStorage(): Storage | null {
  try {
    return typeof window !== "undefined" ? window.sessionStorage : null;
  } catch {
    return null;
  }
}

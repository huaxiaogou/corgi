import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { CorgiButtMark, StatusDot } from "../../components/CorgiMark";
import { ImageLightbox } from "../../components/ImageLightbox";
import type { InlineReplyMode } from "../../components/InlineReplyBox";
import { PromptQueueTray } from "../../components/PromptQueueTray";
import { useAutoScroll } from "../../hooks/useAutoScroll";
import { useKeyboardViewport } from "../../hooks/useKeyboardViewport";
import { formatWeComFocusBanner } from "../../lib/agentIdentity";
import { api } from "../../lib/api";
import {
  isInlineBubbleAction,
  isRoutineChatterEvent,
  shouldHideRedundantQuote,
  type QuoteVisibilityState,
} from "../../lib/chatFilter";
import type { GalleryImageItem } from "../../lib/imageGallery";
import { partitionInboxActions } from "../../lib/inboxFilter";
import { partitionInPlaceReplies, setDraftEntry, shouldUseInlineReplyBox } from "../../lib/inPlaceReply";
import {
  computeLatestSeq,
  computeUnreadSummary,
  nextReadSeqOnStick,
  readStoredReadSeq,
  writeStoredReadSeq,
} from "../../lib/unreadTracker";
import { deriveWorkItems, formatWorkItemPrompt, type DerivedWorkStatus, type NotebookMode } from "../../lib/workItems";
import type {
  Action,
  ChatMessage,
  EndedSession,
  Entry,
  EntryWorkerRef,
  FileDrawerTarget,
  Machine,
  QueuedPrompt,
  Settings,
  SuperView,
  WorkItem,
} from "../../types/api";
import { ChatEntryRow } from "./ChatEntryRow";
import { ChatFloatingDecision, ChatUnreadJumpPill } from "./ChatFloatingDecision";
import { ChatHeader } from "./ChatHeader";
import { ChatPendingInbox } from "./ChatPendingInbox";
import { ChatHistoryPreviewModal, ChatTopBanners } from "./ChatTopBanners";
import { ComposerBar } from "./ComposerBar";
import { WorkNotebook } from "./WorkNotebook";

const NOTEBOOK_MODE_KEY = "corgi.notebook.mode";

function readDesktopNotebookMode(): NotebookMode {
  try {
    const v = localStorage.getItem(NOTEBOOK_MODE_KEY);
    if (v === "mini" || v === "expanded" || v === "hidden") return v;
  } catch {
    // ignore storage errors
  }
  return "mini";
}

interface Props {
  data: SuperView | null;
  superReady?: boolean;
  machines: Machine[];
  thread: string;
  focusSeq?: number;
  onSelectWorker: (paneId: string) => void;
  onOpenFile: (path: string, machine?: string) => void;
  onOpenArtifact: (target: FileDrawerTarget) => void;
  onOpenSettings?: () => void;
  onToast: (msg: string) => void;
}

export function CorgiChatView({
  data,
  superReady = true,
  machines,
  thread,
  focusSeq = 0,
  onSelectWorker,
  onOpenFile,
  onOpenArtifact,
  onOpenSettings,
  onToast,
}: Props) {
  const [replyTo, setReplyTo] = useState<Entry | null>(null);
  const [replyInPlace, setReplyInPlace] = useState(false);
  const [replyActionTarget, setReplyActionTarget] = useState<Action | null>(null);
  const [draftsByEntryId, setDraftsByEntryId] = useState<Record<string, string>>({});
  const [resumingWorkerKey, setResumingWorkerKey] = useState("");
  const [searchOpen, setSearchOpen] = useState(false);
  const [searchQ, setSearchQ] = useState("");
  const [searchHits, setSearchHits] = useState<{ id: string; role: string; snippet: string }[]>([]);
  const [pinnedOpen, setPinnedOpen] = useState(false);
  const [inboxOpen, setInboxOpen] = useState(false);
  const [clearingWeComFocus, setClearingWeComFocus] = useState(false);
  const [activeFloatingId, setActiveFloatingId] = useState("");
  const [olderEntries, setOlderEntries] = useState<Entry[]>([]);
  const [olderResolvedActions, setOlderResolvedActions] = useState<Record<string, Action>>({});
  const [moreOlder, setMoreOlder] = useState<boolean | null>(null);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [highlightedEntryId, setHighlightedEntryId] = useState("");
  const [readSeq, setReadSeq] = useState(() => readStoredReadSeq(thread));
  const [firstUnreadPassed, setFirstUnreadPassed] = useState(false);
  const [optimisticQueued, setOptimisticQueued] = useState<QueuedPrompt[]>([]);
  const [removedQueueIds, setRemovedQueueIds] = useState<string[]>([]);
  const [queueBusyId, setQueueBusyId] = useState("");
  const [stopping, setStopping] = useState(false);
  const [busyActionId, setBusyActionId] = useState("");
  const [historyPreview, setHistoryPreview] = useState<{ sess: EndedSession; msgs: ChatMessage[] } | null>(null);
  const [lightboxState, setLightboxState] = useState<{ items: GalleryImageItem[]; index: number } | null>(null);
  const [isDesktop, setIsDesktop] = useState(() =>
    typeof window !== "undefined" ? window.matchMedia("(min-width: 1024px)").matches : true
  );
  const [isTabletOrDesktop, setIsTabletOrDesktop] = useState(() =>
    typeof window !== "undefined" ? shouldUseInlineReplyBox(window.innerWidth) : true
  );
  const [notebookMode, setNotebookMode] = useState<NotebookMode>(() =>
    typeof window !== "undefined" && window.matchMedia("(min-width: 1024px)").matches ? readDesktopNotebookMode() : "hidden"
  );
  const [linkedWork, setLinkedWork] = useState<WorkItem | null>(null);
  const [draftSeed, setDraftSeed] = useState<{ seq: number; text: string } | null>(null);

  const tmpSeqRef = useRef(0);
  const draftSeqRef = useRef(0);
  const highlightTimerRef = useRef<number | null>(null);
  const inlineOpenersRef = useRef<Map<string, (mode: InlineReplyMode) => void>>(new Map());
  const firstUnreadIdRef = useRef("");
  const composerDockRef = useRef<HTMLDivElement>(null);
  useKeyboardViewport(composerDockRef);

  useEffect(() => {
    if (typeof window === "undefined") return;
    const mqDesktop = window.matchMedia("(min-width: 1024px)");
    const mqTablet = window.matchMedia("(min-width: 768px)");
    const onDesktopChange = (e: MediaQueryListEvent) => {
      setIsDesktop(e.matches);
      setNotebookMode(e.matches ? readDesktopNotebookMode() : "hidden");
    };
    const onTabletChange = (e: MediaQueryListEvent) => setIsTabletOrDesktop(e.matches);
    mqDesktop.addEventListener("change", onDesktopChange);
    mqTablet.addEventListener("change", onTabletChange);
    return () => {
      mqDesktop.removeEventListener("change", onDesktopChange);
      mqTablet.removeEventListener("change", onTabletChange);
    };
  }, []);

  const registerInlineOpen = useCallback((entryId: string, opener: ((mode: InlineReplyMode) => void) | null) => {
    if (opener) inlineOpenersRef.current.set(entryId, opener);
    else inlineOpenersRef.current.delete(entryId);
  }, []);

  const updateInlineDraft = useCallback((entryId: string, value: string) => {
    setDraftsByEntryId((prev) => setDraftEntry(prev, entryId, value));
  }, []);

  const updateNotebookMode = (next: NotebookMode) => {
    setNotebookMode(next);
    if (isDesktop) {
      try {
        localStorage.setItem(NOTEBOOK_MODE_KEY, next);
      } catch {
        // ignore storage errors
      }
    }
  };

  const serverQueuedChat = data?.queued_chat ?? [];
  const serverQueueSig = serverQueuedChat.map((q) => q.id).join(",");

  useEffect(() => {
    setOlderEntries([]);
    setOlderResolvedActions({});
    setMoreOlder(null);
    setHighlightedEntryId("");
    setOptimisticQueued([]);
    setRemovedQueueIds([]);
    setLinkedWork(null);
    setReplyTo(null);
    setReplyInPlace(false);
    setReplyActionTarget(null);
    setReadSeq(readStoredReadSeq(thread));
    setFirstUnreadPassed(false);
  }, [thread]);

  useEffect(() => () => {
    if (highlightTimerRef.current !== null) window.clearTimeout(highlightTimerRef.current);
  }, []);

  useEffect(() => {
    const curIds = new Set(serverQueuedChat.map((q) => q.id));
    setOptimisticQueued((list) => {
      const next = list.filter((q) => !curIds.has(q.id));
      return next.length === list.length ? list : next;
    });
    setRemovedQueueIds((ids) => {
      const next = ids.filter((id) => curIds.has(id));
      return next.length === ids.length ? ids : next;
    });
  }, [serverQueueSig]);

  const effectiveQueue = [
    ...serverQueuedChat.filter((q) => !removedQueueIds.includes(q.id)),
    ...optimisticQueued.filter((o) => !serverQueuedChat.some((q) => q.id === o.id)),
  ];

  const liveEntries = (data?.entries ?? []).filter((e) => !thread || e.thread === thread);
  const liveIds = new Set(liveEntries.map((e) => e.id));
  const entries = [...olderEntries.filter((e) => !liveIds.has(e.id)), ...liveEntries];
  const resolvedActions: Record<string, Action> = { ...olderResolvedActions, ...(data?.resolved_actions ?? {}) };
  const entryActionById = new Map<string, string>();
  for (const en of entries) {
    if (en.role === "corgi" && en.action_id) entryActionById.set(en.id, en.action_id);
  }
  const canLoadMore = moreOlder ?? Boolean(data?.earlier);
  const pinnedList = (data?.pinned ?? []).filter((e) => !thread || e.thread === thread);
  const { pendingActions, pendingDecisions, pendingChecks, activityNotices } = partitionInboxActions(
    data?.actions,
    data?.activity_notices,
    thread
  );
  const visibleEntries = entries.filter((e) => !isRoutineChatterEvent(e, resolvedActions));
  const { timelineEntries, inPlaceByTarget } = useMemo(() => partitionInPlaceReplies(visibleEntries), [visibleEntries]);
  const visibleEntryIds = new Set(visibleEntries.map((e) => e.id));
  const pendingByEntryId = new Map<string, Action>();
  for (const act of pendingActions) {
    if (isInlineBubbleAction(act) && act.entry_id && visibleEntryIds.has(act.entry_id)) {
      pendingByEntryId.set(act.entry_id, act);
    }
  }
  const floatingActions = pendingDecisions.filter(
    (a) => !(isInlineBubbleAction(a) && a.entry_id && visibleEntryIds.has(a.entry_id))
  );
  const activeFloatingIdx = Math.max(0, floatingActions.findIndex((a) => a.id === activeFloatingId));
  const topFloatingAction = floatingActions[activeFloatingIdx] ?? floatingActions[0] ?? null;
  const switchFloatingQueue = (delta: number) => {
    if (floatingActions.length <= 1) return;
    setActiveFloatingId(floatingActions[(activeFloatingIdx + delta + floatingActions.length) % floatingActions.length].id);
  };
  const snoozedCount = thread === "chat:main" ? (data?.snoozed_actions ?? 0) : 0;
  const activeWorkers = (data?.workers ?? []).filter((w) => !w.ended);
  const lastEntry = liveEntries[liveEntries.length - 1];
  const router = data?.router;
  const routerWorking = router?.status === "working";
  const hasThreadContent = entries.length > 0 || pendingActions.length > 0 || routerWorking;
  const latestLoadedSeq = useMemo(() => computeLatestSeq(entries), [entries]);

  const checkFirstUnreadInView = useCallback((el: HTMLDivElement) => {
    const targetId = firstUnreadIdRef.current;
    if (!targetId) return;
    const node = el.querySelector<HTMLElement>(`#entry-${CSS.escape(targetId)}`);
    if (!node) return;
    if (node.getBoundingClientRect().top <= el.getBoundingClientRect().bottom - 24) {
      setFirstUnreadPassed(true);
    }
  }, []);

  const {
    containerRef,
    contentRef,
    onScroll,
    showJumpBottom,
    userScrolledUp,
    scrollToBottom,
    scrollToEntry,
    capturePrependAnchor,
    captureVisibleAnchor,
  } = useAutoScroll({
    resetKey: `${thread}:${focusSeq}`,
    contentKey: [liveEntries.length, lastEntry?.id ?? "", lastEntry?.text.length ?? 0, pendingActions.map((a) => a.id).join(",")].join("|"),
    prependKey: olderEntries.length,
    hasContent: liveEntries.length > 0 || pendingActions.length > 0,
    onViewportScroll: checkFirstUnreadInView,
  });

  const unreadSummary = useMemo(
    () =>
      computeUnreadSummary({
        entries,
        readSeq,
        userScrolledUp,
        showJumpBottom,
        firstUnreadPassed,
        hasEarlier: canLoadMore,
      }),
    [entries, readSeq, userScrolledUp, showJumpBottom, firstUnreadPassed, canLoadMore]
  );
  firstUnreadIdRef.current = unreadSummary.firstUnreadId;

  useEffect(() => {
    if (!unreadSummary.firstUnreadId) setFirstUnreadPassed(false);
  }, [unreadSummary.firstUnreadId]);

  useEffect(() => {
    if (!superReady || userScrolledUp || latestLoadedSeq <= 0) return;
    const next = nextReadSeqOnStick({ superReady, currentReadSeq: readSeq, latestSeq: latestLoadedSeq });
    if (next > readSeq) {
      setReadSeq(next);
      writeStoredReadSeq(thread, next);
      setFirstUnreadPassed(false);
    }
  }, [superReady, userScrolledUp, latestLoadedSeq, readSeq, thread]);

  const flashEntryHighlight = useCallback((entryId: string, durationMs = 1600) => {
    setHighlightedEntryId(entryId);
    if (highlightTimerRef.current !== null) window.clearTimeout(highlightTimerRef.current);
    highlightTimerRef.current = window.setTimeout(() => {
      setHighlightedEntryId((cur) => (cur === entryId ? "" : cur));
    }, durationMs);
  }, []);

  const jumpToEntry = (entryId: string) => {
    const el = document.getElementById(`entry-${entryId}`);
    if (!el) {
      onToast("原消息位于更早历史中，可点击顶部「加载更早消息」查看");
      return;
    }
    el.scrollIntoView({ behavior: "smooth", block: "center" });
    flashEntryHighlight(entryId, 1600);
  };

  const jumpToFirstUnread = () => {
    if (!unreadSummary.firstUnreadId) return;
    if (!scrollToEntry(unreadSummary.firstUnreadId)) {
      scrollToBottom(true);
      return;
    }
    flashEntryHighlight(unreadSummary.firstUnreadId, 1200);
    setFirstUnreadPassed(true);
  };

  const jumpToBottomAndMarkRead = () => {
    scrollToBottom(true);
    if (superReady && latestLoadedSeq > 0) {
      const next = nextReadSeqOnStick({ superReady, currentReadSeq: readSeq, latestSeq: latestLoadedSeq });
      if (next > readSeq) {
        setReadSeq(next);
        writeStoredReadSeq(thread, next);
      }
      setFirstUnreadPassed(false);
    }
  };

  const openHistoryMatch = async (pred: (s: EndedSession) => boolean) => {
    try {
      const list = await api<{ sessions: EndedSession[] }>("/api/history");
      const sess = (list.sessions ?? []).find(pred);
      if (!sess) {
        onToast("该旧会话记录已超出保留数量");
        return;
      }
      const qs = new URLSearchParams({ kind: sess.kind, session: sess.session, cwd: sess.cwd });
      if (sess.machine) qs.set("machine", sess.machine);
      const res = await api<{ messages: ChatMessage[] }>(`/api/transcript?${qs.toString()}`);
      setHistoryPreview({ sess, msgs: res.messages ?? [] });
    } catch (err) {
      onToast(err instanceof Error ? err.message : "读取会话记录失败");
    }
  };

  const openWorkerByName = (name: string) => {
    const wk = data?.workers.find((w) => w.name === name);
    if (wk?.pane_id && !wk.ended) {
      onSelectWorker(wk.pane_id);
      return;
    }
    void openHistoryMatch((s) => s.name === name);
  };

  const openWorkerRefRecord = (ref: EntryWorkerRef) => {
    void openHistoryMatch((s) => (ref.session ? s.session === ref.session : s.name === ref.name));
  };

  const resumeWorkerRef = async (ref: EntryWorkerRef) => {
    const key = `${ref.name}:${ref.session ?? ""}`;
    if (resumingWorkerKey === key) return;
    setResumingWorkerKey(key);
    try {
      const res = await api<{ ok: boolean; pane_id: string; resumed?: boolean; name?: string }>("/api/super/workers/resume", {
        name: ref.name,
        machine: ref.machine ?? "",
        session: ref.session ?? "",
        kind: ref.kind ?? "",
        work_item_id: ref.work_item_id ?? "",
      });
      if (res.pane_id) {
        onToast(res.resumed ? `已原地恢复 ${res.name || ref.name}` : `已切入 ${res.name || ref.name}`);
        onSelectWorker(res.pane_id);
      }
    } catch (err) {
      onToast(err instanceof Error ? err.message : "恢复会话失败");
    } finally {
      setResumingWorkerKey((cur) => (cur === key ? "" : cur));
    }
  };

  const resolveInlineAction = async (act: Action, choice: string, toastText: string) => {
    if (!superReady || busyActionId === act.id) return;
    captureVisibleAnchor();
    setBusyActionId(act.id);
    try {
      await api(`/api/super/actions/${encodeURIComponent(act.id)}/resolve`, { choice, by: "web" });
      onToast(toastText);
    } catch (err) {
      onToast(err instanceof Error ? err.message : "操作失败");
    } finally {
      setBusyActionId((cur) => (cur === act.id ? "" : cur));
    }
  };

  const dismissInlineAction = async (act: Action) => {
    if (!superReady || busyActionId === act.id) return;
    captureVisibleAnchor();
    setBusyActionId(act.id);
    try {
      await api(`/api/super/actions/${encodeURIComponent(act.id)}/dismiss`, {});
      onToast("已忽略待办");
    } catch (err) {
      onToast(err instanceof Error ? err.message : "操作失败");
    } finally {
      setBusyActionId((cur) => (cur === act.id ? "" : cur));
    }
  };

  const submitInlineReply = async (entry: Entry, replyText: string, mode: InlineReplyMode, inlineAction?: Action) => {
    captureVisibleAnchor();
    if (mode === "action" && inlineAction) {
      setBusyActionId(inlineAction.id);
      try {
        await api(`/api/super/actions/${encodeURIComponent(inlineAction.id)}/resolve`, {
          choice: replyText,
          custom: true,
          in_place: true,
          by: "web",
        });
        onToast("已原位提交自定义回答并同步");
      } catch (err) {
        onToast(err instanceof Error ? err.message : "提交失败");
        throw err;
      } finally {
        setBusyActionId((cur) => (cur === inlineAction.id ? "" : cur));
      }
      return;
    }
    const shouldQueue = router?.status === "working" || effectiveQueue.length > 0;
    const clientMsgId =
      typeof crypto !== "undefined" && typeof crypto.randomUUID === "function"
        ? crypto.randomUUID()
        : `ip-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
    const tmpId = shouldQueue ? `tmp-${++tmpSeqRef.current}` : "";
    if (tmpId) {
      setOptimisticQueued((prev) => [
        ...prev,
        { id: tmpId, thread, text: replyText, re: entry.id, in_place: true, created_at: new Date().toISOString() },
      ]);
    }
    try {
      const res = await api<{ ok: boolean; queued?: boolean; item?: QueuedPrompt }>("/api/super/say", {
        thread,
        text: replyText,
        re: entry.id,
        in_place: true,
        client_msg_id: clientMsgId,
        mode: shouldQueue ? "queue" : "auto",
      });
      if (tmpId) {
        setOptimisticQueued((prev) => {
          const without = prev.filter((x) => x.id !== tmpId);
          return res.queued && res.item ? [...without, res.item] : without;
        });
      } else if (res.queued && res.item) {
        setOptimisticQueued((prev) => [...prev, res.item!]);
      }
      onToast(res.queued ? "已加入待发队列（原位）" : "已原位补充");
    } catch (err) {
      if (tmpId) setOptimisticQueued((prev) => prev.filter((x) => x.id !== tmpId));
      onToast(err instanceof Error ? err.message : "发送失败");
      throw err;
    }
  };

  const loadEarlier = async () => {
    if (loadingOlder || entries.length === 0) return;
    setLoadingOlder(true);
    try {
      const res = await api<{ entries: Entry[]; more: boolean; resolved_actions?: Record<string, Action> }>(
        `/api/super/earlier?thread=${encodeURIComponent(thread)}&before=${entries[0].seq}&limit=80`
      );
      const incoming = res.entries ?? [];
      if (incoming.length > 0) {
        capturePrependAnchor();
        setOlderEntries((prev) => [...incoming, ...prev]);
      }
      if (res.resolved_actions) setOlderResolvedActions((prev) => ({ ...res.resolved_actions, ...prev }));
      setMoreOlder(Boolean(res.more));
    } catch (err) {
      onToast(err instanceof Error ? err.message : "加载历史失败");
    } finally {
      setLoadingOlder(false);
    }
  };

  const togglePause = async () => {
    try {
      const r = await api<{ settings: Settings }>("/api/super/settings");
      const next = !r.settings.paused;
      await api("/api/super/settings", { settings: { ...r.settings, paused: next } });
      onToast(next ? "已暂停自动投递" : "已恢复自动投递");
    } catch (err) {
      onToast(err instanceof Error ? err.message : "操作失败");
    }
  };

  const runSearch = async (q: string) => {
    setSearchQ(q);
    if (!q.trim()) {
      setSearchHits([]);
      return;
    }
    try {
      const res = await api<{ results: { id: string; role: string; snippet: string }[] }>(
        `/api/super/search?q=${encodeURIComponent(q)}&thread=${encodeURIComponent(thread)}`
      );
      setSearchHits(res.results ?? []);
    } catch {
      setSearchHits([]);
    }
  };

  const isWeComGroup = thread.startsWith("wecom:");
  const headerTitle = thread === "chat:main" ? "Corgi" : isWeComGroup ? `企微群 · ${thread.replace(/^wecom:/, "")}` : thread;
  const notebookSummary = useMemo(() => {
    const derived = deriveWorkItems(data?.work_items ?? [], data?.workers ?? [], pendingActions, thread);
    const dot: DerivedWorkStatus = derived.some((x) => x.derived === "failed")
      ? "failed"
      : derived.some((x) => x.derived === "blocked")
        ? "blocked"
        : derived.some((x) => x.derived === "working")
          ? "working"
          : derived.some((x) => x.derived === "review")
            ? "review"
            : "idle";
    return { count: derived.length, dot };
  }, [data?.work_items, data?.workers, pendingActions, thread]);

  const toggleNotebook = () => {
    if (!isDesktop) {
      updateNotebookMode(notebookMode === "expanded" ? "hidden" : "expanded");
      return;
    }
    if (notebookSummary.count === 0) {
      updateNotebookMode(notebookMode === "expanded" ? "mini" : "expanded");
      return;
    }
    if (notebookMode === "hidden") updateNotebookMode("mini");
    else if (notebookMode === "mini") updateNotebookMode("expanded");
    else updateNotebookMode("hidden");
  };

  const wecomFocus = data?.wecom_focus;
  const wecomFocusLabel = wecomFocus ? formatWeComFocusBanner(wecomFocus) : "";
  const clearWeComFocus = async () => {
    if (!wecomFocus || clearingWeComFocus) return;
    setClearingWeComFocus(true);
    try {
      await api("/api/super/wecom/focus/clear", { key: wecomFocus.key });
      onToast("已将企微切回 Corgi 主对话");
    } catch (err) {
      onToast(err instanceof Error ? err.message : "切回企微焦点失败");
    } finally {
      setClearingWeComFocus(false);
    }
  };

  return (
    <div className="flex-1 flex flex-col min-w-0 min-h-0 overflow-hidden">
      <ChatHeader
        title={headerTitle}
        thread={thread}
        isWeComGroup={isWeComGroup}
        router={router}
        paused={data?.paused}
        syncing={Boolean(data) && !superReady}
        actionsDisabled={!superReady}
        follow={data?.follow}
        notebookCount={notebookSummary.count}
        notebookDot={notebookSummary.dot}
        notebookMode={notebookMode}
        onToggleNotebook={toggleNotebook}
        onToggleSearch={() => setSearchOpen((v) => !v)}
        onRestartRouter={() => {
          if (!superReady) return;
          void api("/api/super/router/restart", {}).then(() => onToast("已重置 Corgi 大脑状态"));
        }}
        onTogglePause={() => {
          if (!superReady) return;
          void togglePause();
        }}
        onToast={onToast}
      />

      <ChatTopBanners
        wecomFocusLabel={wecomFocusLabel}
        clearingWeComFocus={clearingWeComFocus}
        onClearWeComFocus={() => void clearWeComFocus()}
        searchOpen={searchOpen}
        searchQ={searchQ}
        searchHits={searchHits}
        onSearchChange={(q) => void runSearch(q)}
        onCloseSearch={() => setSearchOpen(false)}
        onJumpToEntry={jumpToEntry}
        pinnedList={pinnedList}
        pinnedOpen={pinnedOpen}
        onTogglePinned={() => setPinnedOpen((v) => !v)}
      />

      <ChatPendingInbox
        thread={thread}
        pendingActions={pendingActions}
        pendingDecisions={pendingDecisions}
        pendingChecks={pendingChecks}
        activityNotices={activityNotices}
        snoozedCount={snoozedCount}
        superReady={superReady}
        inboxOpen={inboxOpen}
        onToggleOpen={() => setInboxOpen((v) => !v)}
        onSetOpen={setInboxOpen}
        onSelectFloating={(id) => {
          setActiveFloatingId(id);
          setInboxOpen(false);
          scrollToBottom(true);
        }}
        onJumpToEntry={jumpToEntry}
        onOpenWorker={openWorkerByName}
        onToast={onToast}
      />

      <div className="flex-1 flex min-h-0 overflow-hidden relative">
        <div className="flex-1 flex flex-col min-w-0 min-h-0">
          <div className="relative flex-1 flex flex-col min-h-0">
            <div ref={containerRef} onScroll={onScroll} className="flex-1 overflow-y-auto scroll-chat px-5 pt-6 pb-2 min-h-0">
              <div ref={contentRef} className="max-w-[740px] mx-auto">
                <div className="space-y-5">
                  {canLoadMore && (
                    <div className="text-center">
                      <button
                        type="button"
                        disabled={loadingOlder}
                        onClick={() => void loadEarlier()}
                        className="px-3.5 py-1 rounded-full text-xs border border-strong text-secondary surface-hover cursor-pointer disabled:opacity-50"
                      >
                        {loadingOlder ? "加载中…" : "加载更早消息"}
                      </button>
                    </div>
                  )}
                  {entries.length === 0 && (
                    <div className="py-16 text-center space-y-2 text-muted">
                      <div className="w-12 h-12 rounded-2xl border border-hairline flex items-center justify-center mx-auto text-primary">
                        <CorgiButtMark className="w-8 h-8" />
                      </div>
                      <div className="text-sm text-secondary">告诉 Corgi 你要办什么事</div>
                      <div className="text-xs">它会拆活派给各台电脑上的 Agent，只在需要你拍板时出声。</div>
                    </div>
                  )}

                  {(() => {
                    const quoteState: QuoteVisibilityState = { lastUserId: "", seenSinceUser: false, lastEventRef: "" };
                    return timelineEntries.map((entry, idx) => {
                      let hideQuote = false;
                      if (entry.role === "event") {
                        quoteState.lastEventRef = entry.ref ?? "";
                      } else if (entry.role === "user") {
                        quoteState.lastUserId = entry.id;
                        quoteState.seenSinceUser = false;
                        quoteState.lastEventRef = "";
                      } else {
                        hideQuote = shouldHideRedundantQuote(entry, quoteState);
                        quoteState.seenSinceUser = true;
                        quoteState.lastEventRef = "";
                      }
                      const inlineAction = entry.role === "corgi" ? pendingByEntryId.get(entry.id) : undefined;
                      return (
                        <ChatEntryRow
                          key={entry.id}
                          entry={entry}
                          idx={idx}
                          timelineEntries={timelineEntries}
                          highlightedEntryId={highlightedEntryId}
                          resolvedActions={resolvedActions}
                          entryActionById={entryActionById}
                          hideQuote={hideQuote}
                          inlineAction={inlineAction}
                          inlineBusy={inlineAction ? busyActionId === inlineAction.id : false}
                          inPlaceReplies={inPlaceByTarget.get(entry.id) ?? []}
                          draftsByEntryId={draftsByEntryId}
                          superReady={superReady}
                          isTabletOrDesktop={isTabletOrDesktop}
                          routerWorking={routerWorking}
                          workers={data?.workers}
                          machines={machines}
                          resumingWorkerKey={resumingWorkerKey}
                          onJumpToEntry={jumpToEntry}
                          onOpenWorker={openWorkerByName}
                          onSelectWorker={onSelectWorker}
                          onOpenWorkerRecord={openWorkerRefRecord}
                          onResumeWorker={(ref) => void resumeWorkerRef(ref)}
                          onOpenHistoryById={(histRef) => void openHistoryMatch((s) => s.id === histRef)}
                          onOpenFile={onOpenFile}
                          onOpenArtifact={onOpenArtifact}
                          onOpenLightbox={(items, index) => setLightboxState({ items, index })}
                          onResolveInlineAction={(act, ch, msg) => void resolveInlineAction(act, ch, msg)}
                          onDismissInlineAction={(act) => void dismissInlineAction(act)}
                          onReplyQuote={(target) => {
                            setReplyInPlace(false);
                            setReplyActionTarget(null);
                            setReplyTo(target);
                          }}
                          onOpenInlineOrMobile={(target, mode, act) => {
                            const opener = inlineOpenersRef.current.get(target.id);
                            if (opener) {
                              opener(mode);
                            } else {
                              setReplyTo(target);
                              setReplyInPlace(true);
                              setReplyActionTarget(mode === "action" && act ? act : null);
                            }
                          }}
                          onMobileReply={(target, mode, act) => {
                            setReplyTo(target);
                            setReplyInPlace(true);
                            setReplyActionTarget(mode === "action" && act ? act : null);
                          }}
                          onSubmitInlineReply={submitInlineReply}
                          onDraftChange={updateInlineDraft}
                          onRegisterInlineOpen={registerInlineOpen}
                          onToast={onToast}
                        />
                      );
                    });
                  })()}

                  <ChatFloatingDecision
                    action={topFloatingAction}
                    queuePos={activeFloatingIdx + 1}
                    queueTotal={floatingActions.length}
                    totalPending={pendingActions.length}
                    superReady={superReady}
                    workerNames={activeWorkers.map((w) => w.name)}
                    onSwitchQueue={switchFloatingQueue}
                    onExpandInbox={() => setInboxOpen(true)}
                    onOpenWorker={openWorkerByName}
                    onOpenFile={(p) => onOpenFile(p)}
                    onToast={onToast}
                  />
                </div>

                {hasThreadContent && (
                  <div
                    aria-live="polite"
                    aria-hidden={!routerWorking}
                    className={`mt-3 h-6 flex items-center gap-2.5 text-xs text-muted transition-opacity duration-150 ${
                      routerWorking ? "opacity-100" : "opacity-0 pointer-events-none select-none"
                    }`}
                  >
                    {routerWorking && <StatusDot status="working" />}
                    <span>{routerWorking ? "Corgi 在想…" : "\u00A0"}</span>
                  </div>
                )}
              </div>
            </div>
            <ChatUnreadJumpPill
              unreadSummary={unreadSummary}
              onJumpToFirstUnread={jumpToFirstUnread}
              onJumpToBottom={jumpToBottomAndMarkRead}
            />
          </div>

          <div ref={composerDockRef} className="px-5 pt-2 shrink-0 space-y-2 kb-dock">
            <PromptQueueTray
              items={effectiveQueue}
              paused={Boolean(data?.chat_queue_paused)}
              busyId={queueBusyId}
              onSteer={async (item) => {
                if (!superReady || item.id.startsWith("tmp-")) return;
                setQueueBusyId(item.id);
                setRemovedQueueIds((prev) => [...prev, item.id]);
                try {
                  await api(`/api/super/queue/${encodeURIComponent(item.id)}/steer`, { thread });
                  if (!item.in_place) scrollToBottom(true);
                } catch (err) {
                  setRemovedQueueIds((prev) => prev.filter((x) => x !== item.id));
                  onToast(err instanceof Error ? err.message : "引导发送失败");
                } finally {
                  setQueueBusyId("");
                }
              }}
              onCancel={async (item) => {
                if (!superReady) return;
                if (item.id.startsWith("tmp-")) {
                  setOptimisticQueued((prev) => prev.filter((x) => x.id !== item.id));
                  return;
                }
                setQueueBusyId(item.id);
                setRemovedQueueIds((prev) => [...prev, item.id]);
                try {
                  await api(`/api/super/queue/${encodeURIComponent(item.id)}/cancel`, { thread });
                } catch (err) {
                  setRemovedQueueIds((prev) => prev.filter((x) => x !== item.id));
                  onToast(err instanceof Error ? err.message : "取消排队失败");
                } finally {
                  setQueueBusyId("");
                }
              }}
              onResume={async () => {
                if (!superReady) return;
                try {
                  await api("/api/super/queue/resume", { thread });
                } catch (err) {
                  onToast(err instanceof Error ? err.message : "恢复队列失败");
                }
              }}
            />
            <ComposerBar
              embedded
              disabled={!superReady}
              machines={machines}
              dispatchDefaults={data?.dispatch_defaults}
              replyTo={replyTo}
              linkedWork={linkedWork}
              draftSeed={draftSeed}
              working={routerWorking}
              stopping={stopping}
              queuedCount={effectiveQueue.length}
              onOpenSettings={onOpenSettings}
              onStop={async () => {
                if (stopping) return;
                setStopping(true);
                try {
                  const res = await api<{ ok: boolean; paused?: boolean }>("/api/super/router/stop", { thread });
                  onToast(res.paused ? "已停止当前步骤（待发队列已暂停保留）" : "已停止当前步骤 (Esc)");
                } catch (err) {
                  onToast(err instanceof Error ? err.message : "停止失败");
                } finally {
                  setStopping(false);
                }
              }}
              onClearReply={() => {
                setReplyTo(null);
                setReplyInPlace(false);
                setReplyActionTarget(null);
              }}
              onClearLinkedWork={() => setLinkedWork(null)}
              onToast={onToast}
              onSend={async ({ text, machine, kind, model, effort, projectId, media, clientMsgId }) => {
                const reId = replyTo?.id ?? "";
                const isInPlaceSend = Boolean(replyInPlace && reId);
                const actionTarget = replyActionTarget;
                if (actionTarget) {
                  captureVisibleAnchor();
                  try {
                    await api(`/api/super/actions/${encodeURIComponent(actionTarget.id)}/resolve`, {
                      choice: text,
                      custom: true,
                      in_place: isInPlaceSend,
                      by: "web",
                    });
                    setReplyTo(null);
                    setReplyInPlace(false);
                    setReplyActionTarget(null);
                    onToast("已提交自定义回答并同步");
                  } catch (err) {
                    onToast(err instanceof Error ? err.message : "提交失败");
                    throw err;
                  }
                  return;
                }
                const finalText = linkedWork ? formatWorkItemPrompt(linkedWork, text) : text;
                const shouldQueue = router?.status === "working" || effectiveQueue.length > 0;
                const tmpId = shouldQueue ? `tmp-${++tmpSeqRef.current}` : "";
                if (isInPlaceSend) captureVisibleAnchor();
                if (tmpId) {
                  setOptimisticQueued((prev) => [
                    ...prev,
                    { id: tmpId, thread, text: finalText, re: reId || undefined, in_place: isInPlaceSend || undefined, created_at: new Date().toISOString() },
                  ]);
                }
                try {
                  const res = await api<{ ok: boolean; queued?: boolean; item?: QueuedPrompt }>("/api/super/say", {
                    thread,
                    text: finalText,
                    re: reId,
                    in_place: isInPlaceSend,
                    machine,
                    kind,
                    model,
                    effort,
                    project_id: projectId,
                    media,
                    client_msg_id: clientMsgId,
                    mode: shouldQueue ? "queue" : "auto",
                  });
                  setReplyTo(null);
                  setReplyInPlace(false);
                  setReplyActionTarget(null);
                  setLinkedWork(null);
                  if (tmpId) {
                    setOptimisticQueued((prev) => {
                      const without = prev.filter((x) => x.id !== tmpId);
                      return res.queued && res.item ? [...without, res.item] : without;
                    });
                  } else if (res.queued && res.item) {
                    setOptimisticQueued((prev) => [...prev, res.item!]);
                  }
                  if (!res.queued && !isInPlaceSend) scrollToBottom(true);
                } catch (err) {
                  if (tmpId) setOptimisticQueued((prev) => prev.filter((x) => x.id !== tmpId));
                  onToast(err instanceof Error ? err.message : "发送失败");
                  throw err;
                }
              }}
            />
          </div>
        </div>

        <WorkNotebook
          items={data?.work_items ?? []}
          closedCount={data?.closed_work_count ?? 0}
          workers={data?.workers ?? []}
          pendingActions={pendingActions}
          thread={thread}
          mode={notebookMode}
          isDesktop={isDesktop}
          onModeChange={updateNotebookMode}
          onAskCorgi={(item) => {
            setLinkedWork(item);
            setDraftSeed({ seq: ++draftSeqRef.current, text: "同步一下当前进展和下一步建议" });
            onToast("已关联小本本，可直接发送或补充");
          }}
          onOpenWorker={openWorkerByName}
          onOpenFile={onOpenFile}
          onOpenArtifact={onOpenArtifact}
          onToast={onToast}
        />
      </div>

      <ChatHistoryPreviewModal
        preview={historyPreview}
        onClose={() => setHistoryPreview(null)}
        onSelectWorker={onSelectWorker}
        onOpenFile={onOpenFile}
        onToast={onToast}
      />
      {lightboxState && lightboxState.items.length > 0 && (
        <ImageLightbox
          items={lightboxState.items}
          initialIndex={lightboxState.index}
          onClose={() => setLightboxState(null)}
        />
      )}
    </div>
  );
}

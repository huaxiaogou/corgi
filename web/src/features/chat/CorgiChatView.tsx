import { useEffect, useRef, useState } from "react";
import { ArrowDown, Check, Pin, X } from "lucide-react";
import { CorgiButtMark, CorgiMark, StatusDot } from "../../components/CorgiMark";
import { DecisionCard } from "../../components/DecisionCard";
import { DecisionReceiptCard } from "../../components/DecisionReceiptCard";
import { MarkdownText } from "../../components/MarkdownText";
import { PromptQueueTray } from "../../components/PromptQueueTray";
import { QuoteBlock } from "../../components/QuoteBlock";
import { useAutoScroll } from "../../hooks/useAutoScroll";
import { api } from "../../lib/api";
import {
  isInlineBubbleAction,
  isRoutineChatterEvent,
  shouldHideRedundantQuote,
  workerSpawnStatusBadge,
  type QuoteVisibilityState,
} from "../../lib/chatFilter";
import type { Action, ChatMessage, EndedSession, Entry, Machine, QueuedPrompt, Settings, SuperView } from "../../types/api";
import { ChatHeader } from "./ChatHeader";
import { ComposerBar } from "./ComposerBar";

function actionPriority(a: Action): number {
  if (a.kind === "worker_dialog" || a.category === "blocked") return 0;
  if (a.category === "worktree") return 1;
  if (a.kind === "patrol_digest" || a.kind === "router_choice") return 2;
  return 3;
}

function actionCategoryTag(a: Action): { key: string; label: string } {
  if (a.kind === "worker_dialog" || a.category === "blocked") return { key: "blocked", label: "终端阻塞" };
  if (a.category === "worktree") return { key: "worktree", label: "未合分支" };
  if (a.kind === "patrol_digest") {
    const cat = (a.category ?? "").toLowerCase();
    if (cat === "morning") return { key: "digest", label: "晨间简报" };
    if (cat === "stale") return { key: "digest", label: "会话归档" };
    return { key: "digest", label: "待办提醒" };
  }
  if (a.kind === "router_check") return { key: "check", label: "待确认" };
  return { key: "decision", label: "待拍板" };
}

interface Props {
  data: SuperView | null;
  machines: Machine[];
  thread: string;
  focusSeq?: number;
  onSelectWorker: (paneId: string) => void;
  onOpenFile: (path: string, machine?: string) => void;
  onToast: (msg: string) => void;
}

export function CorgiChatView({ data, machines, thread, focusSeq = 0, onSelectWorker, onOpenFile, onToast }: Props) {
  const [replyTo, setReplyTo] = useState<Entry | null>(null);
  const [searchOpen, setSearchOpen] = useState(false);
  const [searchQ, setSearchQ] = useState("");
  const [searchHits, setSearchHits] = useState<{ id: string; role: string; snippet: string }[]>([]);
  const [pinnedOpen, setPinnedOpen] = useState(false);
  const [inboxOpen, setInboxOpen] = useState(false);
  const [inboxFilter, setInboxFilter] = useState("all");
  const [inboxSearch, setInboxSearch] = useState("");
  const [olderEntries, setOlderEntries] = useState<Entry[]>([]);
  const [olderResolvedActions, setOlderResolvedActions] = useState<Record<string, Action>>({});
  const [moreOlder, setMoreOlder] = useState<boolean | null>(null);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [highlightedEntryId, setHighlightedEntryId] = useState("");
  const [optimisticQueued, setOptimisticQueued] = useState<QueuedPrompt[]>([]);
  const [removedQueueIds, setRemovedQueueIds] = useState<string[]>([]);
  const [queueBusyId, setQueueBusyId] = useState("");
  const [stopping, setStopping] = useState(false);
  const [busyActionId, setBusyActionId] = useState("");
  const [historyPreview, setHistoryPreview] = useState<{ sess: EndedSession; msgs: ChatMessage[] } | null>(null);
  const tmpSeqRef = useRef(0);
  const highlightTimerRef = useRef<number | null>(null);

  const serverQueuedChat = data?.queued_chat ?? [];
  const serverQueueSig = serverQueuedChat.map((q) => q.id).join(",");

  useEffect(() => {
    setOlderEntries([]);
    setOlderResolvedActions({});
    setMoreOlder(null);
    setHighlightedEntryId("");
    setOptimisticQueued([]);
    setRemovedQueueIds([]);
  }, [thread]);

  useEffect(() => {
    return () => {
      if (highlightTimerRef.current !== null) {
        window.clearTimeout(highlightTimerRef.current);
      }
    };
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
  const resolvedActions: Record<string, Action> = {
    ...olderResolvedActions,
    ...(data?.resolved_actions ?? {}),
  };
  const entryActionById = new Map<string, string>();
  for (const en of entries) {
    if (en.role === "corgi" && en.action_id) {
      entryActionById.set(en.id, en.action_id);
    }
  }
  const canLoadMore = moreOlder ?? Boolean(data?.earlier);
  const pinnedList = (data?.pinned ?? []).filter((e) => !thread || e.thread === thread);
  const pendingActions = (data?.actions ?? [])
    .filter((a) => a.status === "pending" && (!thread || a.thread === thread))
    .sort((a, b) => actionPriority(a) - actionPriority(b));
  const visibleEntries = entries.filter((e) => !isRoutineChatterEvent(e, resolvedActions));
  const visibleEntryIds = new Set(visibleEntries.map((e) => e.id));
  const pendingByEntryId = new Map<string, Action>();
  for (const act of pendingActions) {
    if (isInlineBubbleAction(act) && act.entry_id && visibleEntryIds.has(act.entry_id)) {
      pendingByEntryId.set(act.entry_id, act);
    }
  }
  const floatingActions = pendingActions.filter(
    (a) => !(isInlineBubbleAction(a) && a.entry_id && visibleEntryIds.has(a.entry_id))
  );
  const topAction = pendingActions[0] ?? null;
  const topFloatingAction = floatingActions[0] ?? null;
  const snoozedCount = thread === "chat:main" ? (data?.snoozed_actions ?? 0) : 0;
  const filteredInbox = pendingActions.filter((a) => {
    if (inboxFilter !== "all" && actionCategoryTag(a).key !== inboxFilter) return false;
    if (inboxSearch.trim()) {
      const q = inboxSearch.toLowerCase();
      return `${a.title} ${a.target_worker ?? ""} ${a.summary ?? ""}`.toLowerCase().includes(q);
    }
    return true;
  });
  const activeWorkers = (data?.workers ?? []).filter((w) => !w.ended);
  const lastEntry = liveEntries[liveEntries.length - 1];
  const router = data?.router;
  const routerWorking = router?.status === "working";
  const hasThreadContent = entries.length > 0 || pendingActions.length > 0 || routerWorking;

  const { containerRef, contentRef, onScroll, showJumpBottom, scrollToBottom, capturePrependAnchor, captureVisibleAnchor } = useAutoScroll({
    resetKey: `${thread}:${focusSeq}`,
    contentKey: [
      liveEntries.length,
      lastEntry?.id ?? "",
      lastEntry?.text.length ?? 0,
      pendingActions.map((a) => a.id).join(","),
    ].join("|"),
    prependKey: olderEntries.length,
    hasContent: liveEntries.length > 0 || pendingActions.length > 0,
  });

  const jumpToEntry = (entryId: string) => {
    const el = document.getElementById(`entry-${entryId}`);
    if (!el) {
      onToast("原消息位于更早历史中，可点击顶部「加载更早消息」查看");
      return;
    }
    el.scrollIntoView({ behavior: "smooth", block: "center" });
    setHighlightedEntryId(entryId);
    if (highlightTimerRef.current !== null) {
      window.clearTimeout(highlightTimerRef.current);
    }
    highlightTimerRef.current = window.setTimeout(() => {
      setHighlightedEntryId((cur) => (cur === entryId ? "" : cur));
    }, 1600);
  };

  const openWorkerByName = (name: string) => {
    const wk = data?.workers.find((w) => w.name === name);
    if (wk?.pane_id && !wk.ended) {
      onSelectWorker(wk.pane_id);
      return;
    }
    void openHistoryMatch((s) => s.name === name);
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

  const resolveInlineAction = async (act: Action, choice: string, toastText: string) => {
    if (busyActionId === act.id) return;
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
    if (busyActionId === act.id) return;
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
      if (res.resolved_actions) {
        setOlderResolvedActions((prev) => ({ ...res.resolved_actions, ...prev }));
      }
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

  return (
    <div className="flex-1 flex flex-col min-w-0 min-h-0 overflow-hidden">
      <ChatHeader
        title={headerTitle}
        isWeComGroup={isWeComGroup}
        router={router}
        paused={data?.paused}
        follow={data?.follow}
        onToggleSearch={() => setSearchOpen((v) => !v)}
        onRestartRouter={() => void api("/api/super/router/restart", {}).then(() => onToast("正在重启 Corgi 会话"))}
        onTogglePause={() => void togglePause()}
        onOpenRouterPane={onSelectWorker}
      />

      {searchOpen && (
        <div className="px-5 py-2.5 border-b border-hairline surface-rail space-y-2">
          <div className="flex items-center gap-2">
            <input
              type="search"
              value={searchQ}
              onChange={(e) => void runSearch(e.target.value)}
              placeholder="搜索历史对话（空格分隔多词）…"
              className="flex-1 px-3 py-1.5 rounded-lg text-xs surface-bg border border-hairline outline-none"
            />
            <button type="button" onClick={() => setSearchOpen(false)} className="text-muted cursor-pointer">
              <X className="w-4 h-4" />
            </button>
          </div>
          {searchHits.length > 0 && (
            <div className="max-h-36 overflow-y-auto space-y-1 text-xs">
              {searchHits.map((h) => (
                <div key={h.id} className="px-2.5 py-1.5 rounded-lg surface-bg border border-hairline text-secondary truncate">
                  <span className="font-mono-code text-muted mr-2">{h.role}</span>
                  {h.snippet}
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {activeWorkers.length > 0 && (
        <div className="px-5 py-2 flex items-center gap-2 border-b border-hairline overflow-x-auto shrink-0">
          {activeWorkers.map((w) => (
            <button
              key={w.name}
              type="button"
              onClick={() => w.pane_id && onSelectWorker(w.pane_id)}
              className="inline-flex items-center gap-1.5 px-3 py-1 rounded-full text-xs border border-strong surface-bg surface-hover cursor-pointer shrink-0"
            >
              <StatusDot status={w.status} />
              <span className="font-medium">{w.name}</span>
              <span className="font-mono-code text-muted">
                {w.kind}
                {w.machine ? ` · ${w.machine}` : ""}
                {w.worktree_branch ? ` · ${w.worktree_branch}` : ""}
              </span>
            </button>
          ))}
        </div>
      )}

      {(pendingActions.length > 0 || snoozedCount > 0) && (
        <div className="border-b border-hairline surface-rail shrink-0 text-xs">
          <div className="px-5 py-2 flex items-center justify-between gap-3">
            <button
              type="button"
              onClick={() => pendingActions.length > 0 && setInboxOpen((v) => !v)}
              className="flex items-center gap-2 min-w-0 text-left cursor-pointer"
            >
              <span
                className="w-2 h-2 rounded-full shrink-0"
                style={{ background: pendingActions.length > 0 ? "var(--attn)" : "var(--fg-3)" }}
              />
              <span className="font-medium text-primary shrink-0">
                {pendingActions.length > 0 ? `待你确认 (${pendingActions.length})` : `已暂缓 (${snoozedCount})`}
              </span>
              {topAction ? (
                <>
                  <span className="text-muted">·</span>
                  <span className="text-secondary truncate">{topAction.title}</span>
                </>
              ) : (
                <span className="text-muted truncate">将在下次晨报重新浮出</span>
              )}
            </button>
            {pendingActions.length > 1 && (
              <button
                type="button"
                onClick={() => setInboxOpen((v) => !v)}
                className="px-2 py-0.5 rounded-md border border-strong surface-bg surface-hover text-secondary cursor-pointer shrink-0"
              >
                {inboxOpen ? "收起收件箱" : "展开收件箱"}
              </button>
            )}
          </div>
          {inboxOpen && pendingActions.length > 0 && (
            <div className="px-5 pb-3 pt-1.5 space-y-2 border-t border-hairline surface-bg">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex flex-wrap items-center gap-1.5">
                  {[
                    { key: "all", label: "全部", count: pendingActions.length },
                    { key: "blocked", label: "终端阻塞", count: pendingActions.filter((a) => actionCategoryTag(a).key === "blocked").length },
                    { key: "worktree", label: "未合分支", count: pendingActions.filter((a) => actionCategoryTag(a).key === "worktree").length },
                    { key: "digest", label: "巡检简报", count: pendingActions.filter((a) => actionCategoryTag(a).key === "digest").length },
                    { key: "decision", label: "待拍板", count: pendingActions.filter((a) => actionCategoryTag(a).key === "decision").length },
                    { key: "check", label: "待确认", count: pendingActions.filter((a) => actionCategoryTag(a).key === "check").length },
                  ]
                    .filter((f) => f.key === "all" || f.count > 0)
                    .map((f) => (
                      <button
                        key={f.key}
                        type="button"
                        onClick={() => setInboxFilter(f.key)}
                        className={`px-2.5 py-0.5 rounded-full border cursor-pointer ${
                          inboxFilter === f.key ? "btn-accent border-transparent" : "border-hairline text-secondary surface-hover"
                        }`}
                      >
                        {f.label} {f.count}
                      </button>
                    ))}
                </div>
                <input
                  value={inboxSearch}
                  onChange={(e) => setInboxSearch(e.target.value)}
                  placeholder="筛选任务…"
                  className="w-36 px-2.5 py-1 rounded-lg border border-hairline surface-rail text-xs outline-none"
                />
              </div>
              <div className="max-h-56 overflow-y-auto divide-y divide-[var(--line)] rounded-xl border border-hairline">
                {filteredInbox.slice(0, 20).map((act) => {
                  const tag = actionCategoryTag(act);
                  const primaryChoice = act.choices?.[0];
                  return (
                    <div key={act.id} className="px-3.5 py-2 flex items-center justify-between gap-3 surface-hover">
                      <button
                        type="button"
                        onClick={() => {
                          if (act.entry_id) {
                            setInboxOpen(false);
                            jumpToEntry(act.entry_id);
                          } else if (act.target_worker) {
                            openWorkerByName(act.target_worker);
                          }
                        }}
                        className="min-w-0 flex items-center gap-2 text-left cursor-pointer flex-1"
                      >
                        <span className="px-1.5 py-0.5 rounded font-mono-code text-[10.5px] border border-hairline surface-rail text-secondary shrink-0">
                          {tag.label}
                        </span>
                        <span className="font-medium text-primary truncate">{act.title}</span>
                      </button>
                      <div className="flex items-center gap-1.5 shrink-0">
                        {primaryChoice ? (
                          <button
                            type="button"
                            onClick={() =>
                              void api(`/api/super/actions/${encodeURIComponent(act.id)}/resolve`, {
                                choice: primaryChoice.key || primaryChoice.label,
                                by: "web",
                              }).then(() => onToast("已拍板并同步"))
                            }
                            className="px-2 py-0.5 rounded-md btn-accent text-[11px] cursor-pointer"
                          >
                            {primaryChoice.label}
                          </button>
                        ) : act.kind === "router_check" ? (
                          <button
                            type="button"
                            onClick={() =>
                              void api(`/api/super/actions/${encodeURIComponent(act.id)}/resolve`, {
                                choice: "ack",
                                by: "web",
                              }).then(() => onToast("已标记已看"))
                            }
                            className="inline-flex items-center gap-1 px-2 py-0.5 rounded-md border border-strong surface-bg surface-hover text-primary text-[11px] cursor-pointer"
                          >
                            <Check className="w-3 h-3" />
                            <span>已看</span>
                          </button>
                        ) : null}
                        {act.kind !== "worker_dialog" && (
                          <button
                            type="button"
                            onClick={() =>
                              void api(`/api/super/actions/${encodeURIComponent(act.id)}/snooze`, {}).then(() =>
                                onToast("已暂缓至明天晨报提醒")
                              )
                            }
                            className="px-2 py-0.5 rounded-md border border-strong text-secondary surface-hover text-[11px] cursor-pointer"
                          >
                            明天
                          </button>
                        )}
                        <button
                          type="button"
                          onClick={() =>
                            void api(`/api/super/actions/${encodeURIComponent(act.id)}/dismiss`, {}).then(() =>
                              onToast("已忽略待办")
                            )
                          }
                          className="text-muted hover:text-primary px-1 cursor-pointer"
                          title="忽略"
                        >
                          ×
                        </button>
                      </div>
                    </div>
                  );
                })}
                {filteredInbox.length > 20 && (
                  <div className="px-3.5 py-2 text-center text-[11.5px] text-muted surface-rail">
                    已展示前 20 条（共 {filteredInbox.length} 条），支持按分类或搜索快速筛选
                  </div>
                )}
              </div>
            </div>
          )}
        </div>
      )}

      {pinnedList.length > 0 && (
        <div className="px-5 py-2 border-b border-hairline surface-rail text-xs shrink-0 space-y-1.5">
          <button type="button" onClick={() => setPinnedOpen((v) => !v)} className="flex items-center gap-1.5 text-secondary font-medium cursor-pointer">
            <Pin className="w-3.5 h-3.5" />
            <span>已置顶 {pinnedList.length} 条</span>
          </button>
          {pinnedOpen && (
            <div className="space-y-1 max-h-36 overflow-y-auto">
              {pinnedList.map((p) => (
                <div key={p.id} className="px-2.5 py-1.5 rounded-lg surface-bg border border-hairline flex items-center justify-between gap-2">
                  <span className="truncate text-secondary">{p.text}</span>
                  <button type="button" onClick={() => void api("/api/super/pin", { id: p.id, pinned: false })} className="text-muted hover:underline shrink-0 cursor-pointer">
                    取消置顶
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      <div className="relative flex-1 flex flex-col min-h-0">
        <div ref={containerRef} onScroll={onScroll} className="flex-1 overflow-y-auto scroll-chat px-5 pt-6 pb-2 min-h-0">
          <div ref={contentRef} className="max-w-[740px] mx-auto">
            <div className="space-y-5">
            {canLoadMore && (
            <div className="text-center">
              <button type="button" disabled={loadingOlder} onClick={() => void loadEarlier()} className="px-3.5 py-1 rounded-full text-xs border border-strong text-secondary surface-hover cursor-pointer disabled:opacity-50">
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
            return visibleEntries.map((entry, idx) => {
              const hlClass = highlightedEntryId === entry.id ? "entry-highlight px-2 py-1.5 -mx-2 transition-colors" : "transition-colors";
              if (entry.role === "event") {
                quoteState.lastEventRef = entry.ref ?? "";
                const eventAction = entry.action_id ? resolvedActions[entry.action_id] : undefined;
                if (eventAction && (eventAction.status === "resolved" || eventAction.status === "snoozed")) {
                  const statusNote =
                    eventAction.kind === "worker_dialog"
                      ? eventAction.chosen_key?.toLowerCase() === "esc"
                        ? "已向终端发送取消键 (Esc)"
                        : `已将确认发送至 ${eventAction.target_worker || "终端"}`
                      : undefined;
                  return (
                    <div key={entry.id} id={`entry-${entry.id}`} data-entry-id={entry.id} className={`pl-0 sm:pl-10 ${hlClass}`}>
                      <DecisionReceiptCard
                        action={eventAction}
                        at={entry.at}
                        originEntryId={eventAction.entry_id}
                        statusNote={statusNote}
                        onJumpToEntry={jumpToEntry}
                        onOpenWorker={openWorkerByName}
                      />
                    </div>
                  );
                }
                const histRef = entry.ref?.startsWith("h:") ? entry.ref.slice(2) : "";
                if (histRef) {
                  return (
                    <div key={entry.id} id={`entry-${entry.id}`} data-entry-id={entry.id} className={`flex items-center gap-2.5 text-xs text-muted ${hlClass}`}>
                      <StatusDot status={entry.tone === "error" ? "blocked" : "idle"} />
                      <span className="font-mono-code text-[11px]">{new Date(entry.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</span>
                      <button
                        type="button"
                        onClick={() => void openHistoryMatch((s) => s.id === histRef)}
                        className="inline-flex items-center gap-1.5 text-left hover:text-primary cursor-pointer group"
                      >
                        <span className="group-hover:underline">{entry.text}</span>
                        <span className="px-1.5 py-0.5 rounded border border-hairline surface-rail text-[10.5px] text-secondary shrink-0">
                          查看旧会话 →
                        </span>
                      </button>
                    </div>
                  );
                }
                if (entry.tone === "spawn" && entry.ref) {
                  const wk = data?.workers.find((w) => w.name === entry.ref);
                  const badge = workerSpawnStatusBadge(wk);
                  return (
                    <div key={entry.id} id={`entry-${entry.id}`} data-entry-id={entry.id} className={`flex items-center gap-2.5 text-xs text-muted ${hlClass}`}>
                      <StatusDot status={badge.dot} />
                      <span className="font-mono-code text-[11px]">{new Date(entry.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</span>
                      <button
                        type="button"
                        onClick={() => openWorkerByName(entry.ref!)}
                        className="inline-flex flex-wrap items-center gap-1.5 text-left hover:text-primary cursor-pointer group"
                      >
                        <span className="group-hover:underline">{entry.text}</span>
                        <span className="px-1.5 py-0.5 rounded-full border border-hairline surface-rail text-[10.5px] text-secondary shrink-0">
                          {badge.label}
                        </span>
                      </button>
                    </div>
                  );
                }
                if (entry.ref === "corgi" && router?.pane_id) {
                  return (
                    <div key={entry.id} id={`entry-${entry.id}`} data-entry-id={entry.id} className={`flex items-center gap-2.5 text-xs text-muted ${hlClass}`}>
                      <StatusDot status={entry.tone === "error" || entry.tone === "blocked" ? "blocked" : "idle"} />
                      <span className="font-mono-code text-[11px]">{new Date(entry.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</span>
                      <button
                        type="button"
                        onClick={() => router.pane_id && onSelectWorker(router.pane_id)}
                        className="text-left hover:text-primary hover:underline cursor-pointer"
                      >
                        {entry.text}
                      </button>
                    </div>
                  );
                }
                return (
                  <div key={entry.id} id={`entry-${entry.id}`} data-entry-id={entry.id} className={`flex items-center gap-2.5 text-xs text-muted ${hlClass}`}>
                    <StatusDot status={entry.tone === "blocked" || entry.tone === "error" ? "blocked" : entry.tone === "done" ? "done" : "idle"} />
                    <span className="font-mono-code text-[11px]">{new Date(entry.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</span>
                    <span>{entry.text}</span>
                  </div>
                );
              }
              if (entry.role === "user") {
                quoteState.lastUserId = entry.id;
                quoteState.seenSinceUser = false;
                quoteState.lastEventRef = "";
                const directAct = entry.action_id ? resolvedActions[entry.action_id] : undefined;
                const legacyActId = !directAct && entry.re?.entry ? entryActionById.get(entry.re.entry) : undefined;
                const legacyAct = legacyActId ? resolvedActions[legacyActId] : undefined;
                const receiptAction =
                  directAct?.status === "resolved"
                    ? directAct
                    : legacyAct?.status === "resolved" && legacyAct.chosen_label === entry.text
                      ? legacyAct
                      : undefined;
                if (receiptAction) {
                  const hasFollowingReply = visibleEntries.slice(idx + 1, idx + 5).some((n) => n.role === "corgi");
                  const isLatestUserEntry = idx === visibleEntries.length - 1;
                  const statusNote = hasFollowingReply
                    ? "Corgi 已响应"
                    : isLatestUserEntry && routerWorking
                      ? "Corgi 正在处理…"
                      : "已同步至对话";
                  return (
                    <div key={entry.id} id={`entry-${entry.id}`} data-entry-id={entry.id} className={`pl-0 sm:pl-10 ${hlClass}`}>
                      <DecisionReceiptCard
                        action={receiptAction}
                        at={entry.at}
                        originEntryId={entry.re?.entry || receiptAction.entry_id}
                        statusNote={statusNote}
                        onJumpToEntry={jumpToEntry}
                        onFollowUp={() => setReplyTo(entry.action_id ? entry : { ...entry, action_id: receiptAction.id })}
                        onOpenWorker={openWorkerByName}
                      />
                    </div>
                  );
                }
                return (
                  <div key={entry.id} id={`entry-${entry.id}`} data-entry-id={entry.id} className={`flex flex-col items-end gap-1 ${hlClass}`}>
                    {entry.re && <QuoteBlock quote={entry.re} align="right" onJumpToEntry={jumpToEntry} />}
                    <div className="max-w-[80%] px-4 py-2.5 rounded-2xl rounded-br-sm text-[14px] leading-relaxed pill-user whitespace-pre-wrap">{entry.text}</div>
                    <span className="text-[11px] text-muted pr-1">
                      {entry.channel === "wecom" ? "企业微信 · " : ""}
                      {new Date(entry.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
                    </span>
                  </div>
                );
              }
              const hideQuote = shouldHideRedundantQuote(entry, quoteState);
              quoteState.seenSinceUser = true;
              quoteState.lastEventRef = "";
              const inlineAction = pendingByEntryId.get(entry.id);
              const inlineBusy = inlineAction ? busyActionId === inlineAction.id : false;
              return (
                <div key={entry.id} id={`entry-${entry.id}`} data-entry-id={entry.id} className={`group flex items-start gap-3 ${hlClass}`}>
                  <div className="w-7 h-7 rounded-lg border border-hairline flex items-center justify-center shrink-0 mt-0.5">
                    <CorgiMark className="w-4 h-4" />
                  </div>
                  <div className="flex-1 min-w-0 space-y-1.5">
                    {entry.re && !hideQuote && <QuoteBlock quote={entry.re} align="left" onJumpToEntry={jumpToEntry} />}
                    <div className="text-[14.5px]">
                      <MarkdownText text={entry.text} onOpenFile={onOpenFile} />
                    </div>
                    {inlineAction && (
                      <div className="pt-1 flex flex-wrap items-center gap-1.5">
                        {inlineAction.kind === "router_check" || !inlineAction.choices?.length ? (
                          <button
                            type="button"
                            disabled={inlineBusy}
                            onClick={() => void resolveInlineAction(inlineAction, "ack", "已标记已看")}
                            className="inline-flex items-center gap-1 px-2.5 py-1 rounded-full text-xs border border-strong surface-bg surface-hover text-primary cursor-pointer disabled:opacity-50"
                          >
                            <Check className="w-3.5 h-3.5" />
                            <span>已看</span>
                          </button>
                        ) : (
                          inlineAction.choices.map((ch, cIdx) => (
                            <button
                              key={ch.key || cIdx}
                              type="button"
                              disabled={inlineBusy}
                              onClick={() => void resolveInlineAction(inlineAction, ch.key || ch.label, "已拍板并同步")}
                              className={`px-3 py-1 rounded-full text-xs cursor-pointer disabled:opacity-50 ${
                                cIdx === 0
                                  ? "btn-accent font-medium"
                                  : "border border-strong surface-bg surface-hover text-primary"
                              }`}
                            >
                              {ch.label}
                            </button>
                          ))
                        )}
                        <button
                          type="button"
                          disabled={inlineBusy}
                          onClick={() => setReplyTo(entry)}
                          className="px-2.5 py-1 rounded-full text-xs border border-hairline text-secondary surface-hover cursor-pointer disabled:opacity-50"
                        >
                          回复其他…
                        </button>
                        <button
                          type="button"
                          disabled={inlineBusy}
                          onClick={() => void dismissInlineAction(inlineAction)}
                          title="忽略待办"
                          className="px-1.5 py-1 rounded-full text-xs text-muted hover:text-primary cursor-pointer disabled:opacity-50"
                        >
                          ×
                        </button>
                      </div>
                    )}
                    <div className="flex items-center gap-3 text-[11px] text-muted opacity-0 group-hover:opacity-100 transition-opacity">
                      <button type="button" onClick={() => setReplyTo(entry)} className="hover:underline cursor-pointer">
                        引用回复
                      </button>
                      <button type="button" onClick={() => void api("/api/super/pin", { id: entry.id, pinned: !entry.pinned })} className="inline-flex items-center gap-1 hover:underline cursor-pointer">
                        <Pin className="w-3 h-3" />
                        {entry.pinned ? "取消置顶" : "置顶"}
                      </button>
                    </div>
                  </div>
                </div>
              );
            });
          })()}

          {topFloatingAction && (
            <div className="pl-0 sm:pl-10 space-y-2">
              <DecisionCard
                action={topFloatingAction}
                workerNames={activeWorkers.map((w) => w.name)}
                onResolve={async (id, key, custom) => {
                  await api(`/api/super/actions/${encodeURIComponent(id)}/resolve`, {
                    choice: key,
                    by: "web",
                    ...(custom ? { custom: true } : {}),
                  });
                  onToast(custom ? "已提交自定义回答并同步" : "已拍板并同步");
                }}
                onSnooze={async (id) => {
                  await api(`/api/super/actions/${encodeURIComponent(id)}/snooze`, {});
                  onToast("已暂缓至明天晨报再次提醒");
                }}
                onDismiss={async (id) => {
                  await api(`/api/super/actions/${encodeURIComponent(id)}/dismiss`, {});
                  onToast("已忽略待办");
                }}
                onOpenWorker={openWorkerByName}
                onOpenFile={(p) => onOpenFile(p)}
              />
              {floatingActions.length > 1 && (
                <div className="flex items-center justify-between px-4 py-2 rounded-xl border border-hairline surface-rail text-xs">
                  <span className="text-secondary">
                    还有 <strong>{floatingActions.length - 1}</strong> 项待确认已折叠在顶部收件箱
                  </span>
                  <button
                    type="button"
                    onClick={() => setInboxOpen(true)}
                    className="font-medium text-primary hover:underline cursor-pointer"
                  >
                    展开全部 {pendingActions.length} 项 →
                  </button>
                </div>
              )}
            </div>
          )}

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
                <span>{routerWorking ? (router?.replied ? "已回复，在整理记忆…" : "Corgi 在想…") : "\u00A0"}</span>
              </div>
            )}
          </div>
        </div>
        {showJumpBottom && (
          <button
            type="button"
            onClick={() => scrollToBottom(true)}
            className="absolute bottom-3 left-1/2 -translate-x-1/2 inline-flex items-center gap-1 px-3 py-1 rounded-full text-xs card-floating border-strong text-secondary surface-hover cursor-pointer"
          >
            <ArrowDown className="w-3.5 h-3.5" />
            <span>回到最新</span>
          </button>
        )}
      </div>

      <div className="px-5 pb-4 pt-2 shrink-0 space-y-2">
        <PromptQueueTray
          items={effectiveQueue}
          paused={Boolean(data?.chat_queue_paused)}
          busyId={queueBusyId}
          onSteer={async (item) => {
            if (item.id.startsWith("tmp-")) return;
            setQueueBusyId(item.id);
            setRemovedQueueIds((prev) => [...prev, item.id]);
            try {
              await api(`/api/super/queue/${encodeURIComponent(item.id)}/steer`, { thread });
              scrollToBottom(true);
            } catch (err) {
              setRemovedQueueIds((prev) => prev.filter((x) => x !== item.id));
              onToast(err instanceof Error ? err.message : "引导发送失败");
            } finally {
              setQueueBusyId("");
            }
          }}
          onCancel={async (item) => {
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
            try {
              await api("/api/super/queue/resume", { thread });
            } catch (err) {
              onToast(err instanceof Error ? err.message : "恢复队列失败");
            }
          }}
        />
        <ComposerBar
          embedded
          machines={machines}
          replyTo={replyTo}
          working={router?.status === "working"}
          stopping={stopping}
          queuedCount={effectiveQueue.length}
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
          onClearReply={() => setReplyTo(null)}
          onToast={onToast}
          onSend={async ({ text, machine, kind, model, effort, clientMsgId }) => {
            const reId = replyTo?.id ?? "";
            const shouldQueue = router?.status === "working" || effectiveQueue.length > 0;
            const tmpId = shouldQueue ? `tmp-${++tmpSeqRef.current}` : "";
            if (tmpId) {
              setOptimisticQueued((prev) => [...prev, { id: tmpId, thread, text, created_at: new Date().toISOString() }]);
            }
            try {
              const res = await api<{ ok: boolean; queued?: boolean; item?: QueuedPrompt }>("/api/super/say", {
                thread,
                text,
                re: reId,
                machine,
                kind,
                model,
                effort,
                client_msg_id: clientMsgId,
                mode: shouldQueue ? "queue" : "auto",
              });
              setReplyTo(null);
              if (tmpId) {
                setOptimisticQueued((prev) => {
                  const without = prev.filter((x) => x.id !== tmpId);
                  return res.queued && res.item ? [...without, res.item] : without;
                });
              } else if (res.queued && res.item) {
                setOptimisticQueued((prev) => [...prev, res.item!]);
              }
              if (!res.queued) {
                scrollToBottom(true);
              }
            } catch (err) {
              if (tmpId) {
                setOptimisticQueued((prev) => prev.filter((x) => x.id !== tmpId));
              }
              onToast(err instanceof Error ? err.message : "发送失败");
              throw err;
            }
          }}
        />
      </div>

      {historyPreview && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center p-4"
          style={{ background: "rgba(0,0,0,0.4)" }}
          onClick={() => setHistoryPreview(null)}
        >
          <div
            onClick={(e) => e.stopPropagation()}
            className="w-full max-w-[680px] max-h-[82vh] flex flex-col rounded-2xl card-floating overflow-hidden"
          >
            <div className="px-5 py-3.5 border-b border-hairline flex items-center justify-between gap-2">
              <div className="min-w-0">
                <div className="font-semibold text-sm truncate">
                  {historyPreview.sess.title || historyPreview.sess.name || historyPreview.sess.kind}
                </div>
                <div className="text-[11px] font-mono-code text-muted truncate">{historyPreview.sess.cwd}</div>
              </div>
              <div className="flex items-center gap-2 shrink-0">
                <button
                  type="button"
                  onClick={() => {
                    const sess = historyPreview.sess;
                    void api<{ pane_id: string }>(`/api/history/${encodeURIComponent(sess.id)}/resume`, {})
                      .then((res) => {
                        setHistoryPreview(null);
                        onToast(`已续接 ${sess.title || sess.name || sess.kind}`);
                        onSelectWorker(res.pane_id);
                      })
                      .catch((err) => onToast(err instanceof Error ? err.message : "续接失败"));
                  }}
                  className="px-3 py-1.5 rounded-lg text-xs btn-accent cursor-pointer"
                >
                  在此续接会话
                </button>
                <button type="button" onClick={() => setHistoryPreview(null)} className="text-muted cursor-pointer">
                  <X className="w-4 h-4" />
                </button>
              </div>
            </div>
            <div className="flex-1 overflow-y-auto p-5 space-y-3 text-xs">
              {historyPreview.msgs.length === 0 && (
                <div className="text-muted text-center py-8">暂无可展示的对话消息</div>
              )}
              {historyPreview.msgs.map((m, i) =>
                m.kind === "user" ? (
                  <div key={i} className="flex justify-end">
                    <div className="max-w-[80%] px-3.5 py-2 rounded-2xl pill-user whitespace-pre-wrap">{m.text}</div>
                  </div>
                ) : m.kind === "assistant" ? (
                  <div key={i} className="text-[13.5px]">
                    <MarkdownText text={m.text ?? ""} onOpenFile={onOpenFile} />
                  </div>
                ) : null
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

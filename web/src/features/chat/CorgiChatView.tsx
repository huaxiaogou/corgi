import { useEffect, useRef, useState } from "react";
import { ArrowDown, Pin, X } from "lucide-react";
import { CorgiButtMark, CorgiMark, StatusDot } from "../../components/CorgiMark";
import { DecisionCard } from "../../components/DecisionCard";
import { DecisionReceiptCard } from "../../components/DecisionReceiptCard";
import { MarkdownText } from "../../components/MarkdownText";
import { PromptQueueTray } from "../../components/PromptQueueTray";
import { useAutoScroll } from "../../hooks/useAutoScroll";
import { api } from "../../lib/api";
import type { Action, Entry, Machine, QueuedPrompt, Settings, SuperView } from "../../types/api";
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
  if (a.kind === "patrol_digest") return { key: "digest", label: "巡检简报" };
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
  const topAction = pendingActions[0] ?? null;
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

  const { containerRef, contentRef, onScroll, showJumpBottom, scrollToBottom, capturePrependAnchor } = useAutoScroll({
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
    if (wk?.pane_id) onSelectWorker(wk.pane_id);
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
    <div className="flex-1 flex flex-col min-h-0 overflow-hidden">
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
                      <div className="min-w-0 flex items-center gap-2">
                        <span className="px-1.5 py-0.5 rounded font-mono-code text-[10.5px] border border-hairline surface-rail text-secondary shrink-0">
                          {tag.label}
                        </span>
                        <span className="font-medium text-primary truncate">{act.title}</span>
                      </div>
                      <div className="flex items-center gap-1.5 shrink-0">
                        {primaryChoice && (
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
                        )}
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

          {entries.map((entry, idx) => {
            const hlClass = highlightedEntryId === entry.id ? "entry-highlight px-2 py-1.5 -mx-2 transition-colors" : "transition-colors";
            if (entry.role === "event") {
              const eventAction = entry.action_id ? resolvedActions[entry.action_id] : undefined;
              if (eventAction && (eventAction.status === "resolved" || eventAction.status === "snoozed")) {
                const statusNote =
                  eventAction.kind === "worker_dialog"
                    ? eventAction.chosen_key?.toLowerCase() === "esc"
                      ? "已向终端发送取消键 (Esc)"
                      : `已将确认发送至 ${eventAction.target_worker || "终端"}`
                    : undefined;
                return (
                  <div key={entry.id} id={`entry-${entry.id}`} className={`pl-0 sm:pl-10 ${hlClass}`}>
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
              return (
                <div key={entry.id} id={`entry-${entry.id}`} className={`flex items-center gap-2.5 text-xs text-muted ${hlClass}`}>
                  <StatusDot status={entry.tone === "blocked" ? "blocked" : entry.tone === "done" ? "done" : "idle"} />
                  <span className="font-mono-code text-[11px]">{new Date(entry.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</span>
                  <span>{entry.text}</span>
                </div>
              );
            }
            if (entry.role === "user") {
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
                const hasFollowingReply = entries.slice(idx + 1, idx + 5).some((n) => n.role === "corgi");
                const isLatestUserEntry = idx === entries.length - 1;
                const statusNote = hasFollowingReply
                  ? "Corgi 已响应"
                  : isLatestUserEntry && routerWorking
                    ? "Corgi 正在处理…"
                    : "已同步至对话";
                return (
                  <div key={entry.id} id={`entry-${entry.id}`} className={`pl-0 sm:pl-10 ${hlClass}`}>
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
                <div key={entry.id} id={`entry-${entry.id}`} className={`flex flex-col items-end gap-1 ${hlClass}`}>
                  {entry.re && (
                    <button
                      type="button"
                      onClick={() => entry.re?.entry && jumpToEntry(entry.re.entry)}
                      className={`max-w-[82%] text-left text-[11.5px] text-muted px-2.5 py-1 rounded-lg surface-rail border border-hairline truncate ${entry.re.entry ? "hover:text-primary cursor-pointer" : "cursor-default"}`}
                      title={entry.re.text}
                    >
                      {entry.re.who}：{entry.re.text}
                    </button>
                  )}
                  <div className="max-w-[80%] px-4 py-2.5 rounded-2xl rounded-br-sm text-[14px] leading-relaxed pill-user whitespace-pre-wrap">{entry.text}</div>
                  <span className="text-[11px] text-muted pr-1">
                    {entry.channel === "wecom" ? "企业微信 · " : ""}
                    {new Date(entry.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
                  </span>
                </div>
              );
            }
            return (
              <div key={entry.id} id={`entry-${entry.id}`} className={`group flex items-start gap-3 ${hlClass}`}>
                <div className="w-7 h-7 rounded-lg border border-hairline flex items-center justify-center shrink-0 mt-0.5">
                  <CorgiMark className="w-4 h-4" />
                </div>
                <div className="flex-1 min-w-0 space-y-1.5">
                  {entry.re && (
                    <div className="text-xs text-muted pl-2.5 border-l-2 border-strong truncate">
                      {entry.re.who}：{entry.re.text}
                    </div>
                  )}
                  <div className="text-[14.5px]">
                    <MarkdownText text={entry.text} onOpenFile={(p) => onOpenFile(p)} />
                  </div>
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
          })}

          {topAction && (
            <div className="pl-0 sm:pl-10 space-y-2">
              <DecisionCard
                action={topAction}
                onResolve={async (id, key) => {
                  await api(`/api/super/actions/${encodeURIComponent(id)}/resolve`, { choice: key, by: "web" });
                  onToast("已拍板并同步");
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
              />
              {pendingActions.length > 1 && (
                <div className="flex items-center justify-between px-4 py-2 rounded-xl border border-hairline surface-rail text-xs">
                  <span className="text-secondary">
                    还有 <strong>{pendingActions.length - 1}</strong> 项待确认已折叠在顶部收件箱
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
          onSend={async ({ text, machine, kind, model, effort }) => {
            const reId = replyTo?.id ?? "";
            setReplyTo(null);
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
              if (!res.queued) {
                scrollToBottom(true);
              }
            } catch (err) {
              if (tmpId) {
                setOptimisticQueued((prev) => prev.filter((x) => x.id !== tmpId));
              }
              onToast(err instanceof Error ? err.message : "发送失败");
            }
          }}
        />
      </div>
    </div>
  );
}

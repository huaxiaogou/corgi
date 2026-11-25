import { useEffect, useRef, useState } from "react";
import { ArrowDown, Pin, X } from "lucide-react";
import { CorgiButtMark, CorgiMark, StatusDot } from "../../components/CorgiMark";
import { DecisionCard } from "../../components/DecisionCard";
import { MarkdownText } from "../../components/MarkdownText";
import { PromptQueueTray } from "../../components/PromptQueueTray";
import { useAutoScroll } from "../../hooks/useAutoScroll";
import { api } from "../../lib/api";
import type { Entry, Machine, QueuedPrompt, Settings, SuperView } from "../../types/api";
import { ChatHeader } from "./ChatHeader";
import { ComposerBar } from "./ComposerBar";

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
  const [olderEntries, setOlderEntries] = useState<Entry[]>([]);
  const [moreOlder, setMoreOlder] = useState<boolean | null>(null);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [optimisticQueued, setOptimisticQueued] = useState<QueuedPrompt[]>([]);
  const [removedQueueIds, setRemovedQueueIds] = useState<string[]>([]);
  const [queueBusyId, setQueueBusyId] = useState("");
  const [stopping, setStopping] = useState(false);
  const tmpSeqRef = useRef(0);

  const serverQueuedChat = data?.queued_chat ?? [];
  const serverQueueSig = serverQueuedChat.map((q) => q.id).join(",");

  useEffect(() => {
    setOlderEntries([]);
    setMoreOlder(null);
    setOptimisticQueued([]);
    setRemovedQueueIds([]);
  }, [thread]);

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
  const canLoadMore = moreOlder ?? Boolean(data?.earlier);
  const pinnedList = (data?.pinned ?? []).filter((e) => !thread || e.thread === thread);
  const pendingActions = (data?.actions ?? []).filter((a) => a.status === "pending" && (!thread || a.thread === thread));
  const activeWorkers = (data?.workers ?? []).filter((w) => !w.ended);
  const lastEntry = liveEntries[liveEntries.length - 1];
  const router = data?.router;

  const { containerRef, contentRef, onScroll, showJumpBottom, scrollToBottom, capturePrependAnchor } = useAutoScroll({
    resetKey: `${thread}:${focusSeq}`,
    contentKey: [
      liveEntries.length,
      lastEntry?.id ?? "",
      lastEntry?.text.length ?? 0,
      pendingActions.map((a) => a.id).join(","),
      router?.status ?? "",
      router?.replied ? "1" : "0",
    ].join("|"),
    prependKey: olderEntries.length,
    hasContent: liveEntries.length > 0 || pendingActions.length > 0,
  });

  const loadEarlier = async () => {
    if (loadingOlder || entries.length === 0) return;
    setLoadingOlder(true);
    try {
      const res = await api<{ entries: Entry[]; more: boolean }>(
        `/api/super/earlier?thread=${encodeURIComponent(thread)}&before=${entries[0].seq}&limit=80`
      );
      const incoming = res.entries ?? [];
      if (incoming.length > 0) {
        capturePrependAnchor();
        setOlderEntries((prev) => [...incoming, ...prev]);
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
              </span>
            </button>
          ))}
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
        <div ref={containerRef} onScroll={onScroll} className="flex-1 overflow-y-auto scroll-chat px-5 py-6 min-h-0">
          <div ref={contentRef} className="max-w-[740px] mx-auto space-y-5">
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

          {entries.map((entry) => {
            if (entry.role === "event") {
              return (
                <div key={entry.id} className="flex items-center gap-2.5 text-xs text-muted">
                  <StatusDot status={entry.tone === "blocked" ? "blocked" : entry.tone === "done" ? "done" : "idle"} />
                  <span className="font-mono-code text-[11px]">{new Date(entry.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</span>
                  <span>{entry.text}</span>
                </div>
              );
            }
            if (entry.role === "user") {
              return (
                <div key={entry.id} className="flex flex-col items-end gap-1">
                  <div className="max-w-[80%] px-4 py-2.5 rounded-2xl rounded-br-sm text-[14px] leading-relaxed pill-user whitespace-pre-wrap">{entry.text}</div>
                  <span className="text-[11px] text-muted pr-1">
                    {entry.channel === "wecom" ? "企业微信 · " : ""}
                    {new Date(entry.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
                  </span>
                </div>
              );
            }
            return (
              <div key={entry.id} className="group flex items-start gap-3">
                <div className="w-7 h-7 rounded-lg border border-hairline flex items-center justify-center shrink-0 mt-0.5">
                  <CorgiMark className="w-4 h-4" />
                </div>
                <div className="flex-1 min-w-0 space-y-1.5">
                  {entry.re && <div className="text-xs text-muted pl-2.5 border-l-2 border-strong truncate">{entry.re.who}：{entry.re.text}</div>}
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

          {pendingActions.map((act) => (
            <div key={act.id} className="pl-0 sm:pl-10">
              <DecisionCard
                action={act}
                onResolve={async (id, key) => {
                  await api(`/api/super/actions/${encodeURIComponent(id)}/resolve`, { choice: key, by: "web" });
                  onToast("已拍板并同步");
                }}
                onDismiss={async (id) => {
                  await api(`/api/super/actions/${encodeURIComponent(id)}/dismiss`, {});
                  onToast("已忽略待办");
                }}
                onOpenWorker={(name) => {
                  const wk = data?.workers.find((w) => w.name === name);
                  if (wk?.pane_id) onSelectWorker(wk.pane_id);
                }}
              />
            </div>
          ))}

          {router?.status === "working" && (
            <div className="flex items-center gap-2.5 text-xs text-muted pt-1">
              <StatusDot status="working" />
              <span>{router.replied ? "已回复，在整理记忆…" : "Corgi 在想…"}</span>
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

import { lazy, Suspense, useEffect, useRef, useState, type DragEvent, type FormEvent } from "react";
import { ArrowDown } from "lucide-react";
import { ChunkErrorBoundary } from "../../components/ChunkErrorBoundary";
import { useAutoScroll } from "../../hooks/useAutoScroll";
import { useKeyboardViewport } from "../../hooks/useKeyboardViewport";
import { formatAgentComposerContext } from "../../lib/agentIdentity";
import { api, uploadFile } from "../../lib/api";
import type { AgentView, Approval, ChatMessage, MsgPatch, QueuedPrompt } from "../../types/api";
import { AgentChatThread, type PendingPrompt } from "./AgentChatThread";
import { AgentPaneComposer } from "./AgentPaneComposer";
import { AgentPaneHeader } from "./AgentPaneHeader";

interface Props {
  pane: AgentView;
  stateReady?: boolean;
  focusSeq?: number;
  queuedPrompts?: QueuedPrompt[];
  queuePaused?: boolean;
  onBackToCorgi: () => void;
  onClosed: () => void;
  onOpenFile?: (path: string, machine?: string) => void;
  onToast: (msg: string) => void;
}

const LazyTerminalScreen = lazy(() =>
  import("../../components/TerminalScreen").then((m) => ({ default: m.TerminalScreen }))
);

const SETUP_RE = /choose your color scheme|welcome to antigravity|select login method|waiting for auth|do you trust|trust this folder|terms of service/i;

function computeCursorMoveKeys(ap: Approval, targetIdx: number): string[] {
  let active = ap.choices.findIndex((c) => c.active);
  if (active < 0) active = 0;
  const keys: string[] = [];
  while (active < targetIdx) {
    keys.push("down");
    active++;
  }
  while (active > targetIdx) {
    keys.push("up");
    active--;
  }
  keys.push("enter");
  return keys;
}

function reconcilePendingPrompts(pending: PendingPrompt[], liveMsgs: ChatMessage[], liveBase: number): PendingPrompt[] {
  if (pending.length === 0 || liveMsgs.length === 0) return pending;
  const remainingUsers = new Map<number, string>();
  for (let i = 0; i < liveMsgs.length; i++) {
    const m = liveMsgs[i];
    if (m.kind === "user" && m.text) remainingUsers.set(liveBase + i, m.text.trim());
  }
  if (remainingUsers.size === 0) return pending;

  let changed = false;
  const next: PendingPrompt[] = [];
  for (const p of pending) {
    const needle = p.text.trim();
    let matched = false;
    if (needle) {
      for (const [absIdx, hay] of remainingUsers) {
        if (absIdx < p.afterAbsIdx) continue;
        const pos = hay.indexOf(needle);
        if (pos !== -1) {
          remainingUsers.set(absIdx, hay.slice(0, pos) + hay.slice(pos + needle.length));
          matched = true;
          break;
        }
      }
    }
    if (matched) changed = true;
    else next.push(p);
  }
  return changed ? next : pending;
}

export function AgentPaneView({
  pane,
  stateReady = true,
  focusSeq = 0,
  queuedPrompts = [],
  queuePaused = false,
  onBackToCorgi,
  onClosed,
  onOpenFile,
  onToast,
}: Props) {
  const [mode, setMode] = useState<"chat" | "screen" | "log">(!pane.is_agent ? "screen" : "chat");
  const [olderMsgs, setOlderMsgs] = useState<ChatMessage[]>([]);
  const [liveMsgs, setLiveMsgs] = useState<ChatMessage[]>([]);
  const [liveBase, setLiveBase] = useState(0);
  const [resetSeq, setResetSeq] = useState(0);
  const [patchSeq, setPatchSeq] = useState(0);
  const [pendingPrompts, setPendingPrompts] = useState<PendingPrompt[]>([]);
  const [optimisticQueued, setOptimisticQueued] = useState<QueuedPrompt[]>([]);
  const [removedQueueIds, setRemovedQueueIds] = useState<string[]>([]);
  const [queueBusyId, setQueueBusyId] = useState("");
  const [stopping, setStopping] = useState(false);
  const [earlierCursor, setEarlierCursor] = useState(0);
  const [loadingEarlier, setLoadingEarlier] = useState(false);
  const [logText, setLogText] = useState("");
  const [approval, setApproval] = useState<Approval | null>(null);
  const [liveStatus, setLiveStatus] = useState(pane.status);
  const [liveTail, setLiveTail] = useState("");
  const [liveMeta, setLiveMeta] = useState("");
  const [contextPct, setContextPct] = useState(0);
  const [input, setInput] = useState("");
  const [attachments, setAttachments] = useState<{ name: string; path: string }[]>([]);
  const [showKeys, setShowKeys] = useState(false);
  const baseRef = useRef(0);
  const pendingSeqRef = useRef(0);
  const prevServerQueueRef = useRef<QueuedPrompt[]>(queuedPrompts);
  const localHandledIdsRef = useRef<Set<string>>(new Set());
  const composerDockRef = useRef<HTMLDivElement>(null);
  useKeyboardViewport(composerDockRef);

  useEffect(() => {
    setMode(!pane.is_agent ? "screen" : "chat");
    setShowKeys(false);
    setPendingPrompts([]);
    setOptimisticQueued([]);
    setRemovedQueueIds([]);
    prevServerQueueRef.current = queuedPrompts;
    localHandledIdsRef.current.clear();
    void api("/api/seen", { pane: pane.pane_id });
  }, [pane.pane_id, pane.is_agent]);

  const serverQueueSig = queuedPrompts.map((q) => q.id).join(",");

  useEffect(() => {
    const curIds = new Set(queuedPrompts.map((q) => q.id));
    for (const item of prevServerQueueRef.current) {
      if (!curIds.has(item.id) && !localHandledIdsRef.current.has(item.id)) {
        const pid = `d-${++pendingSeqRef.current}`;
        const afterAbsIdx = baseRef.current + liveMsgs.length;
        setPendingPrompts((p) => [...p, { id: pid, text: item.text, afterAbsIdx, sending: false }]);
      }
    }
    prevServerQueueRef.current = queuedPrompts;
    setOptimisticQueued((list) => {
      const next = list.filter((q) => !curIds.has(q.id));
      return next.length === list.length ? list : next;
    });
    setRemovedQueueIds((ids) => {
      const next = ids.filter((id) => curIds.has(id));
      return next.length === ids.length ? ids : next;
    });
    for (const id of Array.from(localHandledIdsRef.current)) {
      if (!curIds.has(id)) localHandledIdsRef.current.delete(id);
    }
  }, [serverQueueSig]);

  const effectiveQueue = [
    ...queuedPrompts.filter((q) => !removedQueueIds.includes(q.id)),
    ...optimisticQueued.filter((o) => !queuedPrompts.some((q) => q.id === o.id)),
  ];

  useEffect(() => {
    setLogText("");
    setOlderMsgs([]);
    setLiveMsgs([]);
    setLiveBase(0);
    setPendingPrompts([]);
    setEarlierCursor(0);
    baseRef.current = 0;
    setApproval(null);
    const enc = encodeURIComponent(pane.pane_id);
    const url = mode === "chat" ? `/api/panes/${enc}/chat` : `/api/panes/${enc}/stream?mode=${mode}`;
    const es = new EventSource(url);
    es.addEventListener("reset", () => {
      baseRef.current = 0;
      setLiveBase(0);
      setOlderMsgs([]);
      setLiveMsgs([]);
      setEarlierCursor(0);
      setResetSeq((n) => n + 1);
    });
    es.addEventListener("patch", (ev) => {
      const p = JSON.parse((ev as MessageEvent).data) as MsgPatch;
      setLiveMsgs((prev) => {
        const dropFront = Math.max(0, p.base - baseRef.current);
        const trimmed = dropFront > 0 ? prev.slice(dropFront) : prev;
        baseRef.current = p.base;
        const keepLen = Math.max(0, p.from - p.base);
        return [...trimmed.slice(0, keepLen), ...(p.msgs ?? [])];
      });
      setLiveBase(p.base);
      setEarlierCursor((cur) => (cur > 0 ? cur : p.earlier ?? 0));
      setPatchSeq((n) => n + 1);
    });
    es.addEventListener("messages", (ev) => {
      baseRef.current = 0;
      setLiveBase(0);
      setLiveMsgs((JSON.parse((ev as MessageEvent).data) as { messages?: ChatMessage[] }).messages ?? []);
      setPatchSeq((n) => n + 1);
    });
    es.addEventListener("log", (ev) => {
      setLogText((JSON.parse((ev as MessageEvent).data) as { text?: string }).text ?? "");
      setPatchSeq((n) => n + 1);
    });
    es.addEventListener("frame", (ev) => {
      setLogText((JSON.parse((ev as MessageEvent).data) as { text?: string }).text ?? "");
      setPatchSeq((n) => n + 1);
    });
    es.addEventListener("live", (ev) => {
      const p = JSON.parse((ev as MessageEvent).data) as { status?: string; approval?: Approval; tail?: string; meta?: string; context?: number };
      setLiveStatus(p.status ?? pane.status);
      setApproval(p.approval ?? null);
      setLiveTail(p.tail ?? "");
      setLiveMeta(p.meta ?? "");
      if (p.context) setContextPct(p.context);
    });
    return () => es.close();
  }, [pane.pane_id, mode]);

  useEffect(() => {
    setPendingPrompts((prev) => reconcilePendingPrompts(prev, liveMsgs, liveBase));
  }, [liveMsgs, liveBase]);

  const lastLive = liveMsgs[liveMsgs.length - 1];
  const scrollHasContent = mode === "log" ? logText.length > 0 : liveMsgs.length > 0 || pendingPrompts.length > 0 || Boolean(approval);
  const scrollContentKey =
    mode === "log"
      ? `log:${patchSeq}:${logText.length}`
      : [
          liveBase,
          patchSeq,
          liveMsgs.length,
          pendingPrompts.length,
          lastLive?.kind ?? "",
          lastLive?.text?.length ?? 0,
          lastLive?.input?.length ?? 0,
          lastLive?.output?.length ?? 0,
          liveStatus,
          liveTail,
          approval?.title ?? "",
          approval?.choices.length ?? 0,
        ].join("|");

  const { containerRef, contentRef, onScroll, showJumpBottom, scrollToBottom, capturePrependAnchor } = useAutoScroll({
    resetKey: `${pane.pane_id}:${mode}:${resetSeq}:${focusSeq}`,
    contentKey: scrollContentKey,
    prependKey: olderMsgs.length,
    hasContent: scrollHasContent,
    enabled: mode !== "screen",
  });

  const switchMode = (next: "chat" | "screen" | "log") => {
    if (next === mode) return;
    setLogText("");
    setOlderMsgs([]);
    setLiveMsgs([]);
    setEarlierCursor(0);
    baseRef.current = 0;
    setApproval(null);
    setMode(next);
  };

  const loadEarlier = async () => {
    if (!earlierCursor || loadingEarlier) return;
    setLoadingEarlier(true);
    try {
      const enc = encodeURIComponent(pane.pane_id);
      const res = await api<{ messages: ChatMessage[]; earlier: number }>(`/api/panes/${enc}/earlier?before=${earlierCursor}`);
      const incoming = res.messages ?? [];
      if (incoming.length > 0) {
        capturePrependAnchor();
        setOlderMsgs((prev) => [...incoming, ...prev]);
      }
      setEarlierCursor(res.earlier ?? 0);
    } catch (err) {
      onToast(err instanceof Error ? err.message : "加载历史失败");
    } finally {
      setLoadingEarlier(false);
    }
  };

  const addFiles = async (files: FileList | File[]) => {
    for (const f of Array.from(files)) {
      try {
        const up = await uploadFile(f, pane.machine);
        setAttachments((prev) => [...prev, { name: up.name, path: up.path }]);
      } catch (err) {
        onToast(err instanceof Error ? err.message : "上传失败");
      }
    }
  };

  const submitPromptText = async (rawText: string) => {
    const enc = encodeURIComponent(pane.pane_id);
    if (pane.is_agent && mode === "chat") {
      const shouldQueue = liveStatus === "working" || effectiveQueue.length > 0;
      if (shouldQueue) {
        const tmpId = `tmp-${++pendingSeqRef.current}`;
        setOptimisticQueued((prev) => [...prev, { id: tmpId, pane_id: pane.pane_id, text: rawText, created_at: new Date().toISOString() }]);
        try {
          const res = await api<{ ok: boolean; queued?: boolean; item?: QueuedPrompt }>(`/api/agents/${enc}/prompt`, { prompt: rawText, mode: "queue" });
          setOptimisticQueued((prev) => {
            const without = prev.filter((x) => x.id !== tmpId);
            return res.queued && res.item ? [...without, res.item] : without;
          });
          if (!res.queued) {
            const pid = `p-${++pendingSeqRef.current}`;
            setPendingPrompts((prev) => [...prev, { id: pid, text: rawText, afterAbsIdx: baseRef.current + liveMsgs.length, sending: false }]);
            scrollToBottom(true);
          }
        } catch (err) {
          setOptimisticQueued((prev) => prev.filter((x) => x.id !== tmpId));
          throw err;
        }
        return;
      }

      const id = `p-${++pendingSeqRef.current}`;
      const afterAbsIdx = baseRef.current + liveMsgs.length;
      setPendingPrompts((prev) => [...prev, { id, text: rawText, afterAbsIdx, sending: true }]);
      scrollToBottom(true);
      try {
        const res = await api<{ ok: boolean; queued?: boolean; item?: QueuedPrompt }>(`/api/agents/${enc}/prompt`, { prompt: rawText, mode: "auto" });
        if (res.queued && res.item) {
          setPendingPrompts((prev) => prev.filter((item) => item.id !== id));
          setOptimisticQueued((prev) => [...prev, res.item!]);
        } else {
          setPendingPrompts((prev) => prev.map((item) => (item.id === id ? { ...item, sending: false } : item)));
        }
      } catch (err) {
        setPendingPrompts((prev) => prev.filter((item) => item.id !== id));
        throw err;
      }
      return;
    }
    await api(`/api/panes/${enc}/text`, { text: rawText, enter: true });
    scrollToBottom(true);
  };

  const steerQueued = async (item: QueuedPrompt) => {
    if (!stateReady || item.id.startsWith("tmp-")) return;
    const enc = encodeURIComponent(pane.pane_id);
    const pid = `s-${++pendingSeqRef.current}`;
    const afterAbsIdx = baseRef.current + liveMsgs.length;
    setQueueBusyId(item.id);
    localHandledIdsRef.current.add(item.id);
    setRemovedQueueIds((prev) => [...prev, item.id]);
    setPendingPrompts((prev) => [...prev, { id: pid, text: item.text, afterAbsIdx, sending: true, steered: true }]);
    scrollToBottom(true);
    try {
      await api(`/api/panes/${enc}/queue/${encodeURIComponent(item.id)}/steer`, {});
      setPendingPrompts((prev) => prev.map((p) => (p.id === pid ? { ...p, sending: false } : p)));
    } catch (err) {
      localHandledIdsRef.current.delete(item.id);
      setRemovedQueueIds((prev) => prev.filter((x) => x !== item.id));
      setPendingPrompts((prev) => prev.filter((p) => p.id !== pid));
      onToast(err instanceof Error ? err.message : "引导发送失败");
    } finally {
      setQueueBusyId("");
    }
  };

  const cancelQueued = async (item: QueuedPrompt) => {
    if (!stateReady) return;
    if (item.id.startsWith("tmp-")) {
      setOptimisticQueued((prev) => prev.filter((x) => x.id !== item.id));
      return;
    }
    const enc = encodeURIComponent(pane.pane_id);
    setQueueBusyId(item.id);
    localHandledIdsRef.current.add(item.id);
    setRemovedQueueIds((prev) => [...prev, item.id]);
    try {
      await api(`/api/panes/${enc}/queue/${encodeURIComponent(item.id)}/cancel`, {});
    } catch (err) {
      localHandledIdsRef.current.delete(item.id);
      setRemovedQueueIds((prev) => prev.filter((x) => x !== item.id));
      onToast(err instanceof Error ? err.message : "取消排队失败");
    } finally {
      setQueueBusyId("");
    }
  };

  const resumeQueue = async () => {
    if (!stateReady) return;
    try {
      await api(`/api/panes/${encodeURIComponent(pane.pane_id)}/queue/resume`, {});
    } catch (err) {
      onToast(err instanceof Error ? err.message : "恢复队列失败");
    }
  };

  const stopCurrentStep = async () => {
    if (!stateReady || stopping) return;
    setStopping(true);
    try {
      const res = await api<{ ok: boolean; paused?: boolean }>(`/api/panes/${encodeURIComponent(pane.pane_id)}/stop`, {});
      onToast(res.paused ? "已停止当前步骤（待发队列已暂停保留）" : "已停止当前步骤 (Esc)");
    } catch (err) {
      onToast(err instanceof Error ? err.message : "停止失败");
    } finally {
      setStopping(false);
    }
  };

  const sendInput = async (e: FormEvent) => {
    e.preventDefault();
    if (!stateReady) return;
    let val = input.trim();
    if (!val && attachments.length === 0) return;
    if (attachments.length > 0) {
      val = `${val ? val + "\n\n" : ""}附件：\n${attachments.map((a) => `- ${a.path}`).join("\n")}`;
    }
    setInput("");
    setAttachments([]);
    try {
      await submitPromptText(val);
    } catch (err) {
      onToast(err instanceof Error ? err.message : "发送失败");
    }
  };

  const sendKey = (key: string) => {
    if (!stateReady) return Promise.resolve();
    return api(`/api/panes/${encodeURIComponent(pane.pane_id)}/keys`, { keys: [key] });
  };

  const answerApproval = async (choice: Approval["choices"][number], idx: number) => {
    if (!stateReady) return;
    const enc = encodeURIComponent(pane.pane_id);
    if (choice.keys && choice.keys.length > 0) {
      await api(`/api/panes/${enc}/keys`, { keys: choice.keys });
      return;
    }
    if (approval?.style === "cursor") {
      await api(`/api/panes/${enc}/keys`, { keys: computeCursorMoveKeys(approval, idx) });
      return;
    }
    await api(`/api/panes/${enc}/text`, { text: choice.key, enter: true });
  };

  const messages = [...olderMsgs, ...liveMsgs];
  const setupHint = mode === "chat" && messages.length === 0 && SETUP_RE.test(logText);
  const composerCtx = formatAgentComposerContext(pane, { mode, busy: liveStatus === "working" || effectiveQueue.length > 0 });

  return (
    <div
      className="flex-1 flex flex-col min-w-0 min-h-0 overflow-hidden"
      onDragOver={(e) => e.preventDefault()}
      onDrop={(e: DragEvent) => {
        e.preventDefault();
        if (e.dataTransfer?.files?.length) void addFiles(e.dataTransfer.files);
      }}
    >
      <AgentPaneHeader
        pane={pane}
        mode={mode}
        badge={composerCtx.badge}
        contextPct={contextPct}
        stateReady={stateReady}
        onBackToCorgi={onBackToCorgi}
        onSwitchMode={switchMode}
        onCloseWorkspace={() =>
          void api(`/api/workspaces/${encodeURIComponent(pane.workspace_id)}/close`, {}).then(() => {
            onToast("已关闭工作区");
            onClosed();
          })
        }
      />

      <div className="relative flex-1 flex flex-col min-h-0">
        <div ref={containerRef} onScroll={onScroll} className="flex-1 overflow-y-auto scroll-chat px-5 py-5 min-h-0">
          {mode === "screen" ? (
            <div className="h-full max-w-[960px] mx-auto">
              <ChunkErrorBoundary resetKey={`${pane.pane_id}:${mode}`}>
                <Suspense
                  fallback={
                    <div className="w-full h-full min-h-[360px] rounded-xl border border-hairline surface-rail flex items-center justify-center text-xs text-muted">
                      正在加载终端视图…
                    </div>
                  }
                >
                  <LazyTerminalScreen
                    frame={logText}
                    onKey={(k) => void sendKey(k)}
                    onText={(t) => void api(`/api/panes/${encodeURIComponent(pane.pane_id)}/text`, { text: t, enter: false })}
                    onToast={onToast}
                  />
                </Suspense>
              </ChunkErrorBoundary>
            </div>
          ) : mode === "chat" ? (
            <div ref={contentRef}>
              <AgentChatThread
                messages={messages}
                pendingPrompts={pendingPrompts}
                earlierCursor={earlierCursor}
                loadingEarlier={loadingEarlier}
                approval={approval}
                liveStatus={liveStatus}
                liveTail={liveTail}
                liveMeta={liveMeta}
                setupHint={setupHint}
                isAgent={pane.is_agent}
                agentName={pane.name}
                agentKind={pane.kind}
                agentMachine={pane.machine}
                onLoadEarlier={() => void loadEarlier()}
                onPickAskOption={(opt) => void submitPromptText(opt)}
                onAnswerApproval={(c, i) => void answerApproval(c, i)}
                onCancelApproval={() => void sendKey("esc")}
                onSwitchToScreen={() => switchMode("screen")}
                onOpenFile={onOpenFile}
              />
            </div>
          ) : (
            <div ref={contentRef}>
              <pre className="max-w-[900px] mx-auto text-xs font-mono-code leading-relaxed whitespace-pre-wrap text-secondary">
                {logText || "等待终端输出…"}
              </pre>
            </div>
          )}
        </div>
        {mode !== "screen" && showJumpBottom && (
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

      <AgentPaneComposer
        composerDockRef={composerDockRef}
        pane={pane}
        mode={mode}
        stateReady={stateReady}
        liveStatus={liveStatus}
        stopping={stopping}
        showKeys={showKeys}
        input={input}
        attachments={attachments}
        effectiveQueue={effectiveQueue}
        queuePaused={queuePaused}
        queueBusyId={queueBusyId}
        composerCtx={composerCtx}
        onInputChange={setInput}
        onRemoveAttachment={(idx) => setAttachments((prev) => prev.filter((_, i) => i !== idx))}
        onAddFiles={(files) => void addFiles(files)}
        onToggleKeys={() => setShowKeys((v) => !v)}
        onSendKey={(k) => void sendKey(k)}
        onSubmit={(e) => void sendInput(e)}
        onStop={() => void stopCurrentStep()}
        onSteerQueued={(it) => void steerQueued(it)}
        onCancelQueued={(it) => void cancelQueued(it)}
        onResumeQueue={() => void resumeQueue()}
        onBackToCorgi={onBackToCorgi}
        onToast={onToast}
      />
    </div>
  );
}

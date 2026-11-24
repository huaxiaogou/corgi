import { useEffect, useRef, useState, type FormEvent } from "react";
import { ArrowUp, Paperclip, Pin, RotateCw, Search, X } from "lucide-react";
import { CorgiButtMark, CorgiMark, StatusDot } from "../../components/CorgiMark";
import { DecisionCard } from "../../components/DecisionCard";
import { MarkdownText } from "../../components/MarkdownText";
import { api, uploadFile } from "../../lib/api";
import type { Entry, Machine, SuperView } from "../../types/api";

interface Props {
  data: SuperView | null;
  machines: Machine[];
  thread: string;
  onSelectWorker: (paneId: string) => void;
  onOpenFile: (path: string, machine?: string) => void;
  onToast: (msg: string) => void;
}

export function CorgiChatView({ data, machines, thread, onSelectWorker, onOpenFile, onToast }: Props) {
  const [text, setText] = useState("");
  const [machine, setMachine] = useState("");
  const [kind, setKind] = useState("");
  const [effort, setEffort] = useState("");
  const [replyTo, setReplyTo] = useState<Entry | null>(null);
  const [attachments, setAttachments] = useState<string[]>([]);
  const [searchOpen, setSearchOpen] = useState(false);
  const [searchQ, setSearchQ] = useState("");
  const [searchHits, setSearchHits] = useState<{ id: string; role: string; snippet: string }[]>([]);
  const [pinnedOpen, setPinnedOpen] = useState(false);
  const [olderEntries, setOlderEntries] = useState<Entry[]>([]);
  const [moreOlder, setMoreOlder] = useState<boolean | null>(null);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    setOlderEntries([]);
    setMoreOlder(null);
  }, [thread]);

  const liveEntries = data?.entries ?? [];
  const liveIds = new Set(liveEntries.map((e) => e.id));
  const entries = [...olderEntries.filter((e) => !liveIds.has(e.id)), ...liveEntries];
  const canLoadMore = moreOlder ?? Boolean(data?.earlier);
  const pinnedList = (data?.pinned ?? []).filter((e) => !thread || e.thread === thread);
  const pendingActions = (data?.actions ?? []).filter((a) => a.status === "pending" && (!thread || a.thread === thread));
  const activeWorkers = (data?.workers ?? []).filter((w) => !w.ended);

  const loadEarlier = async () => {
    if (loadingOlder || entries.length === 0) return;
    setLoadingOlder(true);
    try {
      const before = entries[0].seq;
      const res = await api<{ entries: Entry[]; more: boolean }>(
        `/api/super/earlier?thread=${encodeURIComponent(thread)}&before=${before}&limit=80`
      );
      setOlderEntries((prev) => [...(res.entries ?? []), ...prev]);
      setMoreOlder(Boolean(res.more));
    } catch (err) {
      onToast(err instanceof Error ? err.message : "加载历史失败");
    } finally {
      setLoadingOlder(false);
    }
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    let payload = text.trim();
    if (!payload && attachments.length === 0) return;
    if (attachments.length > 0) {
      payload = (payload ? payload + "\n\n" : "") + "附件：\n" + attachments.join("\n");
    }
    setText("");
    setAttachments([]);
    const reId = replyTo?.id ?? "";
    setReplyTo(null);
    try {
      await api("/api/super/say", {
        thread,
        text: payload,
        re: reId,
        machine,
        kind,
        effort,
      });
    } catch (err) {
      onToast(err instanceof Error ? err.message : "发送失败");
    }
  };

  const resolveAction = async (actionId: string, choiceKey: string) => {
    try {
      await api(`/api/super/actions/${encodeURIComponent(actionId)}/resolve`, { choice: choiceKey, by: "web" });
      onToast("已拍板并同步");
    } catch (err) {
      onToast(err instanceof Error ? err.message : "操作失败");
    }
  };

  const dismissAction = async (actionId: string) => {
    try {
      await api(`/api/super/actions/${encodeURIComponent(actionId)}/dismiss`, {});
      onToast("已忽略待办");
    } catch (err) {
      onToast(err instanceof Error ? err.message : "忽略失败");
    }
  };

  const togglePin = async (entry: Entry) => {
    try {
      await api("/api/super/pin", { id: entry.id, pinned: !entry.pinned });
    } catch (err) {
      onToast(err instanceof Error ? err.message : "置顶失败");
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

  const handleUpload = async (files: FileList | null) => {
    if (!files) return;
    for (const f of Array.from(files)) {
      try {
        const up = await uploadFile(f, machine);
        setAttachments((prev) => [...prev, up.path]);
      } catch (err) {
        onToast(err instanceof Error ? err.message : "上传失败");
      }
    }
  };

  const router = data?.router;
  const isWeComGroup = thread.startsWith("wecom:");
  const headerTitle = thread === "chat:main" ? "Corgi" : isWeComGroup ? `企微群 · ${thread.replace(/^wecom:/, "")}` : thread;

  return (
    <div className="flex-1 flex flex-col min-h-0">
      <header className="h-14 px-5 flex items-center justify-between border-b border-hairline shrink-0">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <span className="font-semibold text-[15px] truncate">{headerTitle}</span>
            {router?.kind && (
              <span className="text-[11px] font-mono-code px-1.5 py-0.5 rounded border border-strong text-secondary shrink-0">
                {router.kind}
              </span>
            )}
            {isWeComGroup && (
              <span className="text-[11px] px-1.5 py-0.5 rounded surface-rail border border-hairline text-muted shrink-0">
                双向同步
              </span>
            )}
          </div>
          <div className="flex items-center gap-2 text-[11.5px] text-muted mt-0.5 flex-wrap">
            <StatusDot status={router?.status ?? "asleep"} />
            <span>
              {router?.dreaming
                ? "正在整理记忆（做梦）…"
                : router?.handover
                  ? "正在交接记忆并切换会话…"
                  : router?.status === "working"
                    ? router.replied
                      ? "正在收尾记忆…"
                      : "正在跑腿…"
                    : router?.alive
                      ? "空闲"
                      : "打盹中"}
            </span>
            {router?.model && <span>· {router.model}</span>}
            {router && router.context ? (
              <span>
                · 上下文 {router.context}%{router.rotate_at > 0 ? `/${router.rotate_at}%` : ""}
              </span>
            ) : null}
            {data?.follow && (
              <span className="truncate max-w-[260px]" title={data.follow.next_text}>
                · 下次唤醒 {new Date(data.follow.next).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
              </span>
            )}
          </div>
        </div>
        <div className="flex items-center gap-1.5">
          <button
            type="button"
            onClick={() => setSearchOpen((v) => !v)}
            className="w-8 h-8 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
            title="搜索对话"
          >
            <Search className="w-4 h-4" strokeWidth={1.7} />
          </button>
          <button
            type="button"
            onClick={() => void api("/api/super/router/restart", {}).then(() => onToast("正在重启 Corgi 会话"))}
            className="w-8 h-8 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
            title="重启 Corgi"
          >
            <RotateCw className="w-4 h-4" strokeWidth={1.7} />
          </button>
        </div>
      </header>

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
          <button
            type="button"
            onClick={() => setPinnedOpen((v) => !v)}
            className="flex items-center gap-1.5 text-secondary font-medium cursor-pointer"
          >
            <Pin className="w-3.5 h-3.5" />
            <span>已置顶 {pinnedList.length} 条</span>
          </button>
          {pinnedOpen && (
            <div className="space-y-1 max-h-36 overflow-y-auto">
              {pinnedList.map((p) => (
                <div key={p.id} className="px-2.5 py-1.5 rounded-lg surface-bg border border-hairline flex items-center justify-between gap-2">
                  <span className="truncate text-secondary">{p.text}</span>
                  <button type="button" onClick={() => void togglePin(p)} className="text-muted hover:underline shrink-0 cursor-pointer">
                    取消置顶
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      <div className="flex-1 overflow-y-auto px-5 py-6">
        <div className="max-w-[740px] mx-auto space-y-5">
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
                  <div className="max-w-[80%] px-4 py-2.5 rounded-2xl rounded-br-sm text-[14px] leading-relaxed pill-user whitespace-pre-wrap">
                    {entry.text}
                  </div>
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
                  {entry.re && (
                    <div className="text-xs text-muted pl-2.5 border-l-2 border-strong truncate">
                      {entry.re.who}：{entry.re.text}
                    </div>
                  )}
                  <div className="text-[14.5px]">
                    <MarkdownText text={entry.text} onOpenFile={(p) => onOpenFile(p, machine)} />
                  </div>
                  <div className="flex items-center gap-3 text-[11px] text-muted opacity-0 group-hover:opacity-100 transition-opacity">
                    <button type="button" onClick={() => setReplyTo(entry)} className="hover:underline cursor-pointer">
                      引用回复
                    </button>
                    <button type="button" onClick={() => void togglePin(entry)} className="inline-flex items-center gap-1 hover:underline cursor-pointer">
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
                onResolve={resolveAction}
                onDismiss={dismissAction}
                onOpenWorker={(name) => {
                  const wk = data?.workers.find((w) => w.name === name);
                  if (wk?.pane_id) onSelectWorker(wk.pane_id);
                }}
              />
            </div>
          ))}
        </div>
      </div>

      <div className="px-5 pb-4 pt-2 shrink-0">
        <form onSubmit={(e) => void submit(e)} className="max-w-[740px] mx-auto composer-box rounded-2xl px-4 pt-3 pb-2.5 space-y-2">
          {replyTo && (
            <div className="flex items-center justify-between text-xs px-2.5 py-1 rounded-lg surface-rail text-secondary">
              <span className="truncate">引用 Corgi：{replyTo.text}</span>
              <button type="button" onClick={() => setReplyTo(null)} className="cursor-pointer ml-2">
                <X className="w-3.5 h-3.5" />
              </button>
            </div>
          )}
          {attachments.length > 0 && (
            <div className="flex flex-wrap gap-1.5 text-xs">
              {attachments.map((p) => (
                <span key={p} className="px-2 py-0.5 rounded surface-rail border border-hairline font-mono-code text-secondary">
                  {p.split("/").pop()}
                </span>
              ))}
            </div>
          )}
          <textarea
            rows={1}
            value={text}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                void submit(e);
              }
            }}
            placeholder="告诉 Corgi 你要做什么…"
            className="w-full resize-none bg-transparent text-[14px] outline-none"
          />
          <div className="flex items-center justify-between pt-1.5">
            <div className="flex items-center gap-2.5 text-xs text-muted flex-wrap">
              <button
                type="button"
                onClick={() => fileRef.current?.click()}
                className="text-secondary hover:opacity-80 cursor-pointer"
                title="上传图片或文件"
              >
                <Paperclip className="w-4 h-4" strokeWidth={1.7} />
              </button>
              <input ref={fileRef} type="file" multiple hidden onChange={(e) => void handleUpload(e.target.files)} />
              <span>派活</span>
              <select
                value={machine}
                onChange={(e) => setMachine(e.target.value)}
                className="bg-transparent text-secondary outline-none cursor-pointer"
              >
                <option value="">默认电脑</option>
                {machines.map((m) => (
                  <option key={m.id || "local"} value={m.id || "local"}>
                    {m.name}
                  </option>
                ))}
              </select>
              <select
                value={kind}
                onChange={(e) => setKind(e.target.value)}
                className="bg-transparent text-secondary outline-none cursor-pointer"
              >
                <option value="">自动 Agent</option>
                <option value="claude">claude</option>
                <option value="codex">codex</option>
                <option value="agy">agy</option>
                <option value="codebuddy">codebuddy</option>
              </select>
              <select
                value={effort}
                onChange={(e) => setEffort(e.target.value)}
                className="bg-transparent text-secondary outline-none cursor-pointer"
              >
                <option value="">默认思考</option>
                <option value="low">思考 · 低</option>
                <option value="medium">思考 · 中</option>
                <option value="high">思考 · 高</option>
                <option value="xhigh">思考 · 极高</option>
              </select>
            </div>
            <button
              type="submit"
              className="w-8 h-8 rounded-full btn-accent flex items-center justify-center cursor-pointer hover:opacity-85 shrink-0"
              title="发送"
            >
              <ArrowUp className="w-4 h-4" strokeWidth={2} />
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}

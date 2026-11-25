import { useEffect, useState } from "react";
import { ChevronDown, ChevronRight, Eye, History, RotateCcw, Trash2, X } from "lucide-react";
import { api } from "../lib/api";
import type { ChatMessage, EndedSession } from "../types/api";
import { MarkdownText } from "./MarkdownText";

interface Props {
  onSelectPane: (paneId: string) => void;
  onToast: (msg: string) => void;
}

export function HistorySection({ onSelectPane, onToast }: Props) {
  const [open, setOpen] = useState(false);
  const [history, setHistory] = useState<EndedSession[]>([]);
  const [resumingId, setResumingId] = useState("");
  const [preview, setPreview] = useState<{ sess: EndedSession; msgs: ChatMessage[] } | null>(null);

  const refresh = () => {
    void api<{ sessions: EndedSession[] }>("/api/history")
      .then((r) => setHistory(r.sessions ?? []))
      .catch(() => setHistory([]));
  };

  useEffect(() => {
    if (open) refresh();
  }, [open]);

  const resumeSession = async (sess: EndedSession) => {
    setResumingId(sess.id);
    try {
      const res = await api<{ pane_id: string }>(`/api/history/${encodeURIComponent(sess.id)}/resume`, {});
      setPreview(null);
      onToast(`已续接 ${sess.title || sess.name || sess.kind}`);
      onSelectPane(res.pane_id);
      refresh();
    } catch (err) {
      onToast(err instanceof Error ? err.message : "续接失败");
    } finally {
      setResumingId("");
    }
  };

  const openPreview = async (sess: EndedSession) => {
    try {
      const qs = new URLSearchParams({ kind: sess.kind, session: sess.session, cwd: sess.cwd });
      if (sess.machine) qs.set("machine", sess.machine);
      const res = await api<{ messages: ChatMessage[] }>(`/api/transcript?${qs.toString()}`);
      setPreview({ sess, msgs: res.messages ?? [] });
    } catch (err) {
      onToast(err instanceof Error ? err.message : "读取会话记录失败");
    }
  };

  const deleteItem = async (id: string) => {
    await api(`/api/history/${encodeURIComponent(id)}/delete`, {});
    setHistory((prev) => prev.filter((x) => x.id !== id));
  };

  return (
    <div className="pt-1">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="w-full px-2.5 py-1.5 flex items-center justify-between text-[11px] font-medium text-muted hover:text-secondary cursor-pointer"
      >
        <span className="inline-flex items-center gap-1.5">
          <History className="w-3.5 h-3.5" />
          <span>历史会话（可续接）</span>
        </span>
        {open ? <ChevronDown className="w-3.5 h-3.5" /> : <ChevronRight className="w-3.5 h-3.5" />}
      </button>

      {open && (
        <div className="space-y-1 pt-1">
          {history.length === 0 && <div className="px-2.5 py-1.5 text-[11.5px] text-muted">暂无归档会话</div>}
          {history.slice(0, 15).map((sess) => (
            <div key={sess.id} className="px-2.5 py-2 rounded-lg border border-hairline surface-bg flex items-center justify-between gap-1.5 text-xs">
              <button type="button" onClick={() => void openPreview(sess)} className="min-w-0 text-left flex-1 cursor-pointer">
                <div className="truncate font-medium text-primary hover:underline">{sess.title || sess.name || sess.kind}</div>
                <div className="text-[10.5px] font-mono-code text-muted truncate">
                  {sess.kind}
                  {sess.machine ? ` · ${sess.machine}` : ""}
                </div>
              </button>
              <button type="button" onClick={() => void openPreview(sess)} className="p-1 text-muted hover:text-primary cursor-pointer" title="只读查看记录">
                <Eye className="w-3.5 h-3.5" />
              </button>
              <button
                type="button"
                disabled={resumingId === sess.id}
                onClick={() => void resumeSession(sess)}
                className="px-2 py-1 rounded-md border border-strong text-[11px] text-secondary surface-hover cursor-pointer shrink-0 inline-flex items-center gap-1 disabled:opacity-50"
                title="用原会话 ID 续接"
              >
                <RotateCcw className="w-3 h-3" />
                续接
              </button>
              <button type="button" onClick={() => void deleteItem(sess.id)} className="p-1 text-muted hover:text-primary cursor-pointer" title="移除归档">
                <Trash2 className="w-3 h-3" />
              </button>
            </div>
          ))}
        </div>
      )}

      {preview && (
        <div className="fixed inset-0 z-50 flex items-center justify-center p-4" style={{ background: "rgba(0,0,0,0.4)" }} onClick={() => setPreview(null)}>
          <div onClick={(e) => e.stopPropagation()} className="w-full max-w-[680px] max-h-[82vh] flex flex-col rounded-2xl card-floating overflow-hidden">
            <div className="px-5 py-3.5 border-b border-hairline flex items-center justify-between gap-2">
              <div className="min-w-0">
                <div className="font-semibold text-sm truncate">{preview.sess.title || preview.sess.name || preview.sess.kind}</div>
                <div className="text-[11px] font-mono-code text-muted truncate">{preview.sess.cwd}</div>
              </div>
              <div className="flex items-center gap-2 shrink-0">
                <button type="button" onClick={() => void resumeSession(preview.sess)} className="px-3 py-1.5 rounded-lg text-xs btn-accent cursor-pointer">
                  在此续接会话
                </button>
                <button type="button" onClick={() => setPreview(null)} className="text-muted cursor-pointer">
                  <X className="w-4 h-4" />
                </button>
              </div>
            </div>
            <div className="flex-1 overflow-y-auto p-5 space-y-3 text-xs">
              {preview.msgs.length === 0 && <div className="text-muted text-center py-8">暂无可展示的对话消息</div>}
              {preview.msgs.map((m, i) =>
                m.kind === "user" ? (
                  <div key={i} className="flex justify-end">
                    <div className="max-w-[80%] px-3.5 py-2 rounded-2xl pill-user whitespace-pre-wrap">{m.text}</div>
                  </div>
                ) : m.kind === "assistant" ? (
                  <div key={i} className="text-[13.5px]">
                    <MarkdownText text={m.text ?? ""} />
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

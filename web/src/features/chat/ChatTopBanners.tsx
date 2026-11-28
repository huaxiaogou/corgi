import { Pin, X } from "lucide-react";
import { MarkdownText } from "../../components/MarkdownText";
import { api } from "../../lib/api";
import type { ChatMessage, EndedSession, Entry } from "../../types/api";

interface BannersProps {
  wecomFocusLabel: string;
  clearingWeComFocus: boolean;
  onClearWeComFocus: () => void;
  searchOpen: boolean;
  searchQ: string;
  searchHits: { id: string; role: string; snippet: string }[];
  onSearchChange: (q: string) => void;
  onCloseSearch: () => void;
  onJumpToEntry: (entryId: string) => void;
  pinnedList: Entry[];
  pinnedOpen: boolean;
  onTogglePinned: () => void;
}

export function ChatTopBanners({
  wecomFocusLabel,
  clearingWeComFocus,
  onClearWeComFocus,
  searchOpen,
  searchQ,
  searchHits,
  onSearchChange,
  onCloseSearch,
  onJumpToEntry,
  pinnedList,
  pinnedOpen,
  onTogglePinned,
}: BannersProps) {
  return (
    <>
      {wecomFocusLabel && (
        <div className="px-5 py-1.5 border-b border-hairline surface-rail shrink-0 flex items-center justify-between gap-3 text-xs">
          <div className="flex items-center gap-2 min-w-0">
            <span className="w-1.5 h-1.5 rounded-full shrink-0" style={{ background: "var(--attn)" }} />
            <span className="text-secondary truncate">{wecomFocusLabel}</span>
          </div>
          <button
            type="button"
            disabled={clearingWeComFocus}
            onClick={onClearWeComFocus}
            className="px-2 py-0.5 rounded-md border border-strong surface-bg surface-hover text-primary font-medium cursor-pointer shrink-0 disabled:opacity-50"
          >
            {clearingWeComFocus ? "切回中…" : "切回 Corgi"}
          </button>
        </div>
      )}

      {searchOpen && (
        <div className="px-5 py-2.5 border-b border-hairline surface-rail space-y-2">
          <div className="flex items-center gap-2">
            <input
              type="search"
              value={searchQ}
              onChange={(e) => onSearchChange(e.target.value)}
              placeholder="搜索历史对话（空格分隔多词）…"
              className="flex-1 px-3 py-1.5 rounded-lg text-xs surface-bg border border-hairline outline-none"
            />
            <button type="button" onClick={onCloseSearch} className="text-muted cursor-pointer">
              <X className="w-4 h-4" />
            </button>
          </div>
          {searchHits.length > 0 && (
            <div className="max-h-36 overflow-y-auto space-y-1 text-xs">
              {searchHits.map((h) => (
                <button
                  key={h.id}
                  type="button"
                  onClick={() => onJumpToEntry(h.id)}
                  className="w-full text-left px-2.5 py-1.5 rounded-lg surface-bg border border-hairline surface-hover text-secondary truncate cursor-pointer"
                >
                  <span className="font-mono-code text-muted mr-2">{h.role}</span>
                  {h.snippet}
                </button>
              ))}
            </div>
          )}
        </div>
      )}

      {pinnedList.length > 0 && (
        <div className="px-5 py-2 border-b border-hairline surface-rail text-xs shrink-0 space-y-1.5">
          <button type="button" onClick={onTogglePinned} className="flex items-center gap-1.5 text-secondary font-medium cursor-pointer">
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
    </>
  );
}

interface HistoryModalProps {
  preview: { sess: EndedSession; msgs: ChatMessage[] } | null;
  onClose: () => void;
  onSelectWorker: (paneId: string) => void;
  onOpenFile: (path: string, machine?: string) => void;
  onToast: (msg: string) => void;
}

export function ChatHistoryPreviewModal({ preview, onClose, onSelectWorker, onOpenFile, onToast }: HistoryModalProps) {
  if (!preview) return null;
  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      style={{ background: "rgba(0,0,0,0.4)" }}
      onClick={onClose}
    >
      <div
        onClick={(e) => e.stopPropagation()}
        className="w-full max-w-[680px] max-h-[82vh] flex flex-col rounded-2xl card-floating overflow-hidden"
      >
        <div className="px-5 py-3.5 border-b border-hairline flex items-center justify-between gap-2">
          <div className="min-w-0">
            <div className="font-semibold text-sm truncate">
              {preview.sess.title || preview.sess.name || preview.sess.kind}
            </div>
            <div className="text-[11px] font-mono-code text-muted truncate">{preview.sess.cwd}</div>
          </div>
          <div className="flex items-center gap-2 shrink-0">
            <button
              type="button"
              onClick={() => {
                const sess = preview.sess;
                void api<{ pane_id: string }>(`/api/history/${encodeURIComponent(sess.id)}/resume`, {})
                  .then((res) => {
                    onClose();
                    onToast(`已续接 ${sess.title || sess.name || sess.kind}`);
                    onSelectWorker(res.pane_id);
                  })
                  .catch((err) => onToast(err instanceof Error ? err.message : "续接失败"));
              }}
              className="px-3 py-1.5 rounded-lg text-xs btn-accent cursor-pointer"
            >
              在此续接会话
            </button>
            <button type="button" onClick={onClose} className="text-muted cursor-pointer">
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
                <MarkdownText text={m.text ?? ""} onOpenFile={onOpenFile} />
              </div>
            ) : null
          )}
        </div>
      </div>
    </div>
  );
}

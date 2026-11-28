import { useRef, type ClipboardEvent, type FormEvent, type RefObject } from "react";
import { ArrowLeft, ArrowUp, Keyboard, Paperclip, Square, X } from "lucide-react";
import { PromptQueueTray } from "../../components/PromptQueueTray";
import { createIMETracker, isPostCompositionSafariEnter } from "../../lib/composerInput";
import { capturePasteSnapshot, resolvePastedFiles } from "../../lib/pasteImage";
import type { AgentView, QueuedPrompt } from "../../types/api";

interface Props {
  composerDockRef: RefObject<HTMLDivElement | null>;
  pane: AgentView;
  mode: "chat" | "screen" | "log";
  stateReady: boolean;
  liveStatus: string;
  stopping: boolean;
  showKeys: boolean;
  input: string;
  attachments: { name: string; path: string }[];
  effectiveQueue: QueuedPrompt[];
  queuePaused: boolean;
  queueBusyId: string;
  composerCtx: { stripDesktop: string; stripMobile: string; backLabel: string; placeholder: string };
  onInputChange: (v: string) => void;
  onRemoveAttachment: (idx: number) => void;
  onAddFiles: (files: FileList | File[]) => void;
  onToggleKeys: () => void;
  onSendKey: (key: string) => void;
  onSubmit: (e: FormEvent) => void;
  onStop: () => void;
  onSteerQueued: (item: QueuedPrompt) => void;
  onCancelQueued: (item: QueuedPrompt) => void;
  onResumeQueue: () => void;
  onBackToCorgi: () => void;
  onToast: (msg: string) => void;
}

export function AgentPaneComposer({
  composerDockRef,
  pane,
  mode,
  stateReady,
  liveStatus,
  stopping,
  showKeys,
  input,
  attachments,
  effectiveQueue,
  queuePaused,
  queueBusyId,
  composerCtx,
  onInputChange,
  onRemoveAttachment,
  onAddFiles,
  onToggleKeys,
  onSendKey,
  onSubmit,
  onStop,
  onSteerQueued,
  onCancelQueued,
  onResumeQueue,
  onBackToCorgi,
  onToast,
}: Props) {
  const fileRef = useRef<HTMLInputElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const imeRef = useRef(createIMETracker());
  const keysVisible = mode !== "chat" || !pane.is_agent || showKeys;

  return (
    <div ref={composerDockRef} className="px-5 pt-2 shrink-0 space-y-2 kb-dock">
      {pane.is_agent && mode === "chat" && (
        <PromptQueueTray
          items={effectiveQueue}
          paused={queuePaused}
          busyId={queueBusyId}
          onSteer={onSteerQueued}
          onCancel={onCancelQueued}
          onResume={onResumeQueue}
        />
      )}
      {keysVisible && (
        <div className="max-w-[740px] mx-auto flex items-center gap-1.5 overflow-x-auto text-xs">
          {["esc", "ctrl+c", "tab", "up", "down", "enter"].map((k) => (
            <button
              key={k}
              type="button"
              onClick={() => onSendKey(k)}
              className="px-2.5 py-1 rounded-lg border border-hairline surface-rail text-secondary font-mono-code cursor-pointer shrink-0"
            >
              {k}
            </button>
          ))}
        </div>
      )}
      <div className="max-w-[740px] mx-auto">
        <div className="direct-strip rounded-t-2xl px-3.5 py-1.5 flex items-center justify-between gap-2 text-[11.5px]">
          <div className="flex items-center gap-1.5 min-w-0">
            <span className="w-1.5 h-1.5 rounded-full shrink-0" style={{ background: "var(--attn)" }} />
            <span className="hidden sm:inline truncate text-primary font-medium">{composerCtx.stripDesktop}</span>
            <span className="sm:hidden truncate text-primary font-medium">{composerCtx.stripMobile}</span>
          </div>
          <button
            type="button"
            onClick={onBackToCorgi}
            className="inline-flex items-center gap-1 text-[11.5px] text-secondary hover:text-primary font-medium cursor-pointer shrink-0"
          >
            <ArrowLeft className="w-3 h-3" />
            <span>{composerCtx.backLabel}</span>
          </button>
        </div>
        <form
          onSubmit={(e) => {
            if (imeRef.current.composing) {
              e.preventDefault();
              return;
            }
            onSubmit(e);
          }}
          className="composer-box-docked rounded-b-2xl px-3 py-2 space-y-1.5"
        >
          {attachments.length > 0 && (
            <div className="flex flex-wrap gap-1.5 px-1">
              {attachments.map((a, idx) => (
                <span key={idx} className="inline-flex items-center gap-1 text-[11px] px-2 py-0.5 rounded-md surface-rail border border-hairline text-secondary">
                  {a.name}
                  <button type="button" onClick={() => onRemoveAttachment(idx)} className="text-muted cursor-pointer">
                    <X className="w-3 h-3" />
                  </button>
                </span>
              ))}
            </div>
          )}
          <div className="flex items-center gap-2">
            <input
              ref={fileRef}
              type="file"
              multiple
              hidden
              onChange={(e) => {
                if (e.target.files) onAddFiles(e.target.files);
                e.target.value = "";
              }}
            />
            <button
              type="button"
              onClick={() => fileRef.current?.click()}
              title="添加图片或文件"
              className="w-8 h-8 rounded-lg flex items-center justify-center text-muted surface-hover cursor-pointer shrink-0"
            >
              <Paperclip className="w-4 h-4" />
            </button>
            {pane.is_agent && mode === "chat" && (
              <button
                type="button"
                onClick={onToggleKeys}
                title={showKeys ? "隐藏终端快捷键" : "显示终端快捷键"}
                className={`w-8 h-8 rounded-lg flex items-center justify-center surface-hover cursor-pointer shrink-0 ${
                  showKeys ? "text-primary surface-rail" : "text-muted"
                }`}
              >
                <Keyboard className="w-4 h-4" />
              </button>
            )}
            <input
              ref={inputRef}
              type="text"
              value={input}
              onCompositionStart={() => {
                imeRef.current.composing = true;
              }}
              onCompositionEnd={(e) => {
                imeRef.current.composing = false;
                imeRef.current.lastCompositionEndAt = e.timeStamp;
              }}
              onKeyDown={(e) => {
                if (
                  !e.nativeEvent.isComposing &&
                  isPostCompositionSafariEnter(
                    {
                      key: e.key,
                      shiftKey: e.shiftKey,
                      keyCode: e.keyCode,
                      isComposing: e.nativeEvent.isComposing,
                      timeStamp: e.timeStamp,
                    },
                    imeRef.current
                  )
                ) {
                  e.preventDefault();
                }
              }}
              onPaste={(e: ClipboardEvent<HTMLInputElement>) => {
                const snap = capturePasteSnapshot(e.clipboardData);
                if (!snap.hasMediaHint) return;
                e.preventDefault();
                void resolvePastedFiles(snap, navigator.clipboard).then((res) => {
                  if (res.files.length > 0) onAddFiles(res.files);
                  else if (res.error) onToast(res.error);
                });
              }}
              onChange={(e) => onInputChange(e.target.value)}
              placeholder={composerCtx.placeholder}
              className="flex-1 bg-transparent text-[14px] outline-none"
            />
            {pane.is_agent && mode === "chat" && liveStatus === "working" && (
              <button
                type="button"
                disabled={stopping}
                onClick={onStop}
                title="停止当前步骤 (Esc)"
                className="w-8 h-8 rounded-full border border-strong surface-rail text-primary flex items-center justify-center surface-hover cursor-pointer shrink-0 disabled:opacity-50"
              >
                <Square className="w-3 h-3 fill-current" />
              </button>
            )}
            <button
              type="submit"
              disabled={!stateReady}
              onPointerDown={(e) => {
                if (document.activeElement === inputRef.current) e.preventDefault();
              }}
              title={
                !stateReady
                  ? "正在同步实时状态…"
                  : pane.is_agent && mode === "chat" && (liveStatus === "working" || effectiveQueue.length > 0)
                    ? "加入待发送队列"
                    : "发送"
              }
              className="w-8 h-8 rounded-full btn-accent flex items-center justify-center cursor-pointer shrink-0 disabled:opacity-35 disabled:cursor-not-allowed"
            >
              <ArrowUp className="w-4 h-4" />
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}

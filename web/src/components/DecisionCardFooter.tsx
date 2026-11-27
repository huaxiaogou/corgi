import { useRef, useState, type FormEvent } from "react";
import { Clock, PenLine } from "lucide-react";
import { useAutoResizeTextarea } from "../hooks/useAutoResizeTextarea";
import { classifyEnterKey, createIMETracker } from "../lib/composerInput";

interface Props {
  busy: boolean;
  isWorkerDialog: boolean;
  isPatrolDigest: boolean;
  onSubmitCustom: (text: string) => Promise<void>;
  onCancelDialog: () => Promise<void>;
  onSnooze?: (minutes?: number) => Promise<void>;
  onDismiss?: () => Promise<void>;
}

export function DecisionCardFooter({
  busy,
  isWorkerDialog,
  isPatrolDigest,
  onSubmitCustom,
  onCancelDialog,
  onSnooze,
  onDismiss,
}: Props) {
  const [customText, setCustomText] = useState("");
  const [snoozeOpen, setSnoozeOpen] = useState(false);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const imeRef = useRef(createIMETracker());

  useAutoResizeTextarea(textareaRef, customText, { minHeight: 34, maxHeight: 120 });

  const submitCustom = (e?: FormEvent) => {
    e?.preventDefault();
    const trimmed = customText.trim();
    if (!trimmed) return;
    void onSubmitCustom(trimmed).then(() => setCustomText(""));
  };

  if (isWorkerDialog) {
    return (
      <div className="px-5 py-2.5">
        <button
          type="button"
          disabled={busy}
          onClick={() => void onCancelDialog()}
          className="px-2.5 py-1 rounded-lg text-xs border border-strong text-secondary surface-hover cursor-pointer disabled:opacity-50"
        >
          Esc 取消
        </button>
      </div>
    );
  }

  return (
    <>
      <form
        onSubmit={submitCustom}
        className="px-5 py-2.5 border-b border-hairline surface-rail flex flex-wrap sm:flex-nowrap items-start gap-2.5"
      >
        <span
          className="w-5 h-5 rounded flex items-center justify-center border border-dashed border-strong text-secondary surface-bg shrink-0 mt-1.5"
          title="自由输入其他想法"
        >
          <PenLine className="w-3 h-3" strokeWidth={1.8} />
        </span>
        <textarea
          ref={textareaRef}
          rows={1}
          value={customText}
          disabled={busy}
          onChange={(e) => setCustomText(e.target.value)}
          onCompositionStart={() => {
            imeRef.current.composing = true;
          }}
          onCompositionEnd={(e) => {
            imeRef.current.composing = false;
            imeRef.current.lastCompositionEndAt = e.timeStamp;
          }}
          onKeyDown={(e) => {
            const action = classifyEnterKey(
              {
                key: e.key,
                shiftKey: e.shiftKey,
                ctrlKey: e.ctrlKey,
                metaKey: e.metaKey,
                altKey: e.altKey,
                keyCode: e.keyCode,
                isComposing: e.nativeEvent.isComposing,
                timeStamp: e.timeStamp,
              },
              imeRef.current
            );
            if (action === "swallow_safari_post_ime") {
              e.preventDefault();
              return;
            }
            if (action === "submit") {
              e.preventDefault();
              submitCustom();
            }
          }}
          placeholder={
            isPatrolDigest
              ? "指定要处理的条目或例外…（如：先验收某项、只保留 X 其余归档）"
              : "输入其他想法或补充条件…（Enter 提交，Shift+Enter 换行）"
          }
          className="block flex-1 min-w-[180px] min-h-[34px] max-h-[120px] px-3 py-1.5 rounded-lg surface-bg border border-strong text-[13px] leading-snug text-primary outline-none resize-none overscroll-contain disabled:opacity-50"
        />
        <button
          type="submit"
          disabled={busy || !customText.trim()}
          className="w-full sm:w-auto px-3.5 py-1.5 min-h-[34px] rounded-lg btn-accent text-xs font-medium cursor-pointer shrink-0 disabled:opacity-35 disabled:cursor-not-allowed"
        >
          {isPatrolDigest ? "发送指令" : "提交想法"}
        </button>
      </form>

      {(onSnooze || onDismiss) && (
        <div className="px-5 py-2 flex flex-wrap items-center justify-between gap-2 text-[11.5px]">
          {onSnooze ? (
            snoozeOpen ? (
              <div className="flex flex-wrap items-center gap-1.5">
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void onSnooze(60)}
                  className="px-2 py-0.5 rounded-md border border-strong surface-bg surface-hover text-primary cursor-pointer disabled:opacity-50"
                >
                  1 小时后
                </button>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void onSnooze()}
                  className="px-2 py-0.5 rounded-md border border-strong surface-bg surface-hover text-secondary cursor-pointer disabled:opacity-50"
                >
                  明天 09:00
                </button>
                <button
                  type="button"
                  onClick={() => setSnoozeOpen(false)}
                  className="px-1.5 py-0.5 text-muted hover:text-primary cursor-pointer"
                >
                  取消
                </button>
              </div>
            ) : (
              <button
                type="button"
                disabled={busy}
                onClick={() => setSnoozeOpen(true)}
                className="inline-flex items-center gap-1.5 text-secondary hover:text-primary cursor-pointer disabled:opacity-50"
              >
                <Clock className="w-3.5 h-3.5" strokeWidth={1.7} />
                <span>稍后处理</span>
              </button>
            )
          ) : (
            <span />
          )}
          {onDismiss && (
            <button
              type="button"
              disabled={busy}
              onClick={() => void onDismiss()}
              className="text-muted hover:underline cursor-pointer disabled:opacity-50"
            >
              暂不处理（跳过）
            </button>
          )}
        </div>
      )}
    </>
  );
}

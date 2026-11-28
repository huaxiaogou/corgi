import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { flushSync } from "react-dom";
import { ArrowUp, X } from "lucide-react";
import { useAutoResizeTextarea } from "../hooks/useAutoResizeTextarea";
import { classifyEnterKey, createIMETracker } from "../lib/composerInput";

export type InlineReplyMode = "say" | "action";

interface Props {
  entryId: string;
  initialDraft?: string;
  disabled?: boolean;
  isDesktopOrTablet: boolean;
  hasInlineAction?: boolean;
  onDraftChange: (entryId: string, value: string) => void;
  onRegisterOpen?: (entryId: string, opener: ((mode: InlineReplyMode) => void) | null) => void;
  onMobileReply: (mode: InlineReplyMode) => void;
  onSubmit: (text: string, mode: InlineReplyMode) => Promise<void>;
}

function scrollBoxMinimallyIntoView(boxEl: HTMLElement | null) {
  if (!boxEl) return;
  const scroller = boxEl.closest(".scroll-chat");
  if (!scroller) return;
  const cRect = scroller.getBoundingClientRect();
  const bRect = boxEl.getBoundingClientRect();
  const margin = 18;
  if (bRect.bottom > cRect.bottom - margin) {
    scroller.scrollTop += bRect.bottom - (cRect.bottom - margin);
  } else if (bRect.top < cRect.top + margin) {
    scroller.scrollTop -= cRect.top + margin - bRect.top;
  }
}

export function InlineReplyBox({
  entryId,
  initialDraft = "",
  disabled = false,
  isDesktopOrTablet,
  hasInlineAction = false,
  onDraftChange,
  onRegisterOpen,
  onMobileReply,
  onSubmit,
}: Props) {
  const [openMode, setOpenMode] = useState<InlineReplyMode | null>(null);
  const [text, setText] = useState(initialDraft);
  const [submitting, setSubmitting] = useState(false);
  const boxRef = useRef<HTMLDivElement>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const imeRef = useRef(createIMETracker());
  const submittingRef = useRef(false);

  useAutoResizeTextarea(textareaRef, text, { minHeight: 34, maxHeight: 140 });

  const openEditor = useCallback(
    (mode: InlineReplyMode) => {
      if (!isDesktopOrTablet) {
        onMobileReply(mode);
        return;
      }
      // Local-only flushSync keeps focus inside the synchronous user gesture without re-rendering the whole list.
      flushSync(() => {
        setOpenMode(mode);
      });
      const ta = textareaRef.current;
      if (ta) {
        ta.focus({ preventScroll: true });
        ta.setSelectionRange(ta.value.length, ta.value.length);
      }
      scrollBoxMinimallyIntoView(boxRef.current);
    },
    [isDesktopOrTablet, onMobileReply]
  );

  useEffect(() => {
    onRegisterOpen?.(entryId, openEditor);
    return () => {
      onRegisterOpen?.(entryId, null);
    };
  }, [entryId, openEditor, onRegisterOpen]);

  const updateText = (next: string) => {
    setText(next);
    onDraftChange(entryId, next);
  };

  const handleSubmit = async (e?: FormEvent) => {
    e?.preventDefault();
    if (disabled || submittingRef.current || !openMode) return;
    const trimmed = text.trim();
    if (!trimmed) return;
    submittingRef.current = true;
    setSubmitting(true);
    try {
      await onSubmit(trimmed, openMode);
      updateText("");
      setOpenMode(null);
    } finally {
      submittingRef.current = false;
      setSubmitting(false);
    }
  };

  if (!openMode) return null;

  const isActionMode = openMode === "action" && hasInlineAction;

  return (
    <div
      ref={boxRef}
      data-inline-reply-box={entryId}
      className="mt-1.5 rounded-xl border border-strong surface-bg px-3 py-2 space-y-1.5"
    >
      <div className="flex items-center justify-between gap-2 text-[11px] text-muted">
        <span>{isActionMode ? "原位自定义拍板" : "原位补充此消息"}</span>
        <button
          type="button"
          onClick={() => setOpenMode(null)}
          className="text-muted hover:text-primary cursor-pointer"
          title="收起补充框（草稿会保留）"
          aria-label="收起补充框"
        >
          <X className="w-3.5 h-3.5" />
        </button>
      </div>
      <form onSubmit={(e) => void handleSubmit(e)} className="flex items-end gap-2">
        <textarea
          ref={textareaRef}
          rows={1}
          value={text}
          disabled={disabled || submitting}
          onChange={(e) => updateText(e.target.value)}
          onCompositionStart={() => {
            imeRef.current.composing = true;
          }}
          onCompositionEnd={(e) => {
            imeRef.current.composing = false;
            imeRef.current.lastCompositionEndAt = e.timeStamp;
          }}
          onKeyDown={(e) => {
            if (e.key === "Escape") {
              e.preventDefault();
              setOpenMode(null);
              return;
            }
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
              void handleSubmit();
            }
          }}
          placeholder={
            isActionMode
              ? "输入你的自定义拍板或补充条件…（Enter 提交，Esc 收起）"
              : "就这条消息原位补充…（Enter 提交，Shift+Enter 换行，Esc 收起）"
          }
          className="block flex-1 min-w-0 min-h-[34px] max-h-[140px] py-1 text-[13px] leading-snug bg-transparent text-primary outline-none resize-none overscroll-contain disabled:opacity-50"
        />
        <button
          type="submit"
          disabled={disabled || submitting || !text.trim()}
          onPointerDown={(e) => {
            if (document.activeElement === textareaRef.current) {
              e.preventDefault();
            }
          }}
          title={isActionMode ? "提交自定义拍板" : "发送原位补充"}
          className="w-7 h-7 rounded-full btn-accent flex items-center justify-center shrink-0 cursor-pointer disabled:opacity-30 disabled:cursor-not-allowed"
        >
          <ArrowUp className="w-3.5 h-3.5" strokeWidth={2} />
        </button>
      </form>
    </div>
  );
}

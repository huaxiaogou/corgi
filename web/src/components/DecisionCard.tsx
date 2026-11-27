import { useRef, useState, type FormEvent } from "react";
import { ArrowUpRight, Clock, PenLine } from "lucide-react";
import { useAutoResizeTextarea } from "../hooks/useAutoResizeTextarea";
import { classifyEnterKey, createIMETracker } from "../lib/composerInput";
import { normalizeEscapedNewlines } from "../lib/text";
import type { Action } from "../types/api";
import { MarkdownText } from "./MarkdownText";

interface Props {
  action: Action;
  workerNames?: string[];
  onResolve: (actionId: string, choiceKey: string, custom?: boolean) => Promise<void>;
  onDismiss?: (actionId: string) => Promise<void>;
  onSnooze?: (actionId: string) => Promise<void>;
  onOpenWorker?: (workerName: string) => void;
  onOpenFile?: (path: string) => void;
}

function collapseWhitespace(s: string): string {
  return s.replace(/\s+/g, " ").trim();
}

function isClippedTitleOfSummary(title: string, summary: string): boolean {
  const cleanSummary = collapseWhitespace(summary);
  if (!cleanSummary) return false;
  const cleanTitle = collapseWhitespace(title);
  if (!cleanTitle || cleanTitle === cleanSummary) return true;
  const prefix = cleanTitle.replace(/(?:\.\.\.|…)+$/, "").trim();
  return prefix.length > 0 && cleanSummary.startsWith(prefix);
}

export function DecisionCard({ action, workerNames, onResolve, onDismiss, onSnooze, onOpenWorker, onOpenFile }: Props) {
  const [busy, setBusy] = useState(false);
  const [customText, setCustomText] = useState("");
  const customTextareaRef = useRef<HTMLTextAreaElement>(null);
  const customImeRef = useRef(createIMETracker());

  useAutoResizeTextarea(customTextareaRef, customText, { minHeight: 34, maxHeight: 120 });

  const run = async (fn: () => Promise<void>) => {
    if (busy || action.status !== "pending") return;
    setBusy(true);
    try {
      await fn();
    } finally {
      setBusy(false);
    }
  };

  const pick = (choiceKey: string, custom = false) =>
    run(async () => {
      await onResolve(action.id, choiceKey, custom);
      if (custom) setCustomText("");
    });

  const submitCustom = (e?: FormEvent) => {
    e?.preventDefault();
    const trimmed = customText.trim();
    if (trimmed) void pick(trimmed, true);
  };

  const dismiss = () => (onDismiss ? run(() => onDismiss(action.id)) : Promise.resolve());
  const snooze = () => (onSnooze ? run(() => onSnooze(action.id)) : Promise.resolve());

  const choices = action.choices ?? [];
  const isRouterCheck = action.kind === "router_check";
  const isWorkerDialog = action.kind === "worker_dialog";
  const isPatrolDigest = action.kind === "patrol_digest";
  const showRecommended = choices.length > 1 && !isRouterCheck && !isPatrolDigest;
  const title = normalizeEscapedNewlines(action.title);
  const summary = normalizeEscapedNewlines(action.summary ?? "");
  const summaryCoversTitle = Boolean(summary) && isClippedTitleOfSummary(title, summary);

  const mentionedWorkers =
    isPatrolDigest && onOpenWorker && workerNames?.length
      ? Array.from(
          new Set(
            workerNames.filter((name) => {
              if (!name || name === action.target_worker) return false;
              return summary.includes(name) || title.includes(name);
            })
          )
        ).slice(0, 6)
      : [];

  const digestCategory = (action.category ?? "").trim().toLowerCase();
  const badgeLabel = isPatrolDigest
    ? digestCategory === "morning"
      ? "晨间待办简报"
      : digestCategory === "stale"
        ? "陈旧会话归档"
        : action.target_worker
          ? `待办提醒 · ${action.target_worker}`
          : "待办巡检提醒"
    : isRouterCheck
      ? action.target_worker
        ? `${action.target_worker} · 审阅提醒`
        : "审阅提醒"
      : action.target_worker
        ? `${action.target_worker} 需要你确认`
        : "需要你拍板";

  return (
    <div className="card-floating rounded-2xl overflow-hidden">
      <div className="px-5 pt-4 pb-3 space-y-2">
        <div className="flex items-center justify-between gap-2 text-xs">
          <div className="flex items-center gap-2 min-w-0">
            <span className="w-2 h-2 rounded-full shrink-0" style={{ background: "var(--attn)" }} />
            <span className="font-medium truncate">{badgeLabel}</span>
          </div>
          {action.target_worker && onOpenWorker && (
            <button
              type="button"
              onClick={() => onOpenWorker(action.target_worker!)}
              className="text-muted hover:underline cursor-pointer shrink-0"
            >
              打开 →
            </button>
          )}
        </div>

        {summaryCoversTitle ? (
          <div className="text-[14px] leading-relaxed text-primary">
            <MarkdownText text={summary} onOpenFile={onOpenFile} />
          </div>
        ) : (
          <>
            <div className="text-[15px] font-semibold leading-snug text-primary">{title}</div>
            {summary && (
              <div className="text-[13px] leading-relaxed text-secondary">
                <MarkdownText text={summary} onOpenFile={onOpenFile} />
              </div>
            )}
          </>
        )}
        {mentionedWorkers.length > 0 && onOpenWorker && (
          <div className="pt-1 flex flex-wrap items-center gap-1.5">
            {mentionedWorkers.map((name) => (
              <button
                key={name}
                type="button"
                onClick={() => onOpenWorker(name)}
                className="inline-flex items-center gap-1 px-2.5 py-0.5 rounded-full text-[11.5px] font-mono-code border border-hairline surface-rail surface-hover text-secondary hover:text-primary cursor-pointer"
              >
                <span>{name}</span>
                <ArrowUpRight className="w-3 h-3" />
              </button>
            ))}
          </div>
        )}
        {action.command && (
          <pre className="px-3.5 py-2.5 rounded-xl text-[12.5px] font-mono-code overflow-x-auto surface-rail border border-hairline">
            {action.command}
          </pre>
        )}
      </div>

      <div className="border-t border-hairline">
        {choices.map((ch, idx) => (
          <button
            key={`${ch.key}-${idx}`}
            type="button"
            disabled={busy}
            onClick={() => void pick(ch.key || ch.label)}
            className="w-full px-5 py-3 flex items-center gap-3 text-left text-[13.5px] border-b border-hairline surface-hover transition-colors cursor-pointer disabled:opacity-50"
          >
            <span className="w-5 h-5 rounded flex items-center justify-center font-mono-code text-[11px] border border-strong text-secondary shrink-0">
              {isRouterCheck && choices.length === 1 ? "✓" : ch.key || idx + 1}
            </span>
            <span className={idx === 0 && showRecommended ? "font-medium text-primary" : "text-primary"}>{ch.label}</span>
            {idx === 0 && showRecommended && <span className="ml-auto text-[11px] text-muted">推荐</span>}
          </button>
        ))}

        {!isWorkerDialog && (
          <form onSubmit={(e) => void submitCustom(e)} className="px-5 py-2.5 border-b border-hairline surface-rail flex flex-wrap sm:flex-nowrap items-start gap-2.5">
            <span className="w-5 h-5 rounded flex items-center justify-center border border-dashed border-strong text-secondary surface-bg shrink-0 mt-1.5" title="自由输入其他想法">
              <PenLine className="w-3 h-3" strokeWidth={1.8} />
            </span>
            <textarea
              ref={customTextareaRef}
              rows={1}
              value={customText}
              disabled={busy}
              onChange={(e) => setCustomText(e.target.value)}
              onCompositionStart={() => {
                customImeRef.current.composing = true;
              }}
              onCompositionEnd={(e) => {
                customImeRef.current.composing = false;
                customImeRef.current.lastCompositionEndAt = e.timeStamp;
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
                  customImeRef.current
                );
                if (action === "swallow_safari_post_ime") {
                  e.preventDefault();
                  return;
                }
                if (action === "submit") {
                  e.preventDefault();
                  void submitCustom();
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
        )}

        {isWorkerDialog ? (
          <div className="px-5 py-2.5">
            <button
              type="button"
              disabled={busy}
              onClick={() => void pick("esc")}
              className="px-2.5 py-1 rounded-lg text-xs border border-strong text-secondary surface-hover cursor-pointer"
            >
              Esc 取消
            </button>
          </div>
        ) : (
          (onSnooze || onDismiss) && (
            <div className="px-5 py-2 flex items-center justify-between gap-3">
              {onSnooze ? (
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void snooze()}
                  className="inline-flex items-center gap-1.5 text-[11.5px] text-secondary hover:text-primary cursor-pointer disabled:opacity-50"
                >
                  <Clock className="w-3.5 h-3.5" strokeWidth={1.7} />
                  <span>明天再提醒</span>
                </button>
              ) : (
                <span />
              )}
              {onDismiss && (
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void dismiss()}
                  className="text-[11.5px] text-muted hover:underline cursor-pointer disabled:opacity-50"
                >
                  暂不处理（忽略）
                </button>
              )}
            </div>
          )
        )}
      </div>
    </div>
  );
}

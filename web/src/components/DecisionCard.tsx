import { useState, type FormEvent } from "react";
import { Clock, PenLine } from "lucide-react";
import type { Action } from "../types/api";

interface Props {
  action: Action;
  onResolve: (actionId: string, choiceKey: string, custom?: boolean) => Promise<void>;
  onDismiss?: (actionId: string) => Promise<void>;
  onSnooze?: (actionId: string) => Promise<void>;
  onOpenWorker?: (workerName: string) => void;
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

export function DecisionCard({ action, onResolve, onDismiss, onSnooze, onOpenWorker }: Props) {
  const [busy, setBusy] = useState(false);
  const [customText, setCustomText] = useState("");

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
  const showRecommended = choices.length > 1 && !isRouterCheck;
  const summaryCoversTitle = Boolean(action.summary) && isClippedTitleOfSummary(action.title, action.summary ?? "");

  const badgeLabel =
    action.kind === "patrol_digest"
      ? action.target_worker
        ? `巡检简报 · ${action.target_worker}`
        : "空闲巡检 / 晨间简报"
      : isRouterCheck
        ? action.target_worker
          ? `${action.target_worker} 待你确认`
          : "待你确认"
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
          <p className="text-[14.5px] font-medium leading-relaxed text-primary whitespace-pre-line">{action.summary}</p>
        ) : (
          <>
            <div className="text-[15px] font-semibold leading-snug text-primary">{action.title}</div>
            {action.summary && <p className="text-[13px] leading-relaxed text-secondary whitespace-pre-line">{action.summary}</p>}
          </>
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
            <span
              className="w-5 h-5 rounded flex items-center justify-center border border-dashed border-strong text-secondary surface-bg shrink-0 mt-1.5"
              title="自由输入其他想法"
            >
              <PenLine className="w-3 h-3" strokeWidth={1.8} />
            </span>
            <textarea
              rows={1}
              value={customText}
              disabled={busy}
              onChange={(e) => setCustomText(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.shiftKey) {
                  e.preventDefault();
                  void submitCustom();
                }
              }}
              placeholder="输入其他想法或补充条件…（Enter 提交，Shift+Enter 换行）"
              className="flex-1 min-w-[180px] min-h-[34px] max-h-28 px-3 py-1.5 rounded-lg surface-bg border border-strong text-[13px] leading-snug text-primary outline-none resize-y disabled:opacity-50"
            />
            <button
              type="submit"
              disabled={busy || !customText.trim()}
              className="w-full sm:w-auto px-3.5 py-1.5 min-h-[34px] rounded-lg btn-accent text-xs font-medium cursor-pointer shrink-0 disabled:opacity-35 disabled:cursor-not-allowed"
            >
              提交想法
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

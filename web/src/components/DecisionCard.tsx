import { useState } from "react";
import { Clock } from "lucide-react";
import type { Action } from "../types/api";

interface Props {
  action: Action;
  onResolve: (actionId: string, choiceKey: string) => Promise<void>;
  onDismiss?: (actionId: string) => Promise<void>;
  onSnooze?: (actionId: string) => Promise<void>;
  onOpenWorker?: (workerName: string) => void;
}

export function DecisionCard({ action, onResolve, onDismiss, onSnooze, onOpenWorker }: Props) {
  const [busy, setBusy] = useState(false);

  const pick = async (choiceKey: string) => {
    if (busy || action.status !== "pending") return;
    setBusy(true);
    try {
      await onResolve(action.id, choiceKey);
    } finally {
      setBusy(false);
    }
  };

  const dismiss = async () => {
    if (!onDismiss || busy || action.status !== "pending") return;
    setBusy(true);
    try {
      await onDismiss(action.id);
    } finally {
      setBusy(false);
    }
  };

  const snooze = async () => {
    if (!onSnooze || busy || action.status !== "pending") return;
    setBusy(true);
    try {
      await onSnooze(action.id);
    } finally {
      setBusy(false);
    }
  };

  const badgeLabel =
    action.kind === "patrol_digest"
      ? action.target_worker
        ? `巡检简报 · ${action.target_worker}`
        : "空闲巡检 / 晨间简报"
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

        <div className="text-[15px] font-semibold leading-snug text-primary">{action.title}</div>
        {action.summary && action.summary !== action.title && (
          <p className="text-[13px] leading-relaxed text-secondary whitespace-pre-line">{action.summary}</p>
        )}
        {action.command && (
          <pre className="px-3.5 py-2.5 rounded-xl text-[12.5px] font-mono-code overflow-x-auto surface-rail border border-hairline">
            {action.command}
          </pre>
        )}
      </div>

      <div className="border-t border-hairline">
        {(action.choices ?? []).map((ch, idx) => (
          <button
            key={`${ch.key}-${idx}`}
            type="button"
            disabled={busy}
            onClick={() => void pick(ch.key || ch.label)}
            className="w-full px-5 py-3 flex items-center gap-3 text-left text-[13.5px] border-b border-hairline surface-hover transition-colors cursor-pointer disabled:opacity-50"
          >
            <span className="w-5 h-5 rounded flex items-center justify-center font-mono-code text-[11px] border border-strong text-secondary shrink-0">
              {ch.key || idx + 1}
            </span>
            <span className={idx === 0 ? "font-medium text-primary" : "text-primary"}>{ch.label}</span>
            {idx === 0 && <span className="ml-auto text-[11px] text-muted">推荐</span>}
          </button>
        ))}

        {action.kind === "worker_dialog" ? (
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

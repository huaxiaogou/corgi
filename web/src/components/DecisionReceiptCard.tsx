import { useState } from "react";
import { ArrowUpRight, Check, ChevronDown, ChevronUp, Clock, CornerDownRight, MessageSquarePlus, X } from "lucide-react";
import type { Action } from "../types/api";

interface Props {
  action: Action;
  at: string;
  originEntryId?: string;
  statusNote?: string;
  onJumpToEntry?: (entryId: string) => void;
  onFollowUp?: () => void;
  onOpenWorker?: (workerName: string) => void;
}

const CHANNEL_LABELS: Record<string, string> = { wecom: "企业微信", web: "Web", router: "Corgi", cli: "命令行" };

function formatTime(iso?: string, withDate = false): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  if (!withDate) return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function DecisionReceiptCard({
  action,
  at,
  originEntryId,
  statusNote,
  onJumpToEntry,
  onFollowUp,
  onOpenWorker,
}: Props) {
  const [altsOpen, setAltsOpen] = useState(false);
  const [qExpanded, setQExpanded] = useState(false);

  const question = (action.summary || action.title || "").trim();
  const originId = originEntryId || action.entry_id || "";
  const timeText = formatTime(action.resolved_at || at);

  if (action.status === "snoozed") {
    const until = formatTime(action.snooze_until, true);
    return (
      <div className="rounded-xl border border-hairline surface-rail px-3.5 py-2.5 flex flex-wrap items-center justify-between gap-2 text-xs">
        <div className="flex items-center gap-2 min-w-0 flex-1">
          <Clock className="w-3.5 h-3.5 text-muted shrink-0" strokeWidth={1.7} />
          <span className="font-medium text-secondary shrink-0">{until ? `已暂缓至 ${until}` : "已暂缓提醒"}</span>
          {question && <span className="text-muted truncate">· 原问题：{question}</span>}
        </div>
        {originId && onJumpToEntry && (
          <button type="button" onClick={() => onJumpToEntry(originId)} className="text-[11px] text-muted hover:text-primary hover:underline cursor-pointer shrink-0">
            定位原问题 ↑
          </button>
        )}
      </div>
    );
  }

  const isEsc = (action.chosen_key ?? "").toLowerCase() === "esc";
  const isDismissed = action.status === "expired" && action.resolved_by === "dismiss";
  const chosenLabel = action.chosen_label || action.chosen_key || (isDismissed ? "已忽略" : "已处理");
  const choices = action.choices ?? [];
  const chosenChoice = choices.find(
    (c) =>
      (action.chosen_key && c.key && c.key.toLowerCase() === action.chosen_key.toLowerCase()) ||
      (action.chosen_label && c.label === action.chosen_label)
  );
  const otherChoices = choices.filter((c) => c !== chosenChoice);
  const rawChannel = (action.resolved_by ?? "").trim();
  const channel = CHANNEL_LABELS[rawChannel.toLowerCase()] ?? rawChannel;
  const needsExpand = question.length > 72 || question.includes("\n");
  const statusLabel = isEsc ? "已取消" : isDismissed ? "已忽略" : "已拍板";
  const statusDotColor = isEsc || isDismissed ? "var(--fg-3)" : "var(--ok)";

  return (
    <div className="rounded-2xl border border-hairline surface-rail overflow-hidden">
      <div className="px-4 py-2 border-b border-hairline flex flex-wrap items-center justify-between gap-2 text-[11.5px]">
        <div className="flex items-center gap-1.5 min-w-0">
          <span className="w-2 h-2 rounded-full shrink-0" style={{ background: statusDotColor }} />
          <span className="font-medium text-primary">{statusLabel}</span>
          {channel && <span className="text-muted">· {channel}</span>}
          {timeText && <span className="font-mono-code text-muted">· {timeText}</span>}
        </div>
        <div className="flex items-center gap-3 shrink-0">
          {originId && onJumpToEntry && (
            <button type="button" onClick={() => onJumpToEntry(originId)} className="text-[11px] text-muted hover:text-primary hover:underline cursor-pointer">
              定位原问题 ↑
            </button>
          )}
          {action.target_worker && onOpenWorker && (
            <button
              type="button"
              onClick={() => onOpenWorker(action.target_worker!)}
              className="inline-flex items-center gap-0.5 text-[11px] text-muted hover:text-primary hover:underline cursor-pointer"
            >
              <span>{action.target_worker}</span>
              <ArrowUpRight className="w-3 h-3" />
            </button>
          )}
        </div>
      </div>

      <div className="px-4 py-3 surface-bg space-y-2.5">
        {question && (
          <div className="border-l-2 border-strong pl-2.5 space-y-1">
            <div className="text-[11px] text-muted">原问题 · {action.target_worker || "Corgi"}</div>
            <p className={`text-[12.5px] leading-relaxed text-secondary whitespace-pre-line ${!qExpanded && needsExpand ? "line-clamp-2" : ""}`}>
              {question}
            </p>
            {needsExpand && (
              <button type="button" onClick={() => setQExpanded((v) => !v)} className="text-[11px] text-muted hover:text-primary hover:underline cursor-pointer">
                {qExpanded ? "收起问题" : "展开全文"}
              </button>
            )}
            {action.command && (
              <pre className="mt-1.5 px-2.5 py-1.5 rounded-lg border border-hairline surface-rail font-mono-code text-[11.5px] text-secondary overflow-x-auto">
                {action.command}
              </pre>
            )}
          </div>
        )}

        <div className="rounded-xl border border-strong surface-rail px-3 py-2.5 flex items-start gap-2.5">
          <span className="w-5 h-5 rounded-md btn-accent flex items-center justify-center shrink-0 mt-0.5">
            {isEsc || isDismissed ? <X className="w-3 h-3" strokeWidth={2.4} /> : <Check className="w-3 h-3" strokeWidth={2.4} />}
          </span>
          <div className="min-w-0 flex-1">
            <div className="text-[11px] text-muted">{isEsc ? "操作结果" : "你的选择"}</div>
            <div className="text-[13.5px] font-semibold text-primary leading-snug break-words">{chosenLabel}</div>
            {chosenChoice?.detail && <div className="mt-0.5 text-xs text-secondary leading-relaxed">{chosenChoice.detail}</div>}
          </div>
        </div>

        {otherChoices.length > 0 && (
          <div className="pt-1 border-t border-hairline">
            <button
              type="button"
              onClick={() => setAltsOpen((v) => !v)}
              className="inline-flex items-center gap-1 text-[11.5px] text-muted hover:text-primary cursor-pointer"
            >
              <span>{altsOpen ? `收起其余 ${otherChoices.length} 个未选备选` : `查看其余 ${otherChoices.length} 个备选`}</span>
              {altsOpen ? <ChevronUp className="w-3.5 h-3.5" /> : <ChevronDown className="w-3.5 h-3.5" />}
            </button>
            {altsOpen && (
              <div className="mt-2 space-y-1">
                {otherChoices.map((ch, idx) => (
                  <div key={`${ch.key}-${idx}`} className="px-2.5 py-1.5 rounded-lg surface-rail flex items-baseline gap-2 text-xs text-muted">
                    <span className="font-mono-code text-[10.5px] shrink-0">{ch.key || idx + 1}</span>
                    <span className="break-words">{ch.label}</span>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
      </div>

      {(statusNote || onFollowUp) && (
        <div className="px-4 py-2 surface-rail border-t border-hairline flex flex-wrap items-center justify-between gap-2 text-[11.5px]">
          <span className="inline-flex items-center gap-1.5 text-secondary min-w-0">
            {statusNote && (
              <>
                <CornerDownRight className="w-3.5 h-3.5 text-muted shrink-0" />
                <span className="truncate">{statusNote}</span>
              </>
            )}
          </span>
          {onFollowUp && (
            <button
              type="button"
              onClick={onFollowUp}
              className="inline-flex items-center gap-1 font-medium text-secondary hover:text-primary hover:underline cursor-pointer shrink-0"
            >
              <MessageSquarePlus className="w-3.5 h-3.5" />
              <span>补充说明 / 追问</span>
            </button>
          )}
        </div>
      )}
    </div>
  );
}

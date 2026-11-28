import { useEffect, useState } from "react";
import { ArrowUpRight, Check, ChevronDown, ChevronLeft, ChevronRight, ChevronUp } from "lucide-react";
import {
  choiceKeyOf,
  computeDecisionFold,
  formatDecisionConfirmLabel,
  isFastSingleAction,
  isMultiAction,
  toggleChoiceSelection,
} from "../lib/decisionCard";
import { normalizeEscapedNewlines } from "../lib/text";
import type { Action } from "../types/api";
import { DecisionCardFooter } from "./DecisionCardFooter";
import { MarkdownText } from "./MarkdownText";

export interface DecisionResolvePayload {
  choice?: string;
  choices?: string[];
  custom?: boolean;
  inPlace?: boolean;
}

interface Props {
  action: Action;
  disabled?: boolean;
  workerNames?: string[];
  queuePos?: number;
  queueTotal?: number;
  onSwitchQueue?: (delta: number) => void;
  onResolve: (actionId: string, payload: DecisionResolvePayload) => Promise<void>;
  onDismiss?: (actionId: string) => Promise<void>;
  onSnooze?: (actionId: string, minutes?: number) => Promise<void>;
  onOpenWorker?: (workerName: string) => void;
  onOpenFile?: (path: string) => void;
}

function isClippedTitleOfSummary(title: string, summary: string): boolean {
  const cleanSummary = summary.replace(/\s+/g, " ").trim();
  if (!cleanSummary) return false;
  const cleanTitle = title.replace(/\s+/g, " ").trim();
  if (!cleanTitle || cleanTitle === cleanSummary) return true;
  const prefix = cleanTitle.replace(/(?:\.\.\.|…)+$/, "").trim();
  return prefix.length > 0 && cleanSummary.startsWith(prefix);
}

export function DecisionCard({
  action,
  disabled = false,
  workerNames,
  queuePos,
  queueTotal,
  onSwitchQueue,
  onResolve,
  onDismiss,
  onSnooze,
  onOpenWorker,
  onOpenFile,
}: Props) {
  const [busy, setBusy] = useState(false);
  const [selected, setSelected] = useState<string[]>([]);
  const [expanded, setExpanded] = useState(false);
  const locked = busy || disabled;

  useEffect(() => {
    setSelected([]);
    setExpanded(false);
  }, [action.id]);

  const run = async (fn: () => Promise<void>) => {
    if (locked || action.status !== "pending") return;
    setBusy(true);
    try {
      await fn();
    } finally {
      setBusy(false);
    }
  };

  const choices = action.choices ?? [];
  const multi = isMultiAction(action);
  const fastSingle = isFastSingleAction(action);
  const isRouterCheck = action.kind === "router_check";
  const isWorkerDialog = action.kind === "worker_dialog";
  const isPatrolDigest = action.kind === "patrol_digest";
  const hasExplicitRec = choices.some((c) => c.recommended);
  const showFirstRec = !hasExplicitRec && choices.length > 1 && !isRouterCheck && !isPatrolDigest && !multi;
  const title = normalizeEscapedNewlines(action.title);
  const summary = normalizeEscapedNewlines(action.summary ?? "");
  const summaryCoversTitle = Boolean(summary) && isClippedTitleOfSummary(title, summary);
  const fold = computeDecisionFold(choices, expanded, selected);

  const mentionedWorkers =
    isPatrolDigest && onOpenWorker && workerNames?.length
      ? Array.from(
          new Set(workerNames.filter((n) => n && n !== action.target_worker && (summary.includes(n) || title.includes(n))))
        ).slice(0, 6)
      : [];

  const modeHint = fastSingle ? "" : multi ? `多选（${choices.length}项）· 勾选后提交` : `单选（${choices.length}项）· 选定后确认`;
  const badgeLabel = isPatrolDigest
    ? action.category === "morning"
      ? "晨间待办简报"
      : action.category === "stale"
        ? "陈旧会话归档"
        : action.target_worker
          ? `待办提醒 · ${action.target_worker}`
          : "待办巡检提醒"
    : action.target_worker
      ? `${action.target_worker} 需要你确认`
      : "需要你拍板";

  const onChoiceClick = (key: string) => {
    if (locked) return;
    if (fastSingle) {
      void run(() => onResolve(action.id, { choice: key }));
      return;
    }
    setSelected((prev) => toggleChoiceSelection(multi, prev, key));
  };

  const submitSelected = () => {
    if (locked || selected.length === 0) return;
    void run(() => onResolve(action.id, multi ? { choices: selected } : { choice: selected[0] }));
  };

  return (
    <div className="card-floating rounded-2xl overflow-hidden">
      <div className="px-5 pt-4 pb-3 space-y-2">
        <div className="flex items-center justify-between gap-2 text-xs">
          <div className="flex items-center gap-2 min-w-0">
            <span className="w-2 h-2 rounded-full shrink-0" style={{ background: "var(--attn)" }} />
            <span className="font-medium truncate">{badgeLabel}</span>
            {queueTotal && queueTotal > 1 ? (
              <span className="px-1.5 py-0.5 rounded-full border border-hairline surface-rail font-mono-code text-[10.5px] text-secondary shrink-0">
                {queuePos ?? 1}/{queueTotal}
              </span>
            ) : null}
            {modeHint && <span className="text-[11px] text-muted truncate hidden sm:inline">{modeHint}</span>}
          </div>
          <div className="flex items-center gap-2 shrink-0">
            {queueTotal && queueTotal > 1 && onSwitchQueue && (
              <div className="inline-flex items-center border border-hairline rounded-md overflow-hidden surface-bg">
                <button
                  type="button"
                  onClick={() => onSwitchQueue(-1)}
                  title="上一项待拍板"
                  className="px-1 py-0.5 text-secondary hover:text-primary surface-hover cursor-pointer"
                >
                  <ChevronLeft className="w-3.5 h-3.5" />
                </button>
                <button
                  type="button"
                  onClick={() => onSwitchQueue(1)}
                  title="下一项待拍板"
                  className="px-1 py-0.5 border-l border-hairline text-secondary hover:text-primary surface-hover cursor-pointer"
                >
                  <ChevronRight className="w-3.5 h-3.5" />
                </button>
              </div>
            )}
            {action.target_worker && onOpenWorker && (
              <button type="button" onClick={() => onOpenWorker(action.target_worker!)} className="text-muted hover:underline cursor-pointer">
                打开 →
              </button>
            )}
          </div>
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
        {multi && choices.length > 1 && (
          <div className="px-5 py-1.5 border-b border-hairline surface-rail flex items-center justify-between gap-2 text-[11.5px]">
            <span className="text-muted">已选 {selected.length}/{choices.length} 项</span>
            <div className="flex items-center gap-2">
              {hasExplicitRec && (
                <button
                  type="button"
                  disabled={locked}
                  onClick={() => setSelected(choices.map((c, i) => (c.recommended ? choiceKeyOf(c, i) : "")).filter(Boolean))}
                  className="text-secondary hover:text-primary hover:underline cursor-pointer disabled:opacity-40"
                >
                  勾选推荐
                </button>
              )}
              <button
                type="button"
                disabled={locked}
                onClick={() => setSelected(selected.length === choices.length ? [] : choices.map((c, i) => choiceKeyOf(c, i)))}
                className="text-secondary hover:text-primary hover:underline cursor-pointer disabled:opacity-40"
              >
                {selected.length === choices.length ? "清空" : "全选"}
              </button>
            </div>
          </div>
        )}

        {fold.visible.map(({ choice: ch, index: idx, key }) => {
          const isSelected = selected.includes(key);
          const isRec = Boolean(ch.recommended) || (idx === 0 && showFirstRec);
          return (
            <button
              key={`${key}-${idx}`}
              type="button"
              disabled={locked}
              aria-pressed={!fastSingle ? isSelected : undefined}
              onClick={() => onChoiceClick(key)}
              className={`w-full px-5 py-3 flex items-start gap-3 text-left text-[13.5px] border-b border-hairline transition-colors cursor-pointer disabled:opacity-50 ${
                isSelected ? "surface-rail" : "surface-hover"
              }`}
            >
              {fastSingle ? (
                <span className="w-5 h-5 mt-0.5 rounded flex items-center justify-center font-mono-code text-[11px] border border-strong text-secondary shrink-0">
                  {isRouterCheck && choices.length === 1 ? "✓" : ch.key || idx + 1}
                </span>
              ) : (
                <span
                  className={`w-4 h-4 mt-1 flex items-center justify-center border shrink-0 ${multi ? "rounded" : "rounded-full"} ${
                    isSelected ? "btn-accent border-transparent" : "border-strong surface-bg"
                  }`}
                >
                  {isSelected && <Check className="w-3 h-3" strokeWidth={2.6} />}
                </span>
              )}
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2">
                  <span className={isSelected || (idx === 0 && showFirstRec) ? "font-medium text-primary" : "text-primary"}>{ch.label}</span>
                  {isRec && <span className="ml-auto text-[11px] text-muted shrink-0">推荐</span>}
                </div>
                {ch.detail && <div className="mt-0.5 text-xs text-secondary leading-relaxed">{ch.detail}</div>}
              </div>
            </button>
          );
        })}

        {choices.length > 4 && (
          <button
            type="button"
            disabled={busy}
            onClick={() => setExpanded((v) => !v)}
            className="w-full px-5 py-2 border-b border-hairline surface-rail surface-hover flex items-center justify-center gap-1 text-xs text-secondary hover:text-primary cursor-pointer"
          >
            {fold.folded ? (
              <>
                <span>
                  + 展开其余 {fold.hiddenCount} 个选项
                  {fold.hiddenSelectedCount > 0 ? `（含 ${fold.hiddenSelectedCount} 项已选）` : ""}
                </span>
                <ChevronDown className="w-3.5 h-3.5" />
              </>
            ) : (
              <>
                <span>收起至前 4 项</span>
                <ChevronUp className="w-3.5 h-3.5" />
              </>
            )}
          </button>
        )}

        {!fastSingle && (
          <div className="px-5 py-2.5 border-b border-hairline flex items-center justify-between gap-3">
            <span className="text-[11.5px] text-muted">{multi ? "勾选所需项后点击右侧提交" : "先选定一项，再确认拍板"}</span>
            <button
              type="button"
              disabled={locked || selected.length === 0}
              onClick={submitSelected}
              className="px-4 py-1.5 rounded-lg btn-accent text-xs font-medium cursor-pointer shrink-0 disabled:opacity-35 disabled:cursor-not-allowed"
            >
              {formatDecisionConfirmLabel(action, selected)}
            </button>
          </div>
        )}

        <DecisionCardFooter
          actionId={action.id}
          busy={locked}
          isWorkerDialog={isWorkerDialog}
          isPatrolDigest={isPatrolDigest}
          onSubmitCustom={(text) => run(() => onResolve(action.id, { choice: text, custom: true }))}
          onCancelDialog={() => run(() => onResolve(action.id, { choice: "esc" }))}
          onSnooze={onSnooze ? (mins) => run(() => onSnooze(action.id, mins)) : undefined}
          onDismiss={onDismiss ? () => run(() => onDismiss(action.id)) : undefined}
        />
      </div>
    </div>
  );
}

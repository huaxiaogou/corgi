import { Check, Pin } from "lucide-react";
import { CorgiMark, StatusDot } from "../../components/CorgiMark";
import { DecisionReceiptCard } from "../../components/DecisionReceiptCard";
import { InPlaceReplies } from "../../components/InPlaceReplies";
import { InlineReplyBox, type InlineReplyMode } from "../../components/InlineReplyBox";
import { MarkdownText, type MarkdownImagePayload } from "../../components/MarkdownText";
import { QuoteBlock } from "../../components/QuoteBlock";
import { api } from "../../lib/api";
import { workerSpawnStatusBadge } from "../../lib/chatFilter";
import type { GalleryImageItem } from "../../lib/imageGallery";
import { hasDraftText } from "../../lib/inPlaceReply";
import type { Action, Entry, EntryWorkerRef, FileDrawerTarget, Machine, Worker } from "../../types/api";
import { ChatEntryArtifacts } from "./ChatEntryArtifacts";

interface Props {
  entry: Entry;
  idx: number;
  timelineEntries: Entry[];
  highlightedEntryId: string;
  resolvedActions: Record<string, Action>;
  entryActionById: Map<string, string>;
  hideQuote: boolean;
  inlineAction?: Action;
  inlineBusy: boolean;
  inPlaceReplies: Entry[];
  draftsByEntryId: Record<string, string>;
  superReady: boolean;
  isTabletOrDesktop: boolean;
  routerWorking: boolean;
  workers: Worker[] | undefined;
  machines: Machine[];
  resumingWorkerKey: string;
  onJumpToEntry: (entryId: string) => void;
  onOpenWorker: (name: string) => void;
  onSelectWorker: (paneId: string) => void;
  onOpenWorkerRecord: (ref: EntryWorkerRef) => void;
  onResumeWorker: (ref: EntryWorkerRef) => void;
  onOpenHistoryById: (histRef: string) => void;
  onOpenFile: (path: string, machine?: string) => void;
  onOpenArtifact: (target: FileDrawerTarget) => void;
  onOpenLightbox: (items: GalleryImageItem[], index: number) => void;
  onResolveInlineAction: (act: Action, choice: string, toastText: string) => void;
  onDismissInlineAction: (act: Action) => void;
  onReplyQuote: (entry: Entry) => void;
  onOpenInlineOrMobile: (entry: Entry, mode: InlineReplyMode, inlineAction?: Action) => void;
  onMobileReply: (entry: Entry, mode: InlineReplyMode, inlineAction?: Action) => void;
  onSubmitInlineReply: (entry: Entry, replyText: string, mode: InlineReplyMode, inlineAction?: Action) => Promise<void>;
  onDraftChange: (entryId: string, value: string) => void;
  onRegisterInlineOpen: (entryId: string, opener: ((mode: InlineReplyMode) => void) | null) => void;
  onToast: (msg: string) => void;
}

export function ChatEntryRow({
  entry,
  idx,
  timelineEntries,
  highlightedEntryId,
  resolvedActions,
  entryActionById,
  hideQuote,
  inlineAction,
  inlineBusy,
  inPlaceReplies,
  draftsByEntryId,
  superReady,
  isTabletOrDesktop,
  routerWorking,
  workers,
  machines,
  resumingWorkerKey,
  onJumpToEntry,
  onOpenWorker,
  onSelectWorker,
  onOpenWorkerRecord,
  onResumeWorker,
  onOpenHistoryById,
  onOpenFile,
  onOpenArtifact,
  onOpenLightbox,
  onResolveInlineAction,
  onDismissInlineAction,
  onReplyQuote,
  onOpenInlineOrMobile,
  onMobileReply,
  onSubmitInlineReply,
  onDraftChange,
  onRegisterInlineOpen,
  onToast,
}: Props) {
  const hlClass = highlightedEntryId === entry.id ? "entry-highlight px-2 py-1.5 -mx-2 transition-colors" : "transition-colors";
  const timeStr = new Date(entry.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

  if (entry.role === "event") {
    const eventAction = entry.action_id ? resolvedActions[entry.action_id] : undefined;
    if (eventAction && (eventAction.status === "resolved" || eventAction.status === "snoozed")) {
      const statusNote =
        eventAction.kind === "worker_dialog"
          ? eventAction.chosen_key?.toLowerCase() === "esc"
            ? "已向终端发送取消键 (Esc)"
            : `已将确认发送至 ${eventAction.target_worker || "终端"}`
          : undefined;
      return (
        <div id={`entry-${entry.id}`} data-entry-id={entry.id} className={`pl-0 sm:pl-10 ${hlClass}`}>
          <DecisionReceiptCard
            action={eventAction}
            at={entry.at}
            originEntryId={eventAction.entry_id}
            statusNote={statusNote}
            onJumpToEntry={onJumpToEntry}
            onOpenWorker={onOpenWorker}
          />
        </div>
      );
    }
    const histRef = entry.ref?.startsWith("h:") ? entry.ref.slice(2) : "";
    if (histRef) {
      return (
        <div id={`entry-${entry.id}`} data-entry-id={entry.id} className={`flex items-center gap-2.5 text-xs text-muted ${hlClass}`}>
          <StatusDot status={entry.tone === "error" ? "blocked" : "idle"} />
          <span className="font-mono-code text-[11px]">{timeStr}</span>
          <button
            type="button"
            onClick={() => onOpenHistoryById(histRef)}
            className="inline-flex items-center gap-1.5 text-left hover:text-primary cursor-pointer group"
          >
            <span className="group-hover:underline">{entry.text}</span>
            <span className="px-1.5 py-0.5 rounded border border-hairline surface-rail text-[10.5px] text-secondary shrink-0">
              查看旧会话 →
            </span>
          </button>
        </div>
      );
    }
    if (entry.tone === "spawn" && entry.ref) {
      const wk = workers?.find((w) => w.name === entry.ref);
      const badge = workerSpawnStatusBadge(wk);
      return (
        <div id={`entry-${entry.id}`} data-entry-id={entry.id} className={`flex items-center gap-2.5 text-xs text-muted ${hlClass}`}>
          <StatusDot status={badge.dot} />
          <span className="font-mono-code text-[11px]">{timeStr}</span>
          <button
            type="button"
            onClick={() => onOpenWorker(entry.ref!)}
            className="inline-flex flex-wrap items-center gap-1.5 text-left hover:text-primary cursor-pointer group"
          >
            <span className="group-hover:underline">{entry.text}</span>
            <span className="px-1.5 py-0.5 rounded-full border border-hairline surface-rail text-[10.5px] text-secondary shrink-0">
              {badge.label}
            </span>
          </button>
        </div>
      );
    }
    return (
      <div id={`entry-${entry.id}`} data-entry-id={entry.id} className={`flex items-center gap-2.5 text-xs text-muted ${hlClass}`}>
        <StatusDot status={entry.tone === "blocked" || entry.tone === "error" ? "blocked" : entry.tone === "done" ? "done" : "idle"} />
        <span className="font-mono-code text-[11px]">{timeStr}</span>
        <span>{entry.text}</span>
      </div>
    );
  }

  if (entry.role === "user") {
    const directAct = entry.action_id ? resolvedActions[entry.action_id] : undefined;
    const legacyActId = !directAct && entry.re?.entry ? entryActionById.get(entry.re.entry) : undefined;
    const legacyAct = legacyActId ? resolvedActions[legacyActId] : undefined;
    const receiptAction =
      directAct?.status === "resolved"
        ? directAct
        : legacyAct?.status === "resolved" && legacyAct.chosen_label === entry.text
          ? legacyAct
          : undefined;
    if (receiptAction) {
      const hasFollowingReply = timelineEntries.slice(idx + 1, idx + 5).some((n) => n.role === "corgi");
      const isLatestUserEntry = idx === timelineEntries.length - 1;
      const statusNote = hasFollowingReply ? "Corgi 已响应" : isLatestUserEntry && routerWorking ? "Corgi 正在处理…" : "已同步至对话";
      return (
        <div id={`entry-${entry.id}`} data-entry-id={entry.id} className={`pl-0 sm:pl-10 ${hlClass}`}>
          <DecisionReceiptCard
            action={receiptAction}
            at={entry.at}
            originEntryId={entry.re?.entry || receiptAction.entry_id}
            statusNote={statusNote}
            onJumpToEntry={onJumpToEntry}
            onFollowUp={() => onReplyQuote(entry.action_id ? entry : { ...entry, action_id: receiptAction.id })}
            onOpenWorker={onOpenWorker}
          />
        </div>
      );
    }
    return (
      <div id={`entry-${entry.id}`} data-entry-id={entry.id} className={`flex flex-col items-end gap-1 ${hlClass}`}>
        {entry.re && <QuoteBlock quote={entry.re} align="right" onJumpToEntry={onJumpToEntry} />}
        <div className="max-w-[80%] px-4 py-2.5 rounded-2xl rounded-br-sm text-[14px] leading-relaxed pill-user whitespace-pre-wrap">
          {entry.text}
        </div>
        <span className="text-[11px] text-muted pr-1">
          {entry.channel === "wecom" ? "企业微信 · " : ""}
          {timeStr}
        </span>
      </div>
    );
  }

  return (
    <div id={`entry-${entry.id}`} data-entry-id={entry.id} className={`group flex items-start gap-3 ${hlClass}`}>
      <div className="w-7 h-7 rounded-lg border border-hairline flex items-center justify-center shrink-0 mt-0.5">
        <CorgiMark className="w-4 h-4" />
      </div>
      <div className="flex-1 min-w-0 space-y-1.5">
        {entry.re && !hideQuote && <QuoteBlock quote={entry.re} align="left" onJumpToEntry={onJumpToEntry} />}
        <div className="text-[14.5px]">
          <MarkdownText
            text={entry.text}
            defaultMachine={entry.file_machine}
            entryArtifacts={entry.artifacts}
            onOpenFile={onOpenFile}
            onOpenImage={(img: MarkdownImagePayload) =>
              onOpenLightbox(
                [{ id: `md:${img.src}`, dedupKey: img.src, src: img.src, downloadUrl: img.downloadUrl, name: img.name, subtitle: img.subtitle }],
                0
              )
            }
            onOpenArtifact={(art) =>
              onOpenArtifact({ artifactId: art.id, name: art.name, kind: art.kind, shareToken: art.share_token })
            }
          />
        </div>
        <ChatEntryArtifacts
          entry={entry}
          workers={workers}
          machines={machines}
          resumingWorkerKey={resumingWorkerKey}
          onSelectWorker={onSelectWorker}
          onOpenWorkerRecord={onOpenWorkerRecord}
          onResumeWorker={onResumeWorker}
          onOpenFile={onOpenFile}
          onOpenArtifact={onOpenArtifact}
          onOpenLightbox={onOpenLightbox}
          onToast={onToast}
        />
        {inlineAction && (
          <div className="pt-1 flex flex-wrap items-center gap-1.5">
            {inlineAction.kind === "router_check" || !inlineAction.choices?.length ? (
              <button
                type="button"
                disabled={inlineBusy || !superReady}
                onClick={() => onResolveInlineAction(inlineAction, "ack", "已标记已看")}
                className="inline-flex items-center gap-1 px-2.5 py-1 rounded-full text-xs border border-strong surface-bg surface-hover text-primary cursor-pointer disabled:opacity-50"
              >
                <Check className="w-3.5 h-3.5" />
                <span>已看</span>
              </button>
            ) : (
              inlineAction.choices.map((ch, cIdx) => (
                <button
                  key={ch.key || cIdx}
                  type="button"
                  disabled={inlineBusy || !superReady}
                  onClick={() => onResolveInlineAction(inlineAction, ch.key || ch.label, "已拍板并同步")}
                  className={`px-3 py-1 rounded-full text-xs cursor-pointer disabled:opacity-50 ${
                    cIdx === 0 ? "btn-accent font-medium" : "border border-strong surface-bg surface-hover text-primary"
                  }`}
                >
                  {ch.label}
                </button>
              ))
            )}
            <button
              type="button"
              disabled={inlineBusy || !superReady}
              onClick={() => onOpenInlineOrMobile(entry, "action", inlineAction)}
              className="inline-flex items-center gap-1 px-2.5 py-1 rounded-full text-xs border border-hairline text-secondary surface-hover cursor-pointer disabled:opacity-50"
            >
              <span>回复其他…</span>
              {hasDraftText(draftsByEntryId, entry.id) && (
                <span className="w-1.5 h-1.5 rounded-full" style={{ background: "var(--attn)" }} />
              )}
            </button>
            <button
              type="button"
              disabled={inlineBusy || !superReady}
              onClick={() => onDismissInlineAction(inlineAction)}
              title="忽略待办"
              className="px-1.5 py-1 rounded-full text-xs text-muted hover:text-primary cursor-pointer disabled:opacity-50"
            >
              ×
            </button>
          </div>
        )}
        <InPlaceReplies replies={inPlaceReplies} resolvedActions={resolvedActions} highlightedEntryId={highlightedEntryId} />
        <InlineReplyBox
          entryId={entry.id}
          initialDraft={draftsByEntryId[entry.id] ?? ""}
          disabled={!superReady || inlineBusy}
          isDesktopOrTablet={isTabletOrDesktop}
          hasInlineAction={Boolean(inlineAction)}
          onDraftChange={onDraftChange}
          onRegisterOpen={onRegisterInlineOpen}
          onMobileReply={(mode) => onMobileReply(entry, mode, inlineAction)}
          onSubmit={(replyText, mode) => onSubmitInlineReply(entry, replyText, mode, inlineAction)}
        />
        <div
          className={`flex items-center gap-3 text-[11px] text-muted transition-opacity ${
            hasDraftText(draftsByEntryId, entry.id) ? "opacity-100" : "opacity-0 group-hover:opacity-100"
          }`}
        >
          <button
            type="button"
            disabled={!superReady}
            onClick={() => onOpenInlineOrMobile(entry, "say")}
            className="inline-flex items-center gap-1 hover:underline cursor-pointer disabled:opacity-40"
          >
            <span>原位补充</span>
            {hasDraftText(draftsByEntryId, entry.id) && (
              <span className="w-1.5 h-1.5 rounded-full" style={{ background: "var(--attn)" }} title="有未发送草稿" />
            )}
          </button>
          <button type="button" onClick={() => onReplyQuote(entry)} className="hover:underline cursor-pointer">
            引用回复
          </button>
          <button
            type="button"
            disabled={!superReady}
            onClick={() => void api("/api/super/pin", { id: entry.id, pinned: !entry.pinned })}
            className="inline-flex items-center gap-1 hover:underline cursor-pointer disabled:opacity-40"
          >
            <Pin className="w-3 h-3" />
            {entry.pinned ? "取消置顶" : "置顶"}
          </button>
        </div>
      </div>
    </div>
  );
}

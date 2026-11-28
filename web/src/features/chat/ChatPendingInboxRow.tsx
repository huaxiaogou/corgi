import { Check } from "lucide-react";
import { api } from "../../lib/api";
import { isFastSingleAction } from "../../lib/decisionCard";
import { actionCategoryTag } from "../../lib/inboxFilter";
import { formatShortAge } from "../../lib/workItems";
import type { Action } from "../../types/api";

interface Props {
  action: Action;
  isActivityRow: boolean;
  superReady: boolean;
  onCloseInbox: () => void;
  onJumpToEntry: (entryId: string) => void;
  onOpenWorker: (workerName: string) => void;
  onSelectFloating: (actionId: string) => void;
  onToast: (msg: string) => void;
}

export function ChatPendingInboxRow({
  action,
  isActivityRow,
  superReady,
  onCloseInbox,
  onJumpToEntry,
  onOpenWorker,
  onSelectFloating,
  onToast,
}: Props) {
  const tag = actionCategoryTag(action);
  const primaryChoice = action.choices?.[0];

  return (
    <div className="px-3.5 py-2 flex items-center justify-between gap-3 surface-hover">
      <button
        type="button"
        onClick={() => {
          if (action.entry_id) {
            onCloseInbox();
            onJumpToEntry(action.entry_id);
          } else if (action.target_worker) {
            onOpenWorker(action.target_worker);
          }
        }}
        className="min-w-0 flex items-center gap-2 text-left cursor-pointer flex-1"
      >
        <span className="px-1.5 py-0.5 rounded font-mono-code text-[10.5px] border border-hairline surface-rail text-secondary shrink-0">
          {tag.label}
        </span>
        <span className={`${isActivityRow ? "text-secondary" : "font-medium text-primary"} truncate`}>
          {action.title}
        </span>
      </button>

      {isActivityRow ? (
        <span className="text-[11px] text-muted font-mono-code shrink-0">
          {formatShortAge(action.created_at)}
        </span>
      ) : (
        <div className="flex items-center gap-1.5 shrink-0">
          {primaryChoice && isFastSingleAction(action) ? (
            <button
              type="button"
              disabled={!superReady}
              onClick={() =>
                void api(`/api/super/actions/${encodeURIComponent(action.id)}/resolve`, {
                  choice: primaryChoice.key || primaryChoice.label,
                  by: "web",
                }).then(() => onToast("已拍板并同步"))
              }
              className="px-2 py-0.5 rounded-md btn-accent text-[11px] cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed"
            >
              {primaryChoice.label}
            </button>
          ) : primaryChoice ? (
            <button
              type="button"
              onClick={() => {
                onCloseInbox();
                onSelectFloating(action.id);
              }}
              className="px-2 py-0.5 rounded-md border border-strong surface-bg surface-hover text-primary text-[11px] cursor-pointer"
            >
              去选择
            </button>
          ) : action.kind === "router_check" ? (
            <button
              type="button"
              disabled={!superReady}
              onClick={() =>
                void api(`/api/super/actions/${encodeURIComponent(action.id)}/resolve`, {
                  choice: "ack",
                  by: "web",
                }).then(() => onToast("已标记已看"))
              }
              className="inline-flex items-center gap-1 px-2 py-0.5 rounded-md border border-strong surface-bg surface-hover text-primary text-[11px] cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed"
            >
              <Check className="w-3 h-3" />
              <span>已看</span>
            </button>
          ) : null}

          {action.kind !== "worker_dialog" && (
            <button
              type="button"
              disabled={!superReady}
              onClick={() =>
                void api(`/api/super/actions/${encodeURIComponent(action.id)}/snooze`, {}).then(() =>
                  onToast("已暂缓至明天晨报提醒")
                )
              }
              className="px-2 py-0.5 rounded-md border border-strong text-secondary surface-hover text-[11px] cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed"
            >
              明天
            </button>
          )}

          <button
            type="button"
            disabled={!superReady}
            onClick={() =>
              void api(`/api/super/actions/${encodeURIComponent(action.id)}/dismiss`, {}).then(() =>
                onToast("已忽略待办")
              )
            }
            className="text-muted hover:text-primary px-1 cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed"
            title="忽略"
          >
            ×
          </button>
        </div>
      )}
    </div>
  );
}

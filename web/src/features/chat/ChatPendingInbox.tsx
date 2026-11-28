import { Check } from "lucide-react";
import { api } from "../../lib/api";
import { isFastSingleAction } from "../../lib/decisionCard";
import type { Action } from "../../types/api";

export function actionPriority(a: Action): number {
  if (a.kind === "worker_dialog" || a.category === "blocked") return 0;
  if (a.category === "worktree") return 1;
  if (a.kind === "patrol_digest" || a.kind === "router_choice") return 2;
  return 3;
}

export function actionCategoryTag(a: Action): { key: string; label: string } {
  if (a.kind === "worker_dialog" || a.category === "blocked") return { key: "blocked", label: "终端阻塞" };
  if (a.category === "worktree") return { key: "worktree", label: "未合分支" };
  if (a.kind === "patrol_digest") {
    const cat = (a.category ?? "").toLowerCase();
    if (cat === "morning") return { key: "digest", label: "晨间简报" };
    if (cat === "stale") return { key: "digest", label: "会话归档" };
    return { key: "digest", label: "待办提醒" };
  }
  if (a.kind === "router_check") return { key: "check", label: "审阅提醒" };
  return { key: "decision", label: "待拍板" };
}

interface Props {
  pendingActions: Action[];
  pendingDecisions: Action[];
  pendingChecks: Action[];
  snoozedCount: number;
  superReady: boolean;
  inboxOpen: boolean;
  inboxFilter: string;
  inboxSearch: string;
  onToggleOpen: () => void;
  onSetOpen: (open: boolean) => void;
  onFilterChange: (filter: string) => void;
  onSearchChange: (search: string) => void;
  onSelectFloating: (actionId: string) => void;
  onJumpToEntry: (entryId: string) => void;
  onOpenWorker: (name: string) => void;
  onToast: (msg: string) => void;
}

export function ChatPendingInbox({
  pendingActions,
  pendingDecisions,
  pendingChecks,
  snoozedCount,
  superReady,
  inboxOpen,
  inboxFilter,
  inboxSearch,
  onToggleOpen,
  onSetOpen,
  onFilterChange,
  onSearchChange,
  onSelectFloating,
  onJumpToEntry,
  onOpenWorker,
  onToast,
}: Props) {
  if (pendingActions.length === 0 && snoozedCount === 0) return null;

  const topAction = pendingDecisions[0] ?? pendingChecks[0] ?? null;
  const filteredInbox = pendingActions.filter((a) => {
    if (inboxFilter !== "all" && actionCategoryTag(a).key !== inboxFilter) return false;
    if (inboxSearch.trim()) {
      const q = inboxSearch.toLowerCase();
      return `${a.title} ${a.target_worker ?? ""} ${a.summary ?? ""}`.toLowerCase().includes(q);
    }
    return true;
  });

  return (
    <div className="border-b border-hairline surface-rail shrink-0 text-xs">
      <div className="px-5 py-2 flex items-center justify-between gap-3">
        <button
          type="button"
          onClick={() => pendingActions.length > 0 && onToggleOpen()}
          className="flex items-center gap-2 min-w-0 text-left cursor-pointer"
        >
          <span
            className="w-2 h-2 rounded-full shrink-0"
            style={{ background: pendingDecisions.length > 0 ? "var(--attn)" : "var(--fg-3)" }}
          />
          <span className="font-medium text-primary shrink-0">
            {pendingDecisions.length > 0
              ? pendingChecks.length > 0
                ? `待你拍板 (${pendingDecisions.length}) · 审阅提醒 (${pendingChecks.length})`
                : `待你拍板 (${pendingDecisions.length})`
              : pendingChecks.length > 0
                ? `审阅提醒 (${pendingChecks.length})`
                : `已暂缓 (${snoozedCount})`}
          </span>
          {topAction ? (
            <>
              <span className="text-muted">·</span>
              <span className="text-secondary truncate">{topAction.title}</span>
            </>
          ) : (
            <span className="text-muted truncate">将在下次晨报重新浮出</span>
          )}
        </button>
        {pendingActions.length > 1 && (
          <button
            type="button"
            onClick={onToggleOpen}
            className="px-2 py-0.5 rounded-md border border-strong surface-bg surface-hover text-secondary cursor-pointer shrink-0"
          >
            {inboxOpen ? "收起收件箱" : "展开收件箱"}
          </button>
        )}
      </div>
      {inboxOpen && pendingActions.length > 0 && (
        <div className="px-5 pb-3 pt-1.5 space-y-2 border-t border-hairline surface-bg">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div className="flex flex-wrap items-center gap-1.5">
              {[
                { key: "all", label: "全部", count: pendingActions.length },
                { key: "blocked", label: "终端阻塞", count: pendingActions.filter((a) => actionCategoryTag(a).key === "blocked").length },
                { key: "worktree", label: "未合分支", count: pendingActions.filter((a) => actionCategoryTag(a).key === "worktree").length },
                { key: "digest", label: "巡检简报", count: pendingActions.filter((a) => actionCategoryTag(a).key === "digest").length },
                { key: "decision", label: "待拍板", count: pendingActions.filter((a) => actionCategoryTag(a).key === "decision").length },
                { key: "check", label: "审阅提醒", count: pendingActions.filter((a) => actionCategoryTag(a).key === "check").length },
              ]
                .filter((f) => f.key === "all" || f.count > 0)
                .map((f) => (
                  <button
                    key={f.key}
                    type="button"
                    onClick={() => onFilterChange(f.key)}
                    className={`px-2.5 py-0.5 rounded-full border cursor-pointer ${
                      inboxFilter === f.key ? "btn-accent border-transparent" : "border-hairline text-secondary surface-hover"
                    }`}
                  >
                    {f.label} {f.count}
                  </button>
                ))}
            </div>
            <input
              value={inboxSearch}
              onChange={(e) => onSearchChange(e.target.value)}
              placeholder="筛选任务…"
              className="w-36 px-2.5 py-1 rounded-lg border border-hairline surface-rail text-xs outline-none"
            />
          </div>
          <div className="max-h-56 overflow-y-auto divide-y divide-[var(--line)] rounded-xl border border-hairline">
            {filteredInbox.slice(0, 20).map((act) => {
              const tag = actionCategoryTag(act);
              const primaryChoice = act.choices?.[0];
              return (
                <div key={act.id} className="px-3.5 py-2 flex items-center justify-between gap-3 surface-hover">
                  <button
                    type="button"
                    onClick={() => {
                      if (act.entry_id) {
                        onSetOpen(false);
                        onJumpToEntry(act.entry_id);
                      } else if (act.target_worker) {
                        onOpenWorker(act.target_worker);
                      }
                    }}
                    className="min-w-0 flex items-center gap-2 text-left cursor-pointer flex-1"
                  >
                    <span className="px-1.5 py-0.5 rounded font-mono-code text-[10.5px] border border-hairline surface-rail text-secondary shrink-0">
                      {tag.label}
                    </span>
                    <span className="font-medium text-primary truncate">{act.title}</span>
                  </button>
                  <div className="flex items-center gap-1.5 shrink-0">
                    {primaryChoice && isFastSingleAction(act) ? (
                      <button
                        type="button"
                        disabled={!superReady}
                        onClick={() =>
                          void api(`/api/super/actions/${encodeURIComponent(act.id)}/resolve`, {
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
                        onClick={() => onSelectFloating(act.id)}
                        className="px-2 py-0.5 rounded-md border border-strong surface-bg surface-hover text-primary text-[11px] cursor-pointer"
                      >
                        去选择
                      </button>
                    ) : act.kind === "router_check" ? (
                      <button
                        type="button"
                        disabled={!superReady}
                        onClick={() =>
                          void api(`/api/super/actions/${encodeURIComponent(act.id)}/resolve`, {
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
                    {act.kind !== "worker_dialog" && (
                      <button
                        type="button"
                        disabled={!superReady}
                        onClick={() =>
                          void api(`/api/super/actions/${encodeURIComponent(act.id)}/snooze`, {}).then(() =>
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
                        void api(`/api/super/actions/${encodeURIComponent(act.id)}/dismiss`, {}).then(() =>
                          onToast("已忽略待办")
                        )
                      }
                      className="text-muted hover:text-primary px-1 cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed"
                      title="忽略"
                    >
                      ×
                    </button>
                  </div>
                </div>
              );
            })}
            {filteredInbox.length > 20 && (
              <div className="px-3.5 py-2 text-center text-[11.5px] text-muted surface-rail">
                已展示前 20 条（共 {filteredInbox.length} 条），支持按分类或搜索快速筛选
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

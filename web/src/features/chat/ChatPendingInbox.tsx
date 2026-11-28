import { useEffect, useState } from "react";
import { api } from "../../lib/api";
import { actionCategoryTag, formatInboxSummaryMeta } from "../../lib/inboxFilter";
import type { Action } from "../../types/api";
import { ChatPendingInboxRow } from "./ChatPendingInboxRow";

export { actionCategoryTag, actionPriority } from "../../lib/inboxFilter";

interface Props {
  thread: string;
  pendingActions: Action[];
  pendingDecisions: Action[];
  pendingChecks: Action[];
  activityNotices: Action[];
  snoozedCount: number;
  superReady: boolean;
  inboxOpen: boolean;
  onToggleOpen: () => void;
  onSetOpen: (open: boolean) => void;
  onSelectFloating: (actionId: string) => void;
  onJumpToEntry: (entryId: string) => void;
  onOpenWorker: (name: string) => void;
  onToast: (msg: string) => void;
}

const FILTER_SPECS = [
  { key: "blocked", label: "终端阻塞" },
  { key: "worktree", label: "未合分支" },
  { key: "digest", label: "巡检简报" },
  { key: "decision", label: "待拍板" },
  { key: "check", label: "审阅提醒" },
] as const;

export function ChatPendingInbox({
  thread,
  pendingActions,
  pendingDecisions,
  pendingChecks,
  activityNotices,
  snoozedCount,
  superReady,
  inboxOpen,
  onToggleOpen,
  onSetOpen,
  onSelectFloating,
  onJumpToEntry,
  onOpenWorker,
  onToast,
}: Props) {
  const [inboxFilter, setInboxFilter] = useState("all");
  const [inboxSearch, setInboxSearch] = useState("");
  const [bulkBusy, setBulkBusy] = useState<"checks" | "activity" | "">("");

  useEffect(() => {
    setInboxFilter("all");
    setInboxSearch("");
  }, [thread]);

  useEffect(() => {
    if (inboxFilter === "activity" && activityNotices.length === 0) {
      setInboxFilter("all");
    }
  }, [inboxFilter, activityNotices.length]);

  const summaryMeta = formatInboxSummaryMeta(
    pendingDecisions.length,
    pendingChecks.length,
    snoozedCount,
    activityNotices.length
  );
  if (!summaryMeta.visible) return null;

  const onlyActivity = pendingActions.length === 0 && snoozedCount === 0 && activityNotices.length > 0;
  const activeTab =
    onlyActivity || (inboxFilter === "activity" && activityNotices.length > 0) ? "activity" : inboxFilter;
  const topAction = pendingDecisions[0] ?? pendingChecks[0] ?? null;
  const topActivity = activityNotices[0] ?? null;
  const canExpand = pendingActions.length > 0 || activityNotices.length > 0;
  const sourceList = activeTab === "activity" ? activityNotices : pendingActions;
  const filteredItems = sourceList.filter((a) => {
    if (activeTab !== "all" && activeTab !== "activity" && actionCategoryTag(a).key !== activeTab) return false;
    if (!inboxSearch.trim()) return true;
    const q = inboxSearch.toLowerCase();
    return `${a.title} ${a.target_worker ?? ""} ${a.summary ?? ""}`.toLowerCase().includes(q);
  });

  const ackAllChecks = async () => {
    if (!superReady || bulkBusy) return;
    setBulkBusy("checks");
    try {
      const res = await api<{ ok: boolean; acked: number }>("/api/super/actions/ack-checks", { thread, by: "web" });
      onToast(res.acked > 0 ? `已将 ${res.acked} 条审阅提醒标为已看` : "暂无待清审阅提醒");
    } catch (err) {
      onToast(err instanceof Error ? err.message : "操作失败");
    } finally {
      setBulkBusy("");
    }
  };

  const clearActivity = async () => {
    if (!superReady || bulkBusy) return;
    setBulkBusy("activity");
    try {
      await api("/api/super/activity/clear", { thread });
      if (onlyActivity) onSetOpen(false);
      else setInboxFilter("all");
      onToast("已清空巡检动态");
    } catch (err) {
      onToast(err instanceof Error ? err.message : "清空失败");
    } finally {
      setBulkBusy("");
    }
  };

  const categoryTabs = [
    { key: "all", label: "全部", count: pendingActions.length },
    ...FILTER_SPECS.map((s) => ({
      key: s.key,
      label: s.label,
      count: pendingActions.filter((a) => actionCategoryTag(a).key === s.key).length,
    })),
  ].filter((f) => f.key === "all" || f.count > 0);

  return (
    <div className="border-b border-hairline surface-rail shrink-0 text-xs">
      <div className="px-5 py-2 flex items-center justify-between gap-3">
        <button
          type="button"
          onClick={() => canExpand && onToggleOpen()}
          className="flex items-center gap-2 min-w-0 text-left cursor-pointer"
        >
          <span
            className="w-2 h-2 rounded-full shrink-0"
            style={{ background: summaryMeta.isBlocking ? "var(--attn)" : "var(--fg-3)" }}
          />
          <span className={`shrink-0 ${onlyActivity ? "text-secondary" : "font-medium text-primary"}`}>
            {summaryMeta.headline}
          </span>
          {topAction ? (
            <>
              <span className="text-muted">·</span>
              <span className="text-secondary truncate">{topAction.title}</span>
            </>
          ) : snoozedCount > 0 ? (
            <span className="text-muted truncate">将在下次晨报重新浮出</span>
          ) : topActivity ? (
            <>
              <span className="text-muted">·</span>
              <span className="text-muted truncate">{topActivity.title}</span>
            </>
          ) : null}
        </button>

        <div className="flex items-center gap-1.5 shrink-0">
          {onlyActivity && (
            <>
              <button
                type="button"
                onClick={onToggleOpen}
                className="px-2 py-0.5 rounded-md border border-hairline surface-bg surface-hover text-secondary cursor-pointer"
              >
                {inboxOpen ? "收起" : "查看"}
              </button>
              <button
                type="button"
                disabled={!superReady || bulkBusy === "activity"}
                onClick={() => void clearActivity()}
                className="px-2 py-0.5 rounded-md border border-hairline surface-bg surface-hover text-muted hover:text-primary cursor-pointer disabled:opacity-40"
              >
                清空
              </button>
            </>
          )}
          {!onlyActivity && pendingDecisions.length === 0 && pendingChecks.length > 0 && (
            <button
              type="button"
              disabled={!superReady || bulkBusy === "checks"}
              onClick={() => void ackAllChecks()}
              className="px-2 py-0.5 rounded-md border border-hairline surface-bg surface-hover text-secondary cursor-pointer disabled:opacity-40"
            >
              全部已看
            </button>
          )}
          {!onlyActivity && (pendingActions.length > 1 || (pendingActions.length > 0 && activityNotices.length > 0)) && (
            <button
              type="button"
              onClick={onToggleOpen}
              className="px-2 py-0.5 rounded-md border border-strong surface-bg surface-hover text-secondary cursor-pointer"
            >
              {inboxOpen ? "收起收件箱" : "展开收件箱"}
            </button>
          )}
        </div>
      </div>

      {inboxOpen && canExpand && (
        <div className="px-5 pb-3 pt-1.5 space-y-2 border-t border-hairline surface-bg">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div className="flex flex-wrap items-center gap-1.5">
              {!onlyActivity &&
                categoryTabs.map((f) => (
                  <button
                    key={f.key}
                    type="button"
                    onClick={() => setInboxFilter(f.key)}
                    className={`px-2.5 py-0.5 rounded-full border cursor-pointer ${
                      activeTab === f.key ? "btn-accent border-transparent" : "border-hairline text-secondary surface-hover"
                    }`}
                  >
                    {f.label} {f.count}
                  </button>
                ))}
              {activityNotices.length > 0 && (
                <button
                  type="button"
                  onClick={() => setInboxFilter("activity")}
                  className={`px-2.5 py-0.5 rounded-full border cursor-pointer ${
                    activeTab === "activity" ? "btn-accent border-transparent" : "border-hairline text-muted surface-hover"
                  }`}
                >
                  巡检动态
                </button>
              )}
            </div>
            <div className="flex items-center gap-1.5">
              {activeTab === "activity" && activityNotices.length > 0 ? (
                <button
                  type="button"
                  disabled={!superReady || bulkBusy === "activity"}
                  onClick={() => void clearActivity()}
                  className="px-2 py-1 rounded-lg border border-hairline surface-rail surface-hover text-secondary text-[11px] cursor-pointer disabled:opacity-40"
                >
                  一键清空动态
                </button>
              ) : pendingChecks.length > 0 ? (
                <button
                  type="button"
                  disabled={!superReady || bulkBusy === "checks"}
                  onClick={() => void ackAllChecks()}
                  className="px-2 py-1 rounded-lg border border-hairline surface-rail surface-hover text-secondary text-[11px] cursor-pointer disabled:opacity-40"
                >
                  审阅全部已看
                </button>
              ) : null}
              <input
                value={inboxSearch}
                onChange={(e) => setInboxSearch(e.target.value)}
                placeholder={activeTab === "activity" ? "筛选动态…" : "筛选任务…"}
                className="w-36 px-2.5 py-1 rounded-lg border border-hairline surface-rail text-xs outline-none"
              />
            </div>
          </div>

          <div className="max-h-56 overflow-y-auto divide-y divide-[var(--line)] rounded-xl border border-hairline">
            {filteredItems.slice(0, 20).map((act) => (
              <ChatPendingInboxRow
                key={act.id}
                action={act}
                isActivityRow={activeTab === "activity"}
                superReady={superReady}
                onCloseInbox={() => onSetOpen(false)}
                onJumpToEntry={onJumpToEntry}
                onOpenWorker={onOpenWorker}
                onSelectFloating={onSelectFloating}
                onToast={onToast}
              />
            ))}
            {filteredItems.length > 20 && (
              <div className="px-3.5 py-2 text-center text-[11.5px] text-muted surface-rail">
                已展示前 20 条（共 {filteredItems.length} 条），支持按分类或搜索快速筛选
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

import { WORK_PAGE_SIZE } from "../../lib/workItems";
import type { WorkItem } from "../../types/api";
import { formatArtifactStamp } from "./WorkNotebookRow";

interface Props {
  doneTotal: number;
  doneExpanded: boolean;
  doneItems: WorkItem[];
  doneHasMore: boolean;
  doneLoading: boolean;
  onToggleExpanded: () => void;
  onReopen: (item: WorkItem) => void;
  onLoadMore: () => void;
}

export function WorkNotebookDoneSection({
  doneTotal,
  doneExpanded,
  doneItems,
  doneHasMore,
  doneLoading,
  onToggleExpanded,
  onReopen,
  onLoadMore,
}: Props) {
  if (doneTotal === 0 && !doneExpanded) return null;
  const doneRemaining = Math.max(0, doneTotal - doneItems.length);
  return (
    <div className="border-t border-hairline">
      <button
        type="button"
        onClick={onToggleExpanded}
        className="w-full px-4 py-2.5 flex items-center justify-between text-[11.5px] text-muted surface-hover cursor-pointer"
      >
        <span className="inline-flex items-center gap-1.5">
          <span className="font-mono-code text-[10px]">{doneExpanded ? "▾" : "▸"}</span>
          <span>已划掉 ({doneTotal})</span>
        </span>
        {doneTotal > WORK_PAGE_SIZE && <span className="text-[10.5px]">按需分页加载</span>}
      </button>
      {doneExpanded && (
        <div className="px-3 pb-3 space-y-1">
          {doneItems.length === 0 && doneLoading ? (
            <div className="px-2 py-2 text-center text-[11px] text-muted">加载中…</div>
          ) : doneItems.length === 0 ? (
            <div className="px-2 py-2 text-center text-[11px] text-muted">暂无已划掉记录</div>
          ) : (
            doneItems.map((item) => (
              <div key={item.id} className="px-2 py-1.5 rounded-lg flex items-start justify-between gap-2 text-[11.5px] surface-hover">
                <div className="min-w-0 flex-1">
                  <div className="truncate text-muted line-through" title={item.title}>
                    {item.title}
                  </div>
                  {(item.close_note || item.closed_at) && (
                    <div className="text-[10.5px] text-muted truncate mt-0.5">
                      {item.closed_at ? `${formatArtifactStamp(item.closed_at)} · ` : ""}
                      {item.close_note || ""}
                    </div>
                  )}
                </div>
                <button
                  type="button"
                  onClick={() => onReopen(item)}
                  className="text-[11px] text-muted hover:underline shrink-0 cursor-pointer"
                >
                  恢复
                </button>
              </div>
            ))
          )}
          {doneHasMore && (
            <button
              type="button"
              disabled={doneLoading}
              onClick={onLoadMore}
              className="w-full py-1.5 mt-1 rounded-lg border border-hairline text-center text-[11px] text-muted surface-hover cursor-pointer disabled:opacity-50"
            >
              {doneLoading ? "加载中…" : `加载更多已划掉${doneRemaining > 0 ? `（还有 ${doneRemaining} 项）` : ""}`}
            </button>
          )}
        </div>
      )}
    </div>
  );
}

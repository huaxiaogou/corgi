import { useEffect, useMemo, useRef, useState } from "react";
import { Minimize2, Search } from "lucide-react";
import { api } from "../../lib/api";
import {
  FILTER_BAR_THRESHOLD,
  MINI_MAX_ROWS,
  WORK_PAGE_SIZE,
  applyFrozenOrder,
  deriveWorkItems,
  filterAndPartitionWorkItems,
  type DerivedWorkStatus,
  type EnrichedWorkItem,
  type NotebookMode,
} from "../../lib/workItems";
import type { Action, FileDrawerTarget, WorkItem, Worker } from "../../types/api";
import { WorkNotebookDoneSection } from "./WorkNotebookDoneSection";
import { WorkNotebookMini } from "./WorkNotebookMini";
import { WorkNotebookRow, dotBg } from "./WorkNotebookRow";
import { prefersReducedMotion, useNotebookFlip } from "./useNotebookFlip";

interface Props {
  items: WorkItem[];
  closedCount: number;
  workers: Worker[];
  pendingActions: Action[];
  thread: string;
  mode: NotebookMode;
  isDesktop: boolean;
  onModeChange: (mode: NotebookMode) => void;
  onAskCorgi: (item: WorkItem) => void;
  onOpenWorker: (name: string) => void;
  onOpenFile: (path: string, machine?: string) => void;
  onOpenArtifact?: (target: FileDrawerTarget) => void;
  onToast: (msg: string) => void;
}

export function WorkNotebook({
  items,
  closedCount,
  workers,
  pendingActions,
  thread,
  mode,
  isDesktop,
  onModeChange,
  onAskCorgi,
  onOpenWorker,
  onOpenFile,
  onOpenArtifact,
  onToast,
}: Props) {
  const [dotFilter, setDotFilter] = useState<"all" | DerivedWorkStatus>("all");
  const [searchQuery, setSearchQuery] = useState("");
  const [openItemId, setOpenItemId] = useState<string | null>(null);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editDraft, setEditDraft] = useState("");
  const [editingTitleId, setEditingTitleId] = useState<string | null>(null);
  const [titleDraft, setTitleDraft] = useState("");
  const [expandedArtsIds, setExpandedArtsIds] = useState<Set<string>>(() => new Set());
  const [savingId, setSavingId] = useState<string | null>(null);
  const [exitingIds, setExitingIds] = useState<Set<string>>(() => new Set());
  const [optimisticClosedIds, setOptimisticClosedIds] = useState<Set<string>>(() => new Set());
  const [staleExpanded, setStaleExpanded] = useState(false);
  const [staleLimit, setStaleLimit] = useState(WORK_PAGE_SIZE);
  const [doneExpanded, setDoneExpanded] = useState(false);
  const [doneItems, setDoneItems] = useState<WorkItem[]>([]);
  const [doneTotal, setDoneTotal] = useState(closedCount);
  const [doneHasMore, setDoneHasMore] = useState(false);
  const [doneLoading, setDoneLoading] = useState(false);

  const [miniHovered, setMiniHovered] = useState(false);
  const [drawerHovered, setDrawerHovered] = useState(false);
  const frozenMiniIdsRef = useRef<string[] | null>(null);
  const frozenDrawerIdsRef = useRef<string[] | null>(null);

  const prevDerivedRef = useRef<Map<string, DerivedWorkStatus>>(new Map());
  const hydratedRef = useRef(false);
  const [pulsingIds, setPulsingIds] = useState<Set<string>>(() => new Set());

  const miniListRef = useRef<HTMLDivElement | null>(null);
  const activeListRef = useRef<HTMLDivElement | null>(null);
  const editInputRef = useRef<HTMLInputElement | null>(null);

  const serverOpenIdsSig = items.map((x) => x.id).join(",");
  useEffect(() => {
    const serverOpen = new Set(items.map((x) => x.id));
    setOptimisticClosedIds((prev) => {
      if (prev.size === 0) return prev;
      let changed = false;
      const next = new Set<string>();
      for (const id of prev) {
        if (serverOpen.has(id)) next.add(id);
        else changed = true;
      }
      return changed ? next : prev;
    });
  }, [serverOpenIdsSig]);

  useEffect(() => {
    setDotFilter("all");
    setSearchQuery("");
    setOpenItemId(null);
    setEditingId(null);
    setStaleExpanded(false);
    setStaleLimit(WORK_PAGE_SIZE);
    setDoneExpanded(false);
    setDoneItems([]);
    setDoneTotal(closedCount);
    setDoneHasMore(false);
    frozenMiniIdsRef.current = null;
    frozenDrawerIdsRef.current = null;
    hydratedRef.current = false;
    prevDerivedRef.current = new Map();
  }, [thread]);

  useEffect(() => {
    if (!searchQuery.trim()) setDoneTotal(closedCount + optimisticClosedIds.size);
  }, [closedCount, optimisticClosedIds.size, searchQuery]);

  useEffect(() => {
    if (editingId && editInputRef.current) {
      editInputRef.current.focus();
      editInputRef.current.select();
    }
  }, [editingId]);

  const naturalOpen = useMemo(() => {
    const visibleSource = optimisticClosedIds.size === 0 ? items : items.filter((x) => !optimisticClosedIds.has(x.id));
    return deriveWorkItems(visibleSource, workers, pendingActions, thread);
  }, [items, optimisticClosedIds, workers, pendingActions, thread]);

  useEffect(() => {
    const prev = prevDerivedRef.current;
    const newlyBlocked: string[] = [];
    if (hydratedRef.current) {
      for (const entry of naturalOpen) {
        const was = prev.get(entry.item.id);
        if (entry.derived === "blocked" && was && was !== "blocked") newlyBlocked.push(entry.item.id);
      }
    }
    const nextMap = new Map<string, DerivedWorkStatus>();
    for (const entry of naturalOpen) nextMap.set(entry.item.id, entry.derived);
    prevDerivedRef.current = nextMap;
    if (!hydratedRef.current && naturalOpen.length > 0) hydratedRef.current = true;
    if (newlyBlocked.length > 0) {
      setPulsingIds((cur) => {
        const next = new Set(cur);
        for (const id of newlyBlocked) next.add(id);
        return next;
      });
      const timer = window.setTimeout(() => {
        setPulsingIds((cur) => {
          const next = new Set(cur);
          let changed = false;
          for (const id of newlyBlocked) if (next.delete(id)) changed = true;
          return changed ? next : cur;
        });
      }, 800);
      return () => window.clearTimeout(timer);
    }
  }, [naturalOpen]);

  const miniOrdered = useMemo(() => {
    if (miniHovered && frozenMiniIdsRef.current) return applyFrozenOrder(naturalOpen, frozenMiniIdsRef.current);
    frozenMiniIdsRef.current = naturalOpen.map((x) => x.item.id);
    return naturalOpen;
  }, [naturalOpen, miniHovered]);

  const drawerLocked = drawerHovered || editingId !== null || editingTitleId !== null;
  const drawerOrdered = useMemo(() => {
    if (drawerLocked && frozenDrawerIdsRef.current) return applyFrozenOrder(naturalOpen, frozenDrawerIdsRef.current);
    frozenDrawerIdsRef.current = naturalOpen.map((x) => x.item.id);
    return naturalOpen;
  }, [naturalOpen, drawerLocked]);

  const miniShown = miniOrdered.slice(0, MINI_MAX_ROWS);
  const miniHiddenCount = Math.max(0, miniOrdered.length - miniShown.length);
  const showFilterBar =
    naturalOpen.length > FILTER_BAR_THRESHOLD || (doneExpanded && doneTotal > 0) || searchQuery.trim() !== "" || dotFilter !== "all";
  const { active: activeGroup, stale: staleGroup, filteringActive } = useMemo(
    () => filterAndPartitionWorkItems(drawerOrdered, searchQuery, dotFilter),
    [drawerOrdered, searchQuery, dotFilter]
  );

  useNotebookFlip({
    mode,
    isDesktop,
    miniListRef,
    activeListRef,
    miniOrderSig: miniShown.map((x) => x.item.id).join(","),
    activeOrderSig: activeGroup.map((x) => x.item.id).join(","),
    resetKey: thread,
  });

  const fetchClosedPage = async (offset: number, append: boolean, qStr: string) => {
    setDoneLoading(true);
    try {
      const qs = new URLSearchParams({ status: "closed", limit: String(WORK_PAGE_SIZE), offset: String(offset) });
      if (thread) qs.set("thread", thread);
      if (qStr.trim()) qs.set("q", qStr.trim());
      const res = await api<{ work_items: WorkItem[]; total: number; has_more: boolean }>(`/api/super/tasks?${qs.toString()}`);
      const list = res.work_items ?? [];
      setDoneItems((prev) => (append ? [...prev, ...list] : list));
      setDoneTotal(res.total ?? list.length);
      setDoneHasMore(Boolean(res.has_more));
    } catch (err) {
      onToast(err instanceof Error ? err.message : "加载已划掉记录失败");
    } finally {
      setDoneLoading(false);
    }
  };

  useEffect(() => {
    if (!doneExpanded || mode !== "expanded") return;
    const timer = window.setTimeout(() => void fetchClosedPage(0, false, searchQuery), searchQuery.trim() ? 180 : 0);
    return () => window.clearTimeout(timer);
  }, [doneExpanded, mode, thread, searchQuery, closedCount]);

  const saveTitle = async (item: WorkItem) => {
    const trimmed = titleDraft.trim();
    if (!trimmed || trimmed === item.title.trim()) {
      setEditingTitleId(null);
      return;
    }
    setSavingId(item.id);
    try {
      await api("/api/super/tasks", { id: item.id, title: trimmed, user_edited_title: true });
      setEditingTitleId(null);
      onToast("已更新标题（已锁定防止自动覆盖）");
    } catch (err) {
      onToast(err instanceof Error ? err.message : "改名失败");
    } finally {
      setSavingId(null);
    }
  };

  const unbindWorker = async (item: WorkItem, workerName: string) => {
    setSavingId(item.id);
    try {
      await api(`/api/super/tasks/${encodeURIComponent(item.id)}/unbind-worker`, { worker: workerName });
      onToast(`已从本事项解绑 ${workerName}（历史记录与会话保留）`);
    } catch (err) {
      onToast(err instanceof Error ? err.message : "解绑失败");
    } finally {
      setSavingId(null);
    }
  };

  const saveNextStep = async (item: WorkItem) => {
    const trimmed = editDraft.trim();
    if (trimmed === (item.next_step ?? "").trim()) {
      setEditingId(null);
      return;
    }
    setSavingId(item.id);
    try {
      await api("/api/super/tasks", { id: item.id, next_step: trimmed, clear_next_step: trimmed === "" });
      setEditingId(null);
      onToast("已更新下一步");
    } catch (err) {
      onToast(err instanceof Error ? err.message : "更新失败");
    } finally {
      setSavingId(null);
    }
  };

  const markDone = (item: WorkItem) => {
    if (exitingIds.has(item.id)) return;
    setExitingIds((prev) => new Set(prev).add(item.id));
    window.setTimeout(() => {
      setOptimisticClosedIds((prev) => new Set(prev).add(item.id));
      setExitingIds((prev) => {
        const next = new Set(prev);
        next.delete(item.id);
        return next;
      });
      if (openItemId === item.id) setOpenItemId(null);
      if (editingId === item.id) setEditingId(null);
      if (frozenMiniIdsRef.current) frozenMiniIdsRef.current = frozenMiniIdsRef.current.filter((id) => id !== item.id);
      if (frozenDrawerIdsRef.current) frozenDrawerIdsRef.current = frozenDrawerIdsRef.current.filter((id) => id !== item.id);
      void api(`/api/super/tasks/${encodeURIComponent(item.id)}/done`, { note: "已确认完成" })
        .then(() => onToast(`已划掉「${item.title}」（底部可恢复）`))
        .catch((err) => {
          setOptimisticClosedIds((prev) => {
            const next = new Set(prev);
            next.delete(item.id);
            return next;
          });
          onToast(err instanceof Error ? err.message : "操作失败");
        });
    }, prefersReducedMotion() ? 0 : 180);
  };

  const reopenItem = async (item: WorkItem) => {
    try {
      await api(`/api/super/tasks/${encodeURIComponent(item.id)}/reopen`, {});
      setDoneItems((prev) => prev.filter((x) => x.id !== item.id));
      setDoneTotal((n) => Math.max(0, n - 1));
      frozenMiniIdsRef.current = null;
      frozenDrawerIdsRef.current = null;
      onToast(`已恢复「${item.title}」`);
    } catch (err) {
      onToast(err instanceof Error ? err.message : "恢复失败");
    }
  };

  const renderRow = (entry: EnrichedWorkItem) => (
    <WorkNotebookRow
      key={entry.item.id}
      entry={entry}
      isOpen={openItemId === entry.item.id}
      isEditing={editingId === entry.item.id}
      isEditingTitle={editingTitleId === entry.item.id}
      isExiting={exitingIds.has(entry.item.id)}
      isPulsing={pulsingIds.has(entry.item.id)}
      isSaving={savingId === entry.item.id}
      artsExpanded={expandedArtsIds.has(entry.item.id)}
      isDesktop={isDesktop}
      editDraft={editDraft}
      titleDraft={titleDraft}
      editInputRef={editInputRef}
      onToggleOpen={() => {
        setOpenItemId((cur) => (cur === entry.item.id ? null : entry.item.id));
        setEditingId(null);
        setEditingTitleId(null);
      }}
      onStartEditTitle={() => {
        setEditingTitleId(entry.item.id);
        setTitleDraft(entry.item.title);
      }}
      onCancelEditTitle={() => setEditingTitleId(null)}
      onChangeTitleDraft={setTitleDraft}
      onSaveTitle={(it) => void saveTitle(it)}
      onStartEditNextStep={() => {
        setEditingId(entry.item.id);
        setEditDraft(entry.item.next_step ?? "");
      }}
      onCancelEditNextStep={() => setEditingId(null)}
      onChangeEditDraft={setEditDraft}
      onSaveNextStep={(it) => void saveNextStep(it)}
      onExpandArts={() => setExpandedArtsIds((prev) => new Set(prev).add(entry.item.id))}
      onOpenWorker={onOpenWorker}
      onUnbindWorker={(it, wk) => void unbindWorker(it, wk)}
      onOpenFile={onOpenFile}
      onOpenArtifact={onOpenArtifact}
      onAskCorgi={onAskCorgi}
      onHideMobile={() => onModeChange("hidden")}
      onMarkDone={markDone}
    />
  );

  if (mode === "mini" && isDesktop) {
    return (
      <WorkNotebookMini
        totalCount={naturalOpen.length}
        miniShown={miniShown}
        miniHiddenCount={miniHiddenCount}
        pulsingIds={pulsingIds}
        miniListRef={miniListRef}
        onHoverChange={setMiniHovered}
        onExpand={() => onModeChange("expanded")}
        onSelectItem={(id) => {
          setOpenItemId(id);
          setEditingId(null);
          onModeChange("expanded");
        }}
      />
    );
  }

  if (mode !== "expanded") return null;

  const staleShown = staleGroup.slice(0, staleLimit);
  const staleRemaining = Math.max(0, staleGroup.length - staleShown.length);

  return (
    <>
      {!isDesktop && (
        <div
          className="absolute inset-0 z-20"
          style={{ background: "rgba(0,0,0,0.35)" }}
          onClick={() => onModeChange("hidden")}
        />
      )}
      <aside
        onMouseEnter={() => setDrawerHovered(true)}
        onMouseLeave={() => setDrawerHovered(false)}
        className={
          isDesktop
            ? "w-[340px] shrink-0 flex flex-col surface-bg border-l border-hairline min-h-0"
            : "absolute right-0 top-0 bottom-0 z-30 w-[92%] max-w-[340px] flex flex-col surface-bg border-l border-hairline min-h-0 shadow-xl"
        }
      >
        <div className="h-12 px-4 flex items-center justify-between border-b border-hairline shrink-0">
          <div className="flex items-center gap-2 min-w-0">
            <span className="font-semibold text-[13.5px]">小本本</span>
            <span className="text-[11px] font-mono-code text-muted">{naturalOpen.length}</span>
          </div>
          <button
            type="button"
            onClick={() => {
              setOpenItemId(null);
              setEditingId(null);
              onModeChange(isDesktop ? "mini" : "hidden");
            }}
            className="w-7 h-7 rounded-lg flex items-center justify-center text-muted surface-hover cursor-pointer"
            title={isDesktop ? "缩回页边" : "收起小本本"}
          >
            <Minimize2 className="w-3.5 h-3.5" strokeWidth={1.9} />
          </button>
        </div>

        {showFilterBar && (
          <div className="px-3.5 py-2 border-b border-hairline flex items-center gap-2 shrink-0">
            <div className="flex-1 flex items-center gap-1.5 px-2.5 py-1 rounded-lg surface-rail border border-hairline">
              <Search className="w-3.5 h-3.5 text-muted shrink-0" strokeWidth={1.8} />
              <input
                type="search"
                value={searchQuery}
                onChange={(e) => setSearchQuery(e.target.value)}
                placeholder="筛选标题、下一步或会话…"
                className="flex-1 bg-transparent text-[11.5px] outline-none min-w-0"
              />
            </div>
            <div className="inline-flex items-center gap-0.5 p-0.5 rounded-lg border border-hairline surface-rail shrink-0" title="按圆点状态筛选">
              <button
                type="button"
                onClick={() => setDotFilter("all")}
                className={`px-1.5 py-1 rounded-md text-[10.5px] cursor-pointer ${
                  dotFilter === "all" ? "surface-bg border border-strong text-primary" : "text-muted"
                }`}
              >
                全部
              </button>
              {(["failed", "blocked", "working", "review", "idle"] as const).map((st) => (
                <button
                  key={st}
                  type="button"
                  onClick={() => setDotFilter((cur) => (cur === st ? "all" : st))}
                  title={
                    st === "failed" ? "仅看执行受阻" : st === "blocked" ? "仅看待拍板" : st === "working" ? "仅看运行中" : st === "review" ? "仅看待审阅" : "仅看待推进"
                  }
                  className={`w-6 h-6 rounded-md flex items-center justify-center cursor-pointer ${
                    dotFilter === st ? "surface-bg border border-strong" : ""
                  }`}
                >
                  <span className="w-2 h-2 rounded-full" style={{ background: dotBg(st) }} />
                </button>
              ))}
            </div>
          </div>
        )}

        <div className="flex-1 overflow-y-auto">
          {staleGroup.length > 0 && activeGroup.length > 0 && (
            <div className="px-4 pt-2.5 pb-1 text-[10.5px] font-medium text-muted">近期活跃</div>
          )}

          <div ref={activeListRef} className="relative divide-y divide-[var(--line)]">
            {activeGroup.length === 0 ? (
              <div className="px-4 py-8 text-center text-xs text-muted">
                {filteringActive ? "没有匹配的记录" : "小本本空空的，聊起来派活会自动记上"}
              </div>
            ) : (
              activeGroup.map(renderRow)
            )}
          </div>

          {staleGroup.length > 0 && (
            <div className="border-t border-hairline">
              <button
                type="button"
                onClick={() => setStaleExpanded((v) => !v)}
                className="w-full px-4 py-2.5 flex items-center justify-between text-[11.5px] text-muted surface-hover cursor-pointer"
              >
                <span className="inline-flex items-center gap-1.5 font-medium">
                  <span className="font-mono-code text-[10px]">{staleExpanded ? "▾" : "▸"}</span>
                  <span>搁置超 7 天 ({staleGroup.length})</span>
                </span>
                <span className="text-[10.5px]">自动折层</span>
              </button>
              {staleExpanded && (
                <div>
                  <div className="divide-y divide-[var(--line)] border-t border-hairline">{staleShown.map(renderRow)}</div>
                  {staleRemaining > 0 && (
                    <button
                      type="button"
                      onClick={() => setStaleLimit((n) => n + WORK_PAGE_SIZE)}
                      className="w-full py-2 text-center text-[11px] text-muted surface-hover border-t border-hairline cursor-pointer"
                    >
                      展开更多搁置记录（还有 {staleRemaining} 项）
                    </button>
                  )}
                </div>
              )}
            </div>
          )}

          <WorkNotebookDoneSection
            doneTotal={doneTotal}
            doneExpanded={doneExpanded}
            doneItems={doneItems}
            doneHasMore={doneHasMore}
            doneLoading={doneLoading}
            onToggleExpanded={() => setDoneExpanded((v) => !v)}
            onReopen={(it) => void reopenItem(it)}
            onLoadMore={() => void fetchClosedPage(doneItems.length, true, searchQuery)}
          />
        </div>
      </aside>
    </>
  );
}

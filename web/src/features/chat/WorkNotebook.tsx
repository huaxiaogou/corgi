import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Check, Maximize2, Minimize2, Pencil, Search, X } from "lucide-react";
import { api } from "../../lib/api";
import {
  FILTER_BAR_THRESHOLD,
  MINI_MAX_ROWS,
  WORK_PAGE_SIZE,
  applyFrozenOrder,
  artifactBasename,
  deriveWorkItems,
  filterAndPartitionWorkItems,
  formatShortAge,
  type DerivedWorkStatus,
  type EnrichedWorkItem,
  type NotebookMode,
} from "../../lib/workItems";
import type { Action, FileDrawerTarget, WorkItem, Worker } from "../../types/api";

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

function dotBg(derived: DerivedWorkStatus): string {
  if (derived === "failed") return "var(--err)";
  if (derived === "blocked") return "var(--attn)";
  if (derived === "working") return "var(--ok)";
  if (derived === "review") return "var(--accent)";
  return "var(--fg-3)";
}

function formatArtifactStamp(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (!Number.isFinite(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function prefersReducedMotion(): boolean {
  return typeof window !== "undefined" && window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
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

  // Anti-misclick hover/edit freeze state
  const [miniHovered, setMiniHovered] = useState(false);
  const [drawerHovered, setDrawerHovered] = useState(false);
  const frozenMiniIdsRef = useRef<string[] | null>(null);
  const frozenDrawerIdsRef = useRef<string[] | null>(null);

  // Single-pulse attention halo for newly blocked items
  const prevDerivedRef = useRef<Map<string, DerivedWorkStatus>>(new Map());
  const hydratedRef = useRef(false);
  const [pulsingIds, setPulsingIds] = useState<Set<string>>(() => new Set());

  // FLIP smooth position glide refs
  const miniListRef = useRef<HTMLDivElement | null>(null);
  const activeListRef = useRef<HTMLDivElement | null>(null);
  const prevMiniTopsRef = useRef<Map<string, number>>(new Map());
  const prevActiveTopsRef = useRef<Map<string, number>>(new Map());
  const prevModeRef = useRef<NotebookMode>(mode);

  const editInputRef = useRef<HTMLInputElement | null>(null);

  // Reconcile optimistic closed items once SSE snapshot drops them
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

  // Reset thread-scoped pagination/filters when thread changes
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
    prevMiniTopsRef.current = new Map();
    prevActiveTopsRef.current = new Map();
    hydratedRef.current = false;
    prevDerivedRef.current = new Map();
  }, [thread]);

  // Keep closed total synced with SSE when not searching
  useEffect(() => {
    if (!searchQuery.trim()) {
      setDoneTotal(closedCount + optimisticClosedIds.size);
    }
  }, [closedCount, optimisticClosedIds.size, searchQuery]);

  // Focus inline next-step input when entering edit mode
  useEffect(() => {
    if (editingId && editInputRef.current) {
      editInputRef.current.focus();
      editInputRef.current.select();
    }
  }, [editingId]);

  // Compute live enriched & priority-sorted open items
  const naturalOpen = useMemo(() => {
    const visibleSource =
      optimisticClosedIds.size === 0 ? items : items.filter((x) => !optimisticClosedIds.has(x.id));
    return deriveWorkItems(visibleSource, workers, pendingActions, thread);
  }, [items, optimisticClosedIds, workers, pendingActions, thread]);

  // Detect newly blocked transitions -> single restrained amber halo (never infinite blink)
  useEffect(() => {
    const prev = prevDerivedRef.current;
    const newlyBlocked: string[] = [];
    if (hydratedRef.current) {
      for (const entry of naturalOpen) {
        const was = prev.get(entry.item.id);
        if (entry.derived === "blocked" && was && was !== "blocked") {
          newlyBlocked.push(entry.item.id);
        }
      }
    }
    const nextMap = new Map<string, DerivedWorkStatus>();
    for (const entry of naturalOpen) {
      nextMap.set(entry.item.id, entry.derived);
    }
    prevDerivedRef.current = nextMap;
    if (!hydratedRef.current && naturalOpen.length > 0) {
      hydratedRef.current = true;
    }
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
          for (const id of newlyBlocked) {
            if (next.delete(id)) changed = true;
          }
          return changed ? next : cur;
        });
      }, 800);
      return () => window.clearTimeout(timer);
    }
  }, [naturalOpen]);

  // Apply hover/edit anti-misclick freeze locks
  const miniOrdered = useMemo(() => {
    if (miniHovered && frozenMiniIdsRef.current) {
      return applyFrozenOrder(naturalOpen, frozenMiniIdsRef.current);
    }
    frozenMiniIdsRef.current = naturalOpen.map((x) => x.item.id);
    return naturalOpen;
  }, [naturalOpen, miniHovered]);

  const drawerLocked = drawerHovered || editingId !== null || editingTitleId !== null;
  const drawerOrdered = useMemo(() => {
    if (drawerLocked && frozenDrawerIdsRef.current) {
      return applyFrozenOrder(naturalOpen, frozenDrawerIdsRef.current);
    }
    frozenDrawerIdsRef.current = naturalOpen.map((x) => x.item.id);
    return naturalOpen;
  }, [naturalOpen, drawerLocked]);

  const miniShown = miniOrdered.slice(0, MINI_MAX_ROWS);
  const miniHiddenCount = Math.max(0, miniOrdered.length - miniShown.length);

  const showFilterBar =
    naturalOpen.length > FILTER_BAR_THRESHOLD ||
    (doneExpanded && doneTotal > 0) ||
    searchQuery.trim() !== "" ||
    dotFilter !== "all";
  const { active: activeGroup, stale: staleGroup, filteringActive } = useMemo(
    () => filterAndPartitionWorkItems(drawerOrdered, searchQuery, dotFilter),
    [drawerOrdered, searchQuery, dotFilter]
  );

  // Smooth zero-bounce FLIP animation using scroll-independent offsetTop
  const miniOrderSig = miniShown.map((x) => x.item.id).join(",");
  const activeOrderSig = activeGroup.map((x) => x.item.id).join(",");

  useLayoutEffect(() => {
    const modeChanged = prevModeRef.current !== mode;
    prevModeRef.current = mode;

    const runFlip = (container: HTMLDivElement | null, prevTopsRef: { current: Map<string, number> }) => {
      if (!container) {
        prevTopsRef.current = new Map();
        return;
      }
      const nodes = Array.from(container.querySelectorAll<HTMLElement>("[data-flip-id]"));
      const nextTops = new Map<string, number>();
      for (const el of nodes) {
        nextTops.set(el.dataset.flipId!, el.offsetTop);
      }

      const prevTops = prevTopsRef.current;
      prevTopsRef.current = nextTops;

      if (modeChanged || prevTops.size === 0 || prefersReducedMotion()) {
        return;
      }

      const toAnimate: { el: HTMLElement; kind: "move" | "enter" }[] = [];
      for (const el of nodes) {
        const id = el.dataset.flipId!;
        const oldTop = prevTops.get(id);
        const newTop = nextTops.get(id)!;
        if (oldTop !== undefined) {
          const dy = oldTop - newTop;
          if (Math.abs(dy) > 0.5) {
            el.style.transition = "none";
            el.style.transform = `translateY(${dy}px)`;
            toAnimate.push({ el, kind: "move" });
          }
        } else {
          el.style.transition = "none";
          el.style.opacity = "0";
          el.style.transform = "translateY(-4px)";
          toAnimate.push({ el, kind: "enter" });
        }
      }
      if (toAnimate.length === 0) return;

      container.getBoundingClientRect();
      requestAnimationFrame(() => {
        for (const { el, kind } of toAnimate) {
          if (kind === "move") {
            el.style.transition = "transform 300ms var(--ease-quiet)";
            el.style.transform = "translateY(0)";
          } else {
            el.style.transition = "opacity 240ms var(--ease-quiet), transform 240ms var(--ease-quiet)";
            el.style.opacity = "";
            el.style.transform = "translateY(0)";
          }
          const cleanup = () => {
            el.style.transition = "";
            el.style.transform = "";
          };
          el.addEventListener("transitionend", cleanup, { once: true });
        }
      });
    };

    if (mode === "mini" && isDesktop) {
      runFlip(miniListRef.current, prevMiniTopsRef);
      prevActiveTopsRef.current = new Map();
    } else if (mode === "expanded") {
      runFlip(activeListRef.current, prevActiveTopsRef);
      prevMiniTopsRef.current = new Map();
    } else {
      prevMiniTopsRef.current = new Map();
      prevActiveTopsRef.current = new Map();
    }
  }, [mode, isDesktop, miniOrderSig, activeOrderSig]);

  // Lazy load archived ("已划掉") items ONLY when expanded
  const fetchClosedPage = async (offset: number, append: boolean, qStr: string) => {
    setDoneLoading(true);
    try {
      const qs = new URLSearchParams({
        status: "closed",
        limit: String(WORK_PAGE_SIZE),
        offset: String(offset),
      });
      if (thread) qs.set("thread", thread);
      if (qStr.trim()) qs.set("q", qStr.trim());
      const res = await api<{ work_items: WorkItem[]; total: number; has_more: boolean }>(
        `/api/super/tasks?${qs.toString()}`
      );
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
    const timer = window.setTimeout(() => {
      void fetchClosedPage(0, false, searchQuery);
    }, searchQuery.trim() ? 180 : 0);
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
      await api("/api/super/tasks", {
        id: item.id,
        title: trimmed,
        user_edited_title: true,
      });
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
      await api("/api/super/tasks", {
        id: item.id,
        next_step: trimmed,
        clear_next_step: trimmed === "",
      });
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
    setExitingIds((prev) => {
      const next = new Set(prev);
      next.add(item.id);
      return next;
    });
    window.setTimeout(() => {
      setOptimisticClosedIds((prev) => {
        const next = new Set(prev);
        next.add(item.id);
        return next;
      });
      setExitingIds((prev) => {
        const next = new Set(prev);
        next.delete(item.id);
        return next;
      });
      if (openItemId === item.id) setOpenItemId(null);
      if (editingId === item.id) setEditingId(null);
      if (frozenMiniIdsRef.current) {
        frozenMiniIdsRef.current = frozenMiniIdsRef.current.filter((id) => id !== item.id);
      }
      if (frozenDrawerIdsRef.current) {
        frozenDrawerIdsRef.current = frozenDrawerIdsRef.current.filter((id) => id !== item.id);
      }
      void api(`/api/super/tasks/${encodeURIComponent(item.id)}/done`, { note: "已确认完成" })
        .then(() => {
          onToast(`已划掉「${item.title}」（底部可恢复）`);
        })
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

  const renderAccordionRow = (entry: EnrichedWorkItem) => {
    const { item, derived, statusText, linkedWorkers } = entry;
    const isOpen = openItemId === item.id;
    const isEditing = editingId === item.id;
    const isEditingTitle = editingTitleId === item.id;
    const isExiting = exitingIds.has(item.id);
    const isPulsing = pulsingIds.has(item.id);
    const age = formatShortAge(item.updated_at || item.created_at);

    return (
      <div
        key={item.id}
        data-flip-id={item.id}
        onClick={() => {
          setOpenItemId((cur) => (cur === item.id ? null : item.id));
          setEditingId(null);
          setEditingTitleId(null);
        }}
        className={`notebook-acc-row px-4 py-2.5 transition-colors cursor-pointer ${
          isOpen ? "is-open surface-rail" : "surface-hover"
        } ${isExiting ? "notebook-row-exiting" : ""}`}
      >
        <div className="flex items-start justify-between gap-2.5">
          {isOpen && isEditingTitle ? (
            <div className="flex-1 flex items-center gap-1.5" onClick={(e) => e.stopPropagation()}>
              <input
                type="text"
                autoFocus
                value={titleDraft}
                onChange={(e) => setTitleDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Escape") {
                    e.preventDefault();
                    setEditingTitleId(null);
                  } else if (e.key === "Enter") {
                    e.preventDefault();
                    void saveTitle(item);
                  }
                }}
                className="flex-1 px-2 py-0.5 rounded-md surface-bg border border-strong text-[12px] outline-none"
              />
              <button
                type="button"
                disabled={savingId === item.id}
                onClick={() => void saveTitle(item)}
                className="px-2 py-0.5 rounded-md btn-accent text-[11px] font-medium cursor-pointer disabled:opacity-50"
              >
                存
              </button>
            </div>
          ) : (
            <div className="min-w-0 flex-1 flex items-start gap-1.5">
              <span
                className={`text-[12.5px] ${isOpen ? "whitespace-normal break-words leading-snug" : "truncate"} ${
                  derived === "failed" || derived === "blocked" ? "font-medium text-primary" : "text-primary"
                }`}
                title={item.title}
              >
                {item.title}
              </span>
              {isOpen && (
                <button
                  type="button"
                  onClick={(e) => {
                    e.stopPropagation();
                    setEditingTitleId(item.id);
                    setTitleDraft(item.title);
                  }}
                  className="mt-0.5 p-0.5 rounded text-muted hover:text-primary cursor-pointer shrink-0"
                  title="修改标题"
                >
                  <Pencil className="w-3 h-3" strokeWidth={1.8} />
                </button>
              )}
            </div>
          )}
          <div className="flex items-center gap-2 shrink-0">
            {!isOpen && (
              <button
                type="button"
                onClick={(e) => {
                  e.stopPropagation();
                  markDone(item);
                }}
                className="notebook-quick-hover px-1.5 py-0.5 rounded border border-hairline surface-bg text-[10.5px] text-secondary hover:opacity-80 cursor-pointer"
                title="划掉"
              >
                <Check className="w-2.5 h-2.5" strokeWidth={2.2} />
              </button>
            )}
            {age && <span className="text-[10.5px] text-muted">{age}</span>}
            <span
              className={`notebook-dot w-1.5 h-1.5 rounded-full shrink-0 ${
                derived === "working" ? "notebook-dot-pulse" : ""
              } ${isPulsing ? "is-attn-once" : ""}`}
              style={{ background: dotBg(derived) }}
              title={statusText}
            />
          </div>
        </div>

        {!isOpen && item.next_step && (
          <div className="text-[11px] text-muted truncate mt-0.5">{item.next_step}</div>
        )}

        {isOpen && (
          <div className="notebook-acc-body mt-2 space-y-2" onClick={(e) => e.stopPropagation()}>
            {isEditing ? (
              <div className="flex items-center gap-1.5">
                <input
                  ref={editInputRef}
                  type="text"
                  value={editDraft}
                  onChange={(e) => setEditDraft(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Escape") {
                      e.preventDefault();
                      setEditingId(null);
                    } else if (e.key === "Enter") {
                      e.preventDefault();
                      void saveNextStep(item);
                    }
                  }}
                  placeholder="下一步（留空可清除）…"
                  className="flex-1 px-2 py-1 rounded-md surface-bg border border-strong text-[11.5px] outline-none"
                />
                <button
                  type="button"
                  disabled={savingId === item.id}
                  onClick={() => void saveNextStep(item)}
                  className="px-2 py-1 rounded-md btn-accent text-[11px] font-medium cursor-pointer disabled:opacity-50"
                >
                  存
                </button>
              </div>
            ) : (
              <div
                onClick={() => {
                  setEditingId(item.id);
                  setEditDraft(item.next_step ?? "");
                }}
                className="text-[11.5px] leading-relaxed text-secondary hover:underline cursor-pointer"
                title="点击修改下一步"
              >
                {item.next_step || "点击补充下一步…"}
              </div>
            )}

            {(linkedWorkers.length > 0 ||
              (item.branches?.length ?? 0) > 0 ||
              (item.snapshots?.length ?? 0) > 0 ||
              (item.artifacts?.length ?? 0) > 0) && (
              <div className="flex flex-wrap gap-1">
                {linkedWorkers.map((w) => (
                  <span
                    key={w.name}
                    className="inline-flex items-center rounded border border-hairline surface-rail text-[10.5px] font-mono-code text-secondary"
                  >
                    <button
                      type="button"
                      onClick={() => onOpenWorker(w.name)}
                      className="inline-flex items-center gap-1 px-1.5 py-0.5 surface-hover cursor-pointer"
                    >
                      <span className="w-1.5 h-1.5 rounded-full shrink-0" style={{ background: dotBg(w.status) }} />
                      <span>{w.name}</span>
                    </button>
                    <button
                      type="button"
                      disabled={savingId === item.id}
                      onClick={(e) => {
                        e.stopPropagation();
                        void unbindWorker(item, w.name);
                      }}
                      title={`解绑 ${w.name}（保留历史，不关闭会话）`}
                      className="px-1 py-0.5 text-muted hover:text-primary border-l border-hairline cursor-pointer disabled:opacity-40"
                    >
                      <X className="w-2.5 h-2.5" strokeWidth={2} />
                    </button>
                  </span>
                ))}
                {(item.branches ?? []).map((br) => (
                  <span
                    key={br}
                    className="px-1.5 py-0.5 rounded border border-hairline text-[10.5px] font-mono-code text-muted"
                  >
                    ⎇ {br}
                  </span>
                ))}
                {(() => {
                  type ChipEntry = {
                    key: string;
                    name: string;
                    stamp: string;
                    expired?: boolean;
                    onClick?: () => void;
                    title: string;
                  };
                  const snapIds = new Set<string>();
                  const allChips: ChipEntry[] = [];
                  for (const snap of [...(item.snapshots ?? [])].reverse()) {
                    snapIds.add(snap.id);
                    allChips.push({
                      key: snap.id,
                      name: snap.name,
                      stamp: "",
                      expired: snap.expired,
                      title: snap.expired ? "快照已过期" : `${snap.name}（Hub 快照）`,
                      onClick: snap.expired
                        ? undefined
                        : () =>
                            onOpenArtifact?.({
                              artifactId: snap.id,
                              name: snap.name,
                              kind: snap.kind,
                              shareToken: snap.share_token,
                            }),
                    });
                  }
                  for (const art of [...(item.artifacts ?? [])].reverse()) {
                    if (art.artifact_id && snapIds.has(art.artifact_id)) continue;
                    const nm = art.name || artifactBasename(art.path);
                    allChips.push({
                      key: art.artifact_id || `${art.machine ?? ""}:${art.path}:${art.delivered_at ?? ""}`,
                      name: nm,
                      stamp: formatArtifactStamp(art.delivered_at),
                      title: art.path,
                      onClick: () =>
                        art.artifact_id && onOpenArtifact
                          ? onOpenArtifact({
                              artifactId: art.artifact_id,
                              name: nm,
                              kind: art.kind || "text",
                            })
                          : onOpenFile(art.path, art.machine),
                    });
                  }
                  const nameCounts = new Map<string, number>();
                  for (const c of allChips) {
                    nameCounts.set(c.name, (nameCounts.get(c.name) ?? 0) + 1);
                  }
                  const artsExpanded = expandedArtsIds.has(item.id);
                  const visibleChips = artsExpanded ? allChips : allChips.slice(0, 3);
                  const hiddenChips = Math.max(0, allChips.length - visibleChips.length);
                  return (
                    <>
                      {visibleChips.map((c, idx) => {
                        const dup = (nameCounts.get(c.name) ?? 0) > 1;
                        const label = dup
                          ? c.stamp
                            ? `${c.name} · ${c.stamp}`
                            : `${c.name} #${allChips.length - idx}`
                          : c.name;
                        if (c.expired || !c.onClick) {
                          return (
                            <span
                              key={c.key}
                              title={c.title}
                              className="px-1.5 py-0.5 rounded border border-hairline text-[10.5px] font-mono-code text-muted line-through"
                            >
                              📄 {label}
                            </span>
                          );
                        }
                        return (
                          <button
                            key={c.key}
                            type="button"
                            onClick={c.onClick}
                            title={c.title}
                            className="px-1.5 py-0.5 rounded border border-strong surface-hover text-[10.5px] font-mono-code text-primary cursor-pointer"
                          >
                            📄 {label}
                          </button>
                        );
                      })}
                      {hiddenChips > 0 && (
                        <button
                          type="button"
                          onClick={() =>
                            setExpandedArtsIds((prev) => {
                              const next = new Set(prev);
                              next.add(item.id);
                              return next;
                            })
                          }
                          className="px-1.5 py-0.5 rounded border border-hairline text-[10.5px] text-muted surface-hover cursor-pointer"
                        >
                          +{hiddenChips} 历史版本
                        </button>
                      )}
                    </>
                  );
                })()}
              </div>
            )}

            <div className="flex items-center justify-between gap-2 pt-1">
              <span
                className="text-[10.5px]"
                style={{ color: dotBg(derived) }}
              >
                {statusText}
              </span>
              <div className="flex items-center gap-1.5">
                <button
                  type="button"
                  onClick={() => {
                    onAskCorgi(item);
                    if (!isDesktop) onModeChange("hidden");
                  }}
                  className="px-2 py-0.5 rounded-md border border-hairline surface-bg surface-hover text-[11px] text-secondary cursor-pointer"
                >
                  问 Corgi
                </button>
                <button
                  type="button"
                  onClick={() => markDone(item)}
                  className="inline-flex items-center gap-1 px-2 py-0.5 rounded-md border border-strong surface-bg surface-hover text-[11px] font-medium text-primary cursor-pointer"
                >
                  <Check className="w-3 h-3" strokeWidth={2.2} />
                  <span>划掉</span>
                </button>
              </div>
            </div>
          </div>
        )}
      </div>
    );
  };

  // STATE 1: Collapsed borderless right-margin micro-list (desktop only, stays invisible when 0 open items)
  if (mode === "mini" && isDesktop) {
    if (naturalOpen.length === 0) {
      return null;
    }
    return (
      <aside
        onMouseEnter={() => setMiniHovered(true)}
        onMouseLeave={() => setMiniHovered(false)}
        className="margin-notebook w-[216px] shrink-0 pt-5 pr-5 pl-2 flex flex-col justify-start select-none"
      >
        <div className="margin-chrome px-1.5 pb-1.5 flex items-center justify-between">
          <span className="text-[10.5px] text-muted font-mono-code">小本本 · {naturalOpen.length}</span>
          <button
            type="button"
            onClick={() => onModeChange("expanded")}
            className="w-5 h-5 rounded flex items-center justify-center text-muted surface-hover cursor-pointer"
            title="展开小本本"
          >
            <Maximize2 className="w-3 h-3" strokeWidth={1.8} />
          </button>
        </div>

        <div ref={miniListRef} className="relative space-y-0.5">
          {miniShown.map((entry) => {
            const { item, derived, statusText } = entry;
            const isPulsing = pulsingIds.has(item.id);
            return (
              <button
                key={item.id}
                type="button"
                data-flip-id={item.id}
                onClick={() => {
                  setOpenItemId(item.id);
                  setEditingId(null);
                  onModeChange("expanded");
                }}
                title={`${item.title}（${statusText}）${item.next_step ? `\n下一步：${item.next_step}` : ""}`}
                className={`margin-notebook-row w-full px-1.5 py-1.5 rounded-md flex items-center justify-between gap-3 text-left surface-hover cursor-pointer ${
                  derived === "blocked" ? "is-attn text-primary" : "text-secondary"
                }`}
              >
                <span className="text-[12px] leading-snug truncate">{item.title}</span>
                <span
                  className={`notebook-dot w-1.5 h-1.5 rounded-full shrink-0 ${
                    derived === "working" ? "notebook-dot-pulse" : ""
                  } ${isPulsing ? "is-attn-once" : ""}`}
                  style={{ background: dotBg(derived) }}
                />
              </button>
            );
          })}
        </div>

        {miniHiddenCount > 0 && (
          <button
            type="button"
            onClick={() => onModeChange("expanded")}
            className="mt-1.5 px-1.5 py-1 rounded-md text-left text-[11px] text-muted surface-hover cursor-pointer transition-colors"
          >
            还有 {miniHiddenCount} 项 →
          </button>
        )}
      </aside>
    );
  }

  // STATE 2: Hidden
  if (mode !== "expanded") {
    return null;
  }

  // STATE 3: Expanded 340px right drawer (inline on desktop, slide-over on narrow viewports)
  const staleShown = staleGroup.slice(0, staleLimit);
  const staleRemaining = Math.max(0, staleGroup.length - staleShown.length);
  const doneRemaining = Math.max(0, doneTotal - doneItems.length);

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
            <div
              className="inline-flex items-center gap-0.5 p-0.5 rounded-lg border border-hairline surface-rail shrink-0"
              title="按圆点状态筛选"
            >
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
                    st === "failed"
                      ? "仅看执行受阻"
                      : st === "blocked"
                        ? "仅看待拍板"
                        : st === "working"
                          ? "仅看运行中"
                          : st === "review"
                            ? "仅看待审阅"
                            : "仅看待推进"
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
              activeGroup.map(renderAccordionRow)
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
                  <div className="divide-y divide-[var(--line)] border-t border-hairline">
                    {staleShown.map(renderAccordionRow)}
                  </div>
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

          {(doneTotal > 0 || doneExpanded) && (
            <div className="border-t border-hairline">
              <button
                type="button"
                onClick={() => setDoneExpanded((v) => !v)}
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
                      <div
                        key={item.id}
                        className="px-2 py-1.5 rounded-lg flex items-start justify-between gap-2 text-[11.5px] surface-hover"
                      >
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
                          onClick={() => void reopenItem(item)}
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
                      onClick={() => void fetchClosedPage(doneItems.length, true, searchQuery)}
                      className="w-full py-1.5 mt-1 rounded-lg border border-hairline text-center text-[11px] text-muted surface-hover cursor-pointer disabled:opacity-50"
                    >
                      {doneLoading
                        ? "加载中…"
                        : `加载更多已划掉${doneRemaining > 0 ? `（还有 ${doneRemaining} 项）` : ""}`}
                    </button>
                  )}
                </div>
              )}
            </div>
          )}
        </div>
      </aside>
    </>
  );
}

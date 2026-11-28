import type { RefObject } from "react";
import { Check, Pencil, X } from "lucide-react";
import { artifactBasename, formatShortAge, type DerivedWorkStatus, type EnrichedWorkItem } from "../../lib/workItems";
import type { FileDrawerTarget, WorkItem } from "../../types/api";

export function dotBg(derived: DerivedWorkStatus): string {
  if (derived === "failed") return "var(--err)";
  if (derived === "blocked") return "var(--attn)";
  if (derived === "working") return "var(--ok)";
  if (derived === "review") return "var(--accent)";
  return "var(--fg-3)";
}

export function formatArtifactStamp(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (!Number.isFinite(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

interface Props {
  entry: EnrichedWorkItem;
  isOpen: boolean;
  isEditing: boolean;
  isEditingTitle: boolean;
  isExiting: boolean;
  isPulsing: boolean;
  isSaving: boolean;
  artsExpanded: boolean;
  isDesktop: boolean;
  editDraft: string;
  titleDraft: string;
  editInputRef: RefObject<HTMLInputElement | null>;
  onToggleOpen: () => void;
  onStartEditTitle: () => void;
  onCancelEditTitle: () => void;
  onChangeTitleDraft: (v: string) => void;
  onSaveTitle: (item: WorkItem) => void;
  onStartEditNextStep: () => void;
  onCancelEditNextStep: () => void;
  onChangeEditDraft: (v: string) => void;
  onSaveNextStep: (item: WorkItem) => void;
  onExpandArts: () => void;
  onOpenWorker: (name: string) => void;
  onUnbindWorker: (item: WorkItem, workerName: string) => void;
  onOpenFile: (path: string, machine?: string) => void;
  onOpenArtifact?: (target: FileDrawerTarget) => void;
  onAskCorgi: (item: WorkItem) => void;
  onHideMobile: () => void;
  onMarkDone: (item: WorkItem) => void;
}

export function WorkNotebookRow({
  entry,
  isOpen,
  isEditing,
  isEditingTitle,
  isExiting,
  isPulsing,
  isSaving,
  artsExpanded,
  isDesktop,
  editDraft,
  titleDraft,
  editInputRef,
  onToggleOpen,
  onStartEditTitle,
  onCancelEditTitle,
  onChangeTitleDraft,
  onSaveTitle,
  onStartEditNextStep,
  onCancelEditNextStep,
  onChangeEditDraft,
  onSaveNextStep,
  onExpandArts,
  onOpenWorker,
  onUnbindWorker,
  onOpenFile,
  onOpenArtifact,
  onAskCorgi,
  onHideMobile,
  onMarkDone,
}: Props) {
  const { item, derived, statusText, linkedWorkers } = entry;
  const age = formatShortAge(item.updated_at || item.created_at);

  type ChipEntry = { key: string; name: string; stamp: string; expired?: boolean; onClick?: () => void; title: string };
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
        : () => onOpenArtifact?.({ artifactId: snap.id, name: snap.name, kind: snap.kind, shareToken: snap.share_token }),
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
          ? onOpenArtifact({ artifactId: art.artifact_id, name: nm, kind: art.kind || "text" })
          : onOpenFile(art.path, art.machine),
    });
  }
  const nameCounts = new Map<string, number>();
  for (const c of allChips) nameCounts.set(c.name, (nameCounts.get(c.name) ?? 0) + 1);
  const visibleChips = artsExpanded ? allChips : allChips.slice(0, 3);
  const hiddenChips = Math.max(0, allChips.length - visibleChips.length);

  return (
    <div
      data-flip-id={item.id}
      onClick={onToggleOpen}
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
              onChange={(e) => onChangeTitleDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Escape") {
                  e.preventDefault();
                  onCancelEditTitle();
                } else if (e.key === "Enter") {
                  e.preventDefault();
                  onSaveTitle(item);
                }
              }}
              className="flex-1 px-2 py-0.5 rounded-md surface-bg border border-strong text-[12px] outline-none"
            />
            <button
              type="button"
              disabled={isSaving}
              onClick={() => onSaveTitle(item)}
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
                  onStartEditTitle();
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
                onMarkDone(item);
              }}
              className="notebook-quick-hover px-1.5 py-0.5 rounded border border-hairline surface-bg text-[10.5px] text-secondary hover:opacity-80 cursor-pointer"
              title="划掉"
            >
              <Check className="w-2.5 h-2.5" strokeWidth={2.2} />
            </button>
          )}
          {age && <span className="text-[10.5px] text-muted">{age}</span>}
          <span
            className={`notebook-dot w-1.5 h-1.5 rounded-full shrink-0 ${derived === "working" ? "notebook-dot-pulse" : ""} ${
              isPulsing ? "is-attn-once" : ""
            }`}
            style={{ background: dotBg(derived) }}
            title={statusText}
          />
        </div>
      </div>

      {!isOpen && (item.next_step || item.latest_update) && (
        <div className="text-[11px] text-muted truncate mt-0.5">{item.next_step || item.latest_update}</div>
      )}

      {isOpen && (
        <div className="notebook-acc-body mt-2 space-y-2" onClick={(e) => e.stopPropagation()}>
          {isEditing ? (
            <div className="flex items-center gap-1.5">
              <input
                ref={editInputRef}
                type="text"
                value={editDraft}
                onChange={(e) => onChangeEditDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Escape") {
                    e.preventDefault();
                    onCancelEditNextStep();
                  } else if (e.key === "Enter") {
                    e.preventDefault();
                    onSaveNextStep(item);
                  }
                }}
                placeholder="下一步（留空可清除）…"
                className="flex-1 px-2 py-1 rounded-md surface-bg border border-strong text-[11.5px] outline-none"
              />
              <button
                type="button"
                disabled={isSaving}
                onClick={() => onSaveNextStep(item)}
                className="px-2 py-1 rounded-md btn-accent text-[11px] font-medium cursor-pointer disabled:opacity-50"
              >
                存
              </button>
            </div>
          ) : (
            <div
              onClick={onStartEditNextStep}
              className="text-[11.5px] leading-relaxed text-secondary hover:underline cursor-pointer"
              title="点击修改下一步"
            >
              {item.next_step || "点击补充下一步…"}
            </div>
          )}
          {item.latest_update && item.latest_update !== item.next_step && (
            <div className="text-[11px] leading-relaxed text-muted">
              <span className="text-secondary">最新巡检</span>
              {item.latest_update_at ? ` · ${formatShortAge(item.latest_update_at)}` : ""}：{item.latest_update}
            </div>
          )}

          {(linkedWorkers.length > 0 || (item.branches?.length ?? 0) > 0 || allChips.length > 0) && (
            <div className="flex flex-wrap gap-1">
              {linkedWorkers.map((w) => (
                <span
                  key={w.name}
                  className="inline-flex items-center rounded border border-hairline surface-rail text-[10.5px] font-mono-code text-secondary"
                >
                  <button
                    type="button"
                    onClick={() => onOpenWorker(w.name)}
                    title={`进入 #${w.name} 直聊`}
                    className="inline-flex items-center gap-1 px-1.5 py-0.5 surface-hover cursor-pointer"
                  >
                    <span className="w-1.5 h-1.5 rounded-full shrink-0" style={{ background: dotBg(w.status) }} />
                    <span>{w.name}</span>
                    <span className="text-muted text-[9.5px]" aria-hidden="true">
                      ↗
                    </span>
                  </button>
                  <button
                    type="button"
                    disabled={isSaving}
                    onClick={(e) => {
                      e.stopPropagation();
                      onUnbindWorker(item, w.name);
                    }}
                    title={`解绑 ${w.name}（保留历史，不关闭会话）`}
                    className="px-1 py-0.5 text-muted hover:text-primary border-l border-hairline cursor-pointer disabled:opacity-40"
                  >
                    <X className="w-2.5 h-2.5" strokeWidth={2} />
                  </button>
                </span>
              ))}
              {(item.branches ?? []).map((br) => (
                <span key={br} className="px-1.5 py-0.5 rounded border border-hairline text-[10.5px] font-mono-code text-muted">
                  ⎇ {br}
                </span>
              ))}
              {visibleChips.map((c, idx) => {
                const dup = (nameCounts.get(c.name) ?? 0) > 1;
                const label = dup ? (c.stamp ? `${c.name} · ${c.stamp}` : `${c.name} #${allChips.length - idx}`) : c.name;
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
                  onClick={onExpandArts}
                  className="px-1.5 py-0.5 rounded border border-hairline text-[10.5px] text-muted surface-hover cursor-pointer"
                >
                  +{hiddenChips} 历史版本
                </button>
              )}
            </div>
          )}

          <div className="flex items-center justify-between gap-2 pt-1">
            <span className="text-[10.5px]" style={{ color: dotBg(derived) }}>
              {statusText}
            </span>
            <div className="flex items-center gap-1.5">
              <button
                type="button"
                onClick={() => {
                  onAskCorgi(item);
                  if (!isDesktop) onHideMobile();
                }}
                className="px-2 py-0.5 rounded-md border border-hairline surface-bg surface-hover text-[11px] text-secondary cursor-pointer"
              >
                问 Corgi
              </button>
              <button
                type="button"
                onClick={() => onMarkDone(item)}
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
}

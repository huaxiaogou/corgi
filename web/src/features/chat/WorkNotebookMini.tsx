import type { RefObject } from "react";
import { Maximize2 } from "lucide-react";
import type { EnrichedWorkItem } from "../../lib/workItems";
import { dotBg } from "./WorkNotebookRow";

interface Props {
  totalCount: number;
  miniShown: EnrichedWorkItem[];
  miniHiddenCount: number;
  pulsingIds: Set<string>;
  miniListRef: RefObject<HTMLDivElement | null>;
  onHoverChange: (hovered: boolean) => void;
  onExpand: () => void;
  onSelectItem: (id: string) => void;
}

export function WorkNotebookMini({
  totalCount,
  miniShown,
  miniHiddenCount,
  pulsingIds,
  miniListRef,
  onHoverChange,
  onExpand,
  onSelectItem,
}: Props) {
  if (totalCount === 0) return null;
  return (
    <aside
      onMouseEnter={() => onHoverChange(true)}
      onMouseLeave={() => onHoverChange(false)}
      className="margin-notebook w-[216px] shrink-0 pt-5 pr-5 pl-2 flex flex-col justify-start select-none"
    >
      <div className="margin-chrome px-1.5 pb-1.5 flex items-center justify-between">
        <span className="text-[10.5px] text-muted font-mono-code">小本本 · {totalCount}</span>
        <button
          type="button"
          onClick={onExpand}
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
              onClick={() => onSelectItem(item.id)}
              title={`${item.title}（${statusText}）${item.next_step ? `\n下一步：${item.next_step}` : ""}`}
              className={`margin-notebook-row w-full px-1.5 py-1.5 rounded-md flex items-center justify-between gap-3 text-left surface-hover cursor-pointer ${
                derived === "blocked" ? "is-attn text-primary" : "text-secondary"
              }`}
            >
              <span className="text-[12px] leading-snug truncate">{item.title}</span>
              <span
                className={`notebook-dot w-1.5 h-1.5 rounded-full shrink-0 ${derived === "working" ? "notebook-dot-pulse" : ""} ${
                  isPulsing ? "is-attn-once" : ""
                }`}
                style={{ background: dotBg(derived) }}
              />
            </button>
          );
        })}
      </div>

      {miniHiddenCount > 0 && (
        <button
          type="button"
          onClick={onExpand}
          className="mt-1.5 px-1.5 py-1 rounded-md text-left text-[11px] text-muted surface-hover cursor-pointer transition-colors"
        >
          还有 {miniHiddenCount} 项 →
        </button>
      )}
    </aside>
  );
}

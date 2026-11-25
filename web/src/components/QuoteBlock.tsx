import { useState } from "react";
import type { Quote } from "../types/api";

interface Props {
  quote: Quote;
  align?: "left" | "right";
  onJumpToEntry?: (entryId: string) => void;
}

export function QuoteBlock({ quote, align = "left", onJumpToEntry }: Props) {
  const [expanded, setExpanded] = useState(false);
  const text = (quote.text ?? "").trim();
  if (!text) return null;

  const isLong = text.length > 48 || text.includes("\n");
  const canJump = Boolean(quote.entry && onJumpToEntry);
  const containerCls =
    align === "right"
      ? "max-w-[82%] rounded-xl surface-rail border border-hairline px-3 py-1.5 text-left space-y-1"
      : "border-l-2 border-strong pl-2.5 py-0.5 space-y-1";

  return (
    <div className={containerCls}>
      <div className="flex items-center justify-between gap-2 text-[11px] text-muted">
        <span className="truncate font-medium">引用 · {quote.who}</span>
        <div className="flex items-center gap-2 shrink-0">
          {isLong && (
            <button
              type="button"
              onClick={() => setExpanded((v) => !v)}
              className="hover:text-primary hover:underline cursor-pointer"
            >
              {expanded ? "收起" : "展开"}
            </button>
          )}
          {canJump && (
            <button
              type="button"
              onClick={() => onJumpToEntry?.(quote.entry!)}
              className="hover:text-primary hover:underline cursor-pointer"
            >
              定位 ↑
            </button>
          )}
        </div>
      </div>
      <p
        className={`text-[12px] leading-relaxed text-secondary whitespace-pre-wrap break-words ${
          !expanded && isLong ? "line-clamp-2" : ""
        }`}
      >
        {text}
      </p>
    </div>
  );
}

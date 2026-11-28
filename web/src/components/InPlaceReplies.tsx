import { useState } from "react";
import { ChevronDown, ChevronUp, CornerDownRight } from "lucide-react";
import type { Action, Entry } from "../types/api";

interface Props {
  replies: Entry[];
  resolvedActions: Record<string, Action>;
  highlightedEntryId: string;
}

const COMPACT_TEXT_LIMIT = 140;

function isCompactFoldable(text: string): boolean {
  return text.length > COMPACT_TEXT_LIMIT || text.split("\n").length > 2;
}

export function InPlaceReplies({ replies, resolvedActions, highlightedEntryId }: Props) {
  const [expandedIds, setExpandedIds] = useState<Record<string, boolean>>({});

  if (replies.length === 0) return null;

  return (
    <div className="pt-1.5 space-y-1.5" data-in-place-replies="true">
      {replies.map((reply) => {
        const isHighlighted = highlightedEntryId === reply.id;
        const resolvedAct = reply.action_id ? resolvedActions[reply.action_id] : undefined;
        const foldable = isCompactFoldable(reply.text);
        const expanded = isHighlighted || Boolean(expandedIds[reply.id]);
        const displayText = !foldable || expanded ? reply.text : `${reply.text.slice(0, COMPACT_TEXT_LIMIT).trimEnd()}…`;
        const timeStr = new Date(reply.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

        return (
          <div
            key={reply.id}
            id={`entry-${reply.id}`}
            data-entry-id={reply.id}
            data-in-place-entry="true"
            className={`rounded-xl border border-hairline surface-rail px-3 py-2 text-[12.5px] leading-relaxed transition-colors ${
              isHighlighted ? "entry-highlight" : ""
            }`}
          >
            <div className="flex items-start gap-2">
              <CornerDownRight className="w-3.5 h-3.5 text-muted shrink-0 mt-0.5" strokeWidth={1.8} />
              <div className="min-w-0 flex-1 space-y-0.5">
                <div className="flex items-center justify-between gap-2 text-[11px] text-muted">
                  <span className="font-medium text-secondary">
                    {resolvedAct ? "你 · 自定义拍板（原位）" : "你 · 原位补充"}
                  </span>
                  <span className="font-mono-code shrink-0">{timeStr}</span>
                </div>
                <div className="text-primary whitespace-pre-wrap break-words">{displayText}</div>
                {foldable && !isHighlighted && (
                  <button
                    type="button"
                    onClick={() =>
                      setExpandedIds((prev) => ({
                        ...prev,
                        [reply.id]: !prev[reply.id],
                      }))
                    }
                    className="inline-flex items-center gap-0.5 text-[11px] text-secondary hover:text-primary cursor-pointer pt-0.5"
                  >
                    {expanded ? (
                      <>
                        <span>收起</span>
                        <ChevronUp className="w-3 h-3" />
                      </>
                    ) : (
                      <>
                        <span>展开全文</span>
                        <ChevronDown className="w-3 h-3" />
                      </>
                    )}
                  </button>
                )}
              </div>
            </div>
          </div>
        );
      })}
    </div>
  );
}

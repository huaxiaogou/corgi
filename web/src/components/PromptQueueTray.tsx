import { X } from "lucide-react";
import type { QueuedPrompt } from "../types/api";

interface Props {
  items: QueuedPrompt[];
  paused?: boolean;
  busyId?: string;
  onSteer: (item: QueuedPrompt) => void;
  onCancel: (item: QueuedPrompt) => void;
  onResume?: () => void;
}

export function PromptQueueTray({ items, paused, busyId, onSteer, onCancel, onResume }: Props) {
  if (items.length === 0) return null;
  return (
    <div className="max-w-[740px] mx-auto rounded-2xl surface-rail border border-hairline overflow-hidden">
      <div className="px-3.5 py-1.5 border-b border-hairline flex items-center justify-between gap-2 text-xs text-muted">
        <span>待发送队列 · {items.length} 条</span>
        {paused && onResume ? (
          <div className="flex items-center gap-2">
            <span>已暂停自动发送</span>
            <button
              type="button"
              onClick={onResume}
              className="text-primary font-medium hover:underline cursor-pointer"
            >
              继续
            </button>
          </div>
        ) : (
          <span>空闲后逐条发送</span>
        )}
      </div>
      <div className="divide-y divide-[var(--line)]">
        {items.map((item, idx) => {
          const disabled = busyId === item.id;
          return (
            <div key={item.id} className="px-3.5 py-2 flex items-center justify-between gap-2.5 text-xs">
              <div className="flex items-center gap-2 min-w-0">
                <span className="font-mono-code text-muted shrink-0">{idx + 1}.</span>
                <span className="text-primary truncate" title={item.text}>
                  {item.text}
                </span>
              </div>
              <div className="flex items-center gap-1.5 shrink-0">
                <button
                  type="button"
                  disabled={disabled}
                  onClick={() => onSteer(item)}
                  className="px-2.5 py-1 rounded-lg border border-strong surface-bg text-primary font-medium surface-hover cursor-pointer disabled:opacity-50"
                >
                  引导
                </button>
                <button
                  type="button"
                  disabled={disabled}
                  onClick={() => onCancel(item)}
                  title="取消排队"
                  className="w-6 h-6 rounded-md flex items-center justify-center text-muted hover:text-primary cursor-pointer disabled:opacity-50"
                >
                  <X className="w-3.5 h-3.5" />
                </button>
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

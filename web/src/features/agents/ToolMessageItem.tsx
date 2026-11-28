import type { ChatMessage } from "../../types/api";

interface Props {
  message: ChatMessage;
}

export function ToolMessageItem({ message }: Props) {
  const sub = message.subagent;
  if (sub) {
    const showModel = Boolean(sub.model && sub.model.toLowerCase() !== "inherit");
    const roleLabel = sub.role || message.name || "Subagent";
    const showSubInput = Boolean(message.input && message.input !== sub.role);

    return (
      <div className="pl-3 py-1.5 border-l-2 border-strong space-y-1 text-xs">
        <div className="flex flex-wrap items-center gap-1.5 min-w-0">
          <span className="font-mono-code text-[10.5px] uppercase tracking-wide text-muted shrink-0">
            Subagent
          </span>
          <span className="font-medium text-primary truncate">{roleLabel}</span>
          {sub.type && (
            <span className="px-1.5 py-0.5 rounded border border-hairline surface-rail font-mono-code text-[10.5px] text-secondary shrink-0">
              {sub.type}
            </span>
          )}
          {showModel && (
            <span className="px-1.5 py-0.5 rounded border border-hairline surface-rail font-mono-code text-[10.5px] text-muted shrink-0">
              {sub.model}
            </span>
          )}
        </div>
        {showSubInput && (
          <div className="text-secondary truncate">{message.input}</div>
        )}
        {sub.prompt && (
          <details className="text-muted">
            <summary className="cursor-pointer hover:text-secondary select-none">
              展开任务提示
            </summary>
            <pre className="mt-1.5 p-2.5 rounded-lg surface-rail border border-hairline font-mono-code text-[11px] whitespace-pre-wrap overflow-x-auto text-secondary">
              {sub.prompt}
            </pre>
          </details>
        )}
      </div>
    );
  }

  const summaryText = message.input || (message.output ? message.output.split("\n")[0] : "");
  const primaryBody = message.detail || message.input || "";
  const hasBody = Boolean(primaryBody || message.output);

  return (
    <details className="text-xs font-mono-code text-muted">
      <summary className="cursor-pointer truncate hover:text-secondary flex items-center gap-1.5">
        {message.error && (
          <span
            aria-label="工具执行失败"
            className="w-1.5 h-1.5 rounded-full shrink-0"
            style={{ background: "var(--attn)" }}
          />
        )}
        <span className="truncate">
          • {message.name} {summaryText}
        </span>
      </summary>
      {hasBody && (
        <pre className="mt-1.5 p-2.5 rounded-lg surface-rail border border-hairline text-[11px] whitespace-pre-wrap overflow-x-auto text-secondary">
          {primaryBody}
          {primaryBody && message.output ? "\n\n" : ""}
          {message.output ?? ""}
        </pre>
      )}
    </details>
  );
}

import { Terminal } from "lucide-react";
import { MarkdownText } from "../../components/MarkdownText";
import type { Approval, ChatMessage } from "../../types/api";

interface Props {
  messages: ChatMessage[];
  approval: Approval | null;
  setupHint: boolean;
  isAgent: boolean;
  onAnswerApproval: (choice: Approval["choices"][number]) => void;
  onCancelApproval: () => void;
  onSwitchToScreen: () => void;
  onOpenFile?: (path: string) => void;
}

export function AgentChatThread({
  messages,
  approval,
  setupHint,
  isAgent,
  onAnswerApproval,
  onCancelApproval,
  onSwitchToScreen,
  onOpenFile,
}: Props) {
  if (messages.length === 0 && !approval) {
    return (
      <div className="max-w-[740px] mx-auto py-16 text-center space-y-3">
        <p className="text-sm text-secondary">
          {setupHint ? "这个 Agent 正在终端里等待首次设置或登录确认" : isAgent ? "还没有对话，发一条消息开始吧。" : "这是一个纯终端会话。"}
        </p>
        {(setupHint || !isAgent) && (
          <button
            type="button"
            onClick={onSwitchToScreen}
            className="inline-flex items-center gap-1.5 px-3.5 py-2 rounded-xl text-xs font-medium btn-accent cursor-pointer"
          >
            <Terminal className="w-3.5 h-3.5" />
            <span>切换到「屏幕」查看并操作</span>
          </button>
        )}
      </div>
    );
  }

  return (
    <div className="max-w-[740px] mx-auto space-y-4">
      {messages.map((m, idx) => {
        if (m.kind === "user") {
          return (
            <div key={idx} className="flex justify-end">
              <div className="max-w-[80%] px-4 py-2.5 rounded-2xl rounded-br-sm text-[14px] pill-user whitespace-pre-wrap">
                {m.text}
              </div>
            </div>
          );
        }
        if (m.kind === "tool") {
          return (
            <details key={idx} className="text-xs font-mono-code text-muted">
              <summary className="cursor-pointer truncate hover:text-secondary">
                • {m.name} {m.input || (m.output ? m.output.split("\n")[0] : "")}
              </summary>
              {(m.input || m.output) && (
                <pre className="mt-1.5 p-2.5 rounded-lg surface-rail border border-hairline text-[11px] whitespace-pre-wrap overflow-x-auto text-secondary">
                  {m.input ? `${m.input}\n` : ""}
                  {m.output ?? ""}
                </pre>
              )}
            </details>
          );
        }
        return (
          <div key={idx} className="text-[14.5px]">
            <MarkdownText text={m.text ?? ""} onOpenFile={onOpenFile} />
          </div>
        );
      })}

      {approval && approval.choices.length > 0 && (
        <div className="card-floating rounded-2xl overflow-hidden">
          <div className="px-5 py-4 space-y-2">
            <div className="text-sm font-semibold">{approval.title}</div>
            {approval.command && (
              <pre className="p-3 rounded-xl text-xs font-mono-code surface-rail border border-hairline overflow-x-auto">
                {approval.command}
              </pre>
            )}
          </div>
          <div className="border-t border-hairline">
            {approval.choices.map((c, i) => (
              <button
                key={i}
                type="button"
                onClick={() => onAnswerApproval(c)}
                className="w-full px-5 py-3 flex items-center gap-3 text-left text-[13.5px] border-b border-hairline surface-hover cursor-pointer"
              >
                <span className="w-5 h-5 rounded border border-strong font-mono-code text-[11px] flex items-center justify-center text-secondary">
                  {c.key}
                </span>
                <span>{c.label}</span>
              </button>
            ))}
            <div className="px-5 py-2.5">
              <button
                type="button"
                onClick={onCancelApproval}
                className="px-2.5 py-1 rounded-lg text-xs border border-strong text-secondary surface-hover cursor-pointer"
              >
                Esc 取消
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

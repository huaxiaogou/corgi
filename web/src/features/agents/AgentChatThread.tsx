import { Terminal } from "lucide-react";
import { AgentGlyph, StatusDot } from "../../components/CorgiMark";
import { MarkdownText } from "../../components/MarkdownText";
import { formatAgentSpeakerLabel } from "../../lib/agentIdentity";
import type { Approval, ChatMessage } from "../../types/api";
import { ToolMessageItem } from "./ToolMessageItem";

export interface PendingPrompt {
  id: string;
  text: string;
  afterAbsIdx: number;
  sending?: boolean;
  steered?: boolean;
}

interface Props {
  messages: ChatMessage[];
  pendingPrompts?: PendingPrompt[];
  earlierCursor: number;
  loadingEarlier: boolean;
  approval: Approval | null;
  liveStatus: string;
  liveTail: string;
  liveMeta: string;
  setupHint: boolean;
  isAgent: boolean;
  agentName?: string;
  agentKind?: string;
  agentMachine?: string;
  onLoadEarlier: () => void;
  onPickAskOption: (opt: string) => void;
  onAnswerApproval: (choice: Approval["choices"][number], index: number) => void;
  onCancelApproval: () => void;
  onSwitchToScreen: () => void;
  onOpenFile?: (path: string, machine?: string) => void;
}

export function AgentChatThread({
  messages,
  pendingPrompts = [],
  earlierCursor,
  loadingEarlier,
  approval,
  liveStatus,
  liveTail,
  liveMeta,
  setupHint,
  isAgent,
  agentName,
  agentKind,
  agentMachine,
  onLoadEarlier,
  onPickAskOption,
  onAnswerApproval,
  onCancelApproval,
  onSwitchToScreen,
  onOpenFile,
}: Props) {
  const speakerLabel = formatAgentSpeakerLabel({
    name: agentName,
    kind: agentKind,
    machine: agentMachine,
    isAgent,
  });
  if (messages.length === 0 && pendingPrompts.length === 0 && !approval && liveStatus !== "working") {
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
      {earlierCursor > 0 && (
        <div className="text-center">
          <button
            type="button"
            disabled={loadingEarlier}
            onClick={onLoadEarlier}
            className="px-3.5 py-1 rounded-full text-xs border border-strong text-secondary surface-hover cursor-pointer disabled:opacity-50"
          >
            {loadingEarlier ? "加载中…" : "加载更早消息"}
          </button>
        </div>
      )}

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
        if (m.kind === "thinking") {
          return (
            <details key={idx} className="text-xs text-muted">
              <summary className="cursor-pointer hover:text-secondary select-none">
                思考过程
              </summary>
              <div className="mt-1.5 px-3 py-2 rounded-lg surface-rail border border-hairline text-[12px] text-secondary whitespace-pre-wrap leading-relaxed">
                {m.text}
              </div>
            </details>
          );
        }
        if (m.kind === "tool") {
          return <ToolMessageItem key={idx} message={m} />;
        }
        if (m.kind === "ask") {
          return (
            <div key={idx} className="card-floating rounded-2xl p-4 space-y-2.5">
              <div className="text-sm font-medium text-primary whitespace-pre-wrap">{m.text}</div>
              {m.options && m.options.length > 0 && (
                <div className="flex flex-wrap gap-2">
                  {m.options.map((opt) => (
                    <button
                      key={opt}
                      type="button"
                      onClick={() => onPickAskOption(opt)}
                      className="px-3 py-1.5 rounded-xl text-xs border border-strong surface-bg surface-hover cursor-pointer"
                    >
                      {opt}
                    </button>
                  ))}
                </div>
              )}
            </div>
          );
        }
        return (
          <div key={idx} className="flex items-start gap-3">
            <div className="w-7 h-7 rounded-lg border border-hairline surface-rail flex items-center justify-center shrink-0 mt-0.5 text-secondary">
              <AgentGlyph kind={agentKind || "agy"} isAgent={isAgent} className="w-4 h-4" />
            </div>
            <div className="flex-1 min-w-0 space-y-1">
              <div className="text-[11px] font-mono-code text-muted truncate">{speakerLabel}</div>
              <div className="text-[14.5px]">
                <MarkdownText text={m.text ?? ""} onOpenFile={onOpenFile} />
              </div>
            </div>
          </div>
        );
      })}

      {liveStatus === "working" && (
        <div className="flex items-center justify-between gap-2 px-3.5 py-2 rounded-xl surface-rail border border-hairline text-xs text-secondary">
          <div className="flex items-center gap-2 min-w-0">
            <StatusDot status="working" />
            <span className="truncate">{liveTail || "正在执行中…"}</span>
          </div>
          {liveMeta && <span className="font-mono-code text-[11px] text-muted shrink-0">{liveMeta}</span>}
        </div>
      )}

      {pendingPrompts.map((p) => (
        <div key={p.id} className="flex flex-col items-end gap-1">
          <div className="max-w-[80%] px-4 py-2.5 rounded-2xl rounded-br-sm text-[14px] pill-user opacity-80 whitespace-pre-wrap">
            {p.text}
          </div>
          <span className="text-[11px] text-muted pr-1">
            {p.sending
              ? "发送中…"
              : p.steered
                ? "已引导 · 等待 Agent 接手…"
                : liveStatus === "working"
                  ? "已发送 · 等待 Agent 接手…"
                  : "已发送 · 等待响应…"}
          </span>
        </div>
      ))}

      {approval && approval.choices.length > 0 && (
        <div className="card-floating rounded-2xl overflow-hidden">
          <div className="px-5 py-4 space-y-2">
            <div className="text-sm font-semibold">{approval.title}</div>
            {approval.detail && approval.detail !== approval.title && (
              <div className="text-xs text-secondary leading-relaxed whitespace-pre-wrap">{approval.detail}</div>
            )}
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
                onClick={() => onAnswerApproval(c, i)}
                className="w-full px-5 py-3 flex items-center gap-3 text-left text-[13.5px] border-b border-hairline surface-hover cursor-pointer"
              >
                <span className="w-5 h-5 rounded border border-strong font-mono-code text-[11px] flex items-center justify-center text-secondary shrink-0">
                  {c.key}
                </span>
                <span className={i === 0 ? "font-medium text-primary" : "text-primary"}>{c.label}</span>
                {c.active && <span className="ml-auto text-[11px] text-muted shrink-0">当前焦点</span>}
              </button>
            ))}
            <div className="px-5 py-2.5 flex items-center justify-between gap-2">
              <button type="button" onClick={onCancelApproval} className="px-2.5 py-1 rounded-lg text-xs border border-strong text-secondary surface-hover cursor-pointer">
                Esc 取消
              </button>
              <button type="button" onClick={onSwitchToScreen} className="text-xs text-muted hover:underline cursor-pointer">
                查看原样屏幕 →
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

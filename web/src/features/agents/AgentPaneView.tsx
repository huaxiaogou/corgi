import { useEffect, useState, type FormEvent } from "react";
import { ArrowUp, Trash2 } from "lucide-react";
import { MarkdownText } from "../../components/MarkdownText";
import { TerminalScreen } from "../../components/TerminalScreen";
import { api } from "../../lib/api";
import type { AgentView, Approval, ChatMessage } from "../../types/api";

interface Props {
  pane: AgentView;
  onClosed: () => void;
  onOpenFile?: (path: string) => void;
  onToast: (msg: string) => void;
}

export function AgentPaneView({ pane, onClosed, onOpenFile, onToast }: Props) {
  const [mode, setMode] = useState<"chat" | "screen" | "log">(pane.is_agent ? "chat" : "screen");
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [logText, setLogText] = useState("");
  const [approval, setApproval] = useState<Approval | null>(null);
  const [input, setInput] = useState("");

  useEffect(() => {
    void api("/api/seen", { pane: pane.pane_id });
  }, [pane.pane_id]);

  useEffect(() => {
    const url =
      mode === "chat"
        ? `/api/panes/${encodeURIComponent(pane.pane_id)}/chat`
        : `/api/panes/${encodeURIComponent(pane.pane_id)}/stream?mode=${mode}`;
    const es = new EventSource(url);
    es.addEventListener("messages", (ev) => {
      const p = JSON.parse((ev as MessageEvent).data) as { messages: ChatMessage[] };
      setMessages(p.messages ?? []);
    });
    es.addEventListener("log", (ev) => {
      const p = JSON.parse((ev as MessageEvent).data) as { text: string };
      setLogText(p.text ?? "");
    });
    es.addEventListener("frame", (ev) => {
      const p = JSON.parse((ev as MessageEvent).data) as { text: string };
      setLogText(p.text ?? "");
    });
    es.addEventListener("live", (ev) => {
      const p = JSON.parse((ev as MessageEvent).data) as { approval?: Approval };
      setApproval(p.approval ?? null);
    });
    return () => es.close();
  }, [pane.pane_id, mode]);

  const sendInput = async (e: FormEvent) => {
    e.preventDefault();
    const val = input.trim();
    if (!val) return;
    setInput("");
    try {
      if (pane.is_agent && mode === "chat") {
        await api(`/api/agents/${encodeURIComponent(pane.pane_id)}/prompt`, { prompt: val });
      } else {
        await api(`/api/panes/${encodeURIComponent(pane.pane_id)}/text`, { text: val, enter: true });
      }
    } catch (err) {
      onToast(err instanceof Error ? err.message : "发送失败");
    }
  };

  const sendKey = async (key: string) => {
    await api(`/api/panes/${encodeURIComponent(pane.pane_id)}/keys`, { keys: [key] });
  };

  const sendRawText = async (text: string) => {
    await api(`/api/panes/${encodeURIComponent(pane.pane_id)}/text`, { text, enter: false });
  };

  const answerApproval = async (choice: Approval["choices"][number]) => {
    if (approval?.style === "cursor" && choice.cursor && choice.cursor > 0) {
      const keys = [...Array<string>(Math.max(0, choice.cursor - 1)).fill("down"), "enter"];
      await api(`/api/panes/${encodeURIComponent(pane.pane_id)}/keys`, { keys });
      return;
    }
    await api(`/api/panes/${encodeURIComponent(pane.pane_id)}/text`, { text: choice.key, enter: true });
  };

  const closeWorkspace = async () => {
    await api(`/api/workspaces/${encodeURIComponent(pane.workspace_id)}/close`, {});
    onToast("已关闭工作区");
    onClosed();
  };

  return (
    <div className="flex-1 flex flex-col min-h-0">
      <header className="h-14 px-5 flex items-center justify-between border-b border-hairline shrink-0 gap-2">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <span className="font-semibold text-[15px] truncate">{pane.topic || pane.name}</span>
            <span className="text-[11px] font-mono-code px-1.5 py-0.5 rounded border border-strong text-secondary">
              {pane.is_agent ? pane.kind : "shell"}
            </span>
          </div>
          <div className="text-[11.5px] font-mono-code text-muted truncate mt-0.5">
            {pane.machine ? `${pane.machine} · ` : ""}
            {pane.cwd}
          </div>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          <div className="inline-flex p-0.5 rounded-lg text-xs surface-rail border border-hairline">
            {(["chat", "screen", "log"] as const).map((m) => (
              <button
                key={m}
                type="button"
                onClick={() => setMode(m)}
                className={`px-2.5 py-1 rounded-md cursor-pointer ${mode === m ? "surface-bg font-medium text-primary" : "text-secondary"}`}
              >
                {m === "chat" ? "对话" : m === "screen" ? "屏幕" : "日志"}
              </button>
            ))}
          </div>
          <button
            type="button"
            onClick={() => void closeWorkspace()}
            className="w-8 h-8 rounded-lg flex items-center justify-center text-muted surface-hover cursor-pointer"
            title="关闭工作区"
          >
            <Trash2 className="w-4 h-4" strokeWidth={1.7} />
          </button>
        </div>
      </header>

      <div className="flex-1 overflow-y-auto px-5 py-5 min-h-0">
        {mode === "screen" ? (
          <div className="h-full max-w-[960px] mx-auto">
            <TerminalScreen frame={logText} onKey={(k) => void sendKey(k)} onText={(t) => void sendRawText(t)} onToast={onToast} />
          </div>
        ) : mode === "chat" && messages.length > 0 ? (
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
                  <div key={idx} className="text-xs font-mono-code text-muted truncate">
                    • {m.name} {m.input || m.output}
                  </div>
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
                      onClick={() => void answerApproval(c)}
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
                      onClick={() => void sendKey("esc")}
                      className="px-2.5 py-1 rounded-lg text-xs border border-strong text-secondary surface-hover cursor-pointer"
                    >
                      Esc 取消
                    </button>
                  </div>
                </div>
              </div>
            )}
          </div>
        ) : (
          <pre className="max-w-[900px] mx-auto text-xs font-mono-code leading-relaxed whitespace-pre-wrap text-secondary">
            {logText || "等待终端输出…"}
          </pre>
        )}
      </div>

      <div className="px-5 pb-4 pt-2 shrink-0 space-y-2">
        <div className="max-w-[740px] mx-auto flex items-center gap-1.5 overflow-x-auto text-xs">
          {["esc", "ctrl+c", "tab", "up", "down", "enter"].map((k) => (
            <button
              key={k}
              type="button"
              onClick={() => void sendKey(k)}
              className="px-2.5 py-1 rounded-lg border border-hairline surface-rail text-secondary font-mono-code cursor-pointer shrink-0"
            >
              {k}
            </button>
          ))}
        </div>
        <form onSubmit={(e) => void sendInput(e)} className="max-w-[740px] mx-auto composer-box rounded-2xl px-4 py-2.5 flex items-center gap-2">
          <input
            type="text"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder={pane.is_agent ? `给 ${pane.kind} 发消息…` : "输入终端命令…"}
            className="flex-1 bg-transparent text-[14px] outline-none"
          />
          <button type="submit" className="w-8 h-8 rounded-full btn-accent flex items-center justify-center cursor-pointer">
            <ArrowUp className="w-4 h-4" />
          </button>
        </form>
      </div>
    </div>
  );
}

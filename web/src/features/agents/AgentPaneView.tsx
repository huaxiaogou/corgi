import { useEffect, useRef, useState, type ClipboardEvent, type FormEvent } from "react";
import { ArrowUp, Keyboard, Paperclip, Trash2, X } from "lucide-react";
import { TerminalScreen } from "../../components/TerminalScreen";
import { api, uploadFile } from "../../lib/api";
import type { AgentView, Approval, ChatMessage } from "../../types/api";
import { AgentChatThread } from "./AgentChatThread";

interface Props {
  pane: AgentView;
  onClosed: () => void;
  onOpenFile?: (path: string) => void;
  onToast: (msg: string) => void;
}

const SETUP_RE = /choose your color scheme|welcome to antigravity|select login method|waiting for auth|do you trust|trust this folder/i;

export function AgentPaneView({ pane, onClosed, onOpenFile, onToast }: Props) {
  const [mode, setMode] = useState<"chat" | "screen" | "log">(pane.is_agent ? "chat" : "screen");
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [logText, setLogText] = useState("");
  const [approval, setApproval] = useState<Approval | null>(null);
  const [input, setInput] = useState("");
  const [attachments, setAttachments] = useState<{ name: string; path: string }[]>([]);
  const [showKeys, setShowKeys] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    setMode(pane.is_agent ? "chat" : "screen");
    setShowKeys(false);
    void api("/api/seen", { pane: pane.pane_id });
  }, [pane.pane_id, pane.is_agent]);

  useEffect(() => {
    setLogText("");
    setMessages([]);
    setApproval(null);
    const enc = encodeURIComponent(pane.pane_id);
    const url = mode === "chat" ? `/api/panes/${enc}/chat` : `/api/panes/${enc}/stream?mode=${mode}`;
    const es = new EventSource(url);
    es.addEventListener("messages", (ev) => setMessages((JSON.parse((ev as MessageEvent).data) as { messages?: ChatMessage[] }).messages ?? []));
    es.addEventListener("log", (ev) => setLogText((JSON.parse((ev as MessageEvent).data) as { text?: string }).text ?? ""));
    es.addEventListener("frame", (ev) => setLogText((JSON.parse((ev as MessageEvent).data) as { text?: string }).text ?? ""));
    es.addEventListener("live", (ev) => setApproval((JSON.parse((ev as MessageEvent).data) as { approval?: Approval }).approval ?? null));
    return () => es.close();
  }, [pane.pane_id, mode]);

  const addFiles = async (files: FileList | File[]) => {
    for (const f of Array.from(files)) {
      try {
        const up = await uploadFile(f, pane.machine);
        setAttachments((prev) => [...prev, { name: up.name, path: up.path }]);
      } catch (err) {
        onToast(err instanceof Error ? err.message : "上传失败");
      }
    }
  };

  const handlePaste = (e: ClipboardEvent<HTMLInputElement>) => {
    const files = Array.from(e.clipboardData?.files ?? []);
    if (files.length > 0) {
      e.preventDefault();
      void addFiles(files);
    }
  };

  const sendInput = async (e: FormEvent) => {
    e.preventDefault();
    let val = input.trim();
    if (!val && attachments.length === 0) return;
    if (attachments.length > 0) {
      val = `${val ? val + "\n\n" : ""}附件：\n${attachments.map((a) => `- ${a.path}`).join("\n")}`;
    }
    setInput("");
    setAttachments([]);
    const enc = encodeURIComponent(pane.pane_id);
    try {
      if (pane.is_agent && mode === "chat") {
        await api(`/api/agents/${enc}/prompt`, { prompt: val });
      } else {
        await api(`/api/panes/${enc}/text`, { text: val, enter: true });
      }
    } catch (err) {
      onToast(err instanceof Error ? err.message : "发送失败");
    }
  };

  const sendKey = (key: string) => api(`/api/panes/${encodeURIComponent(pane.pane_id)}/keys`, { keys: [key] });
  const sendRawText = (text: string) => api(`/api/panes/${encodeURIComponent(pane.pane_id)}/text`, { text, enter: false });

  const answerApproval = async (choice: Approval["choices"][number]) => {
    const enc = encodeURIComponent(pane.pane_id);
    if (approval?.style === "cursor" && choice.cursor && choice.cursor > 0) {
      const keys = [...Array<string>(Math.max(0, choice.cursor - 1)).fill("down"), "enter"];
      await api(`/api/panes/${enc}/keys`, { keys });
      return;
    }
    await api(`/api/panes/${enc}/text`, { text: choice.key, enter: true });
  };

  const closeWorkspace = async () => {
    await api(`/api/workspaces/${encodeURIComponent(pane.workspace_id)}/close`, {});
    onToast("已关闭工作区");
    onClosed();
  };

  const keysVisible = mode !== "chat" || !pane.is_agent || showKeys;
  const setupHint = mode === "chat" && messages.length === 0 && SETUP_RE.test(logText);
  const modeTabs = pane.is_agent ? (["chat", "screen", "log"] as const) : (["screen", "log"] as const);

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
            {pane.machine ? `${pane.machine} · ` : ""}{pane.cwd}
          </div>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          <div className="inline-flex p-0.5 rounded-lg text-xs surface-rail border border-hairline">
            {modeTabs.map((m) => (
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
          <button type="button" onClick={() => void closeWorkspace()} className="w-8 h-8 rounded-lg flex items-center justify-center text-muted surface-hover cursor-pointer" title="关闭工作区">
            <Trash2 className="w-4 h-4" strokeWidth={1.7} />
          </button>
        </div>
      </header>

      <div className="flex-1 overflow-y-auto px-5 py-5 min-h-0">
        {mode === "screen" ? (
          <div className="h-full max-w-[960px] mx-auto">
            <TerminalScreen frame={logText} onKey={(k) => void sendKey(k)} onText={(t) => void sendRawText(t)} onToast={onToast} />
          </div>
        ) : mode === "chat" ? (
          <AgentChatThread
            messages={messages}
            approval={approval}
            setupHint={setupHint}
            isAgent={pane.is_agent}
            onAnswerApproval={(c) => void answerApproval(c)}
            onCancelApproval={() => void sendKey("esc")}
            onSwitchToScreen={() => setMode("screen")}
            onOpenFile={onOpenFile}
          />
        ) : (
          <pre className="max-w-[900px] mx-auto text-xs font-mono-code leading-relaxed whitespace-pre-wrap text-secondary">{logText || "等待终端输出…"}</pre>
        )}
      </div>

      <div className="px-5 pb-4 pt-2 shrink-0 space-y-2">
        {keysVisible && (
          <div className="max-w-[740px] mx-auto flex items-center gap-1.5 overflow-x-auto text-xs">
            {["esc", "ctrl+c", "tab", "up", "down", "enter"].map((k) => (
              <button key={k} type="button" onClick={() => void sendKey(k)} className="px-2.5 py-1 rounded-lg border border-hairline surface-rail text-secondary font-mono-code cursor-pointer shrink-0">
                {k}
              </button>
            ))}
          </div>
        )}
        <form onSubmit={(e) => void sendInput(e)} className="max-w-[740px] mx-auto composer-box rounded-2xl px-3 py-2 space-y-1.5">
          {attachments.length > 0 && (
            <div className="flex flex-wrap gap-1.5 px-1">
              {attachments.map((a, idx) => (
                <span key={idx} className="inline-flex items-center gap-1 text-[11px] px-2 py-0.5 rounded-md surface-rail border border-hairline text-secondary">
                  {a.name}
                  <button type="button" onClick={() => setAttachments((prev) => prev.filter((_, i) => i !== idx))} className="text-muted cursor-pointer"><X className="w-3 h-3" /></button>
                </span>
              ))}
            </div>
          )}
          <div className="flex items-center gap-2">
            <input ref={fileRef} type="file" multiple hidden onChange={(e) => { if (e.target.files) void addFiles(e.target.files); e.target.value = ""; }} />
            <button type="button" onClick={() => fileRef.current?.click()} title="添加图片或文件" className="w-8 h-8 rounded-lg flex items-center justify-center text-muted surface-hover cursor-pointer shrink-0">
              <Paperclip className="w-4 h-4" />
            </button>
            {pane.is_agent && mode === "chat" && (
              <button
                type="button"
                onClick={() => setShowKeys((v) => !v)}
                title={showKeys ? "隐藏终端快捷键" : "显示终端快捷键"}
                className={`w-8 h-8 rounded-lg flex items-center justify-center surface-hover cursor-pointer shrink-0 ${showKeys ? "text-primary surface-rail" : "text-muted"}`}
              >
                <Keyboard className="w-4 h-4" />
              </button>
            )}
            <input
              type="text"
              value={input}
              onPaste={handlePaste}
              onChange={(e) => setInput(e.target.value)}
              placeholder={pane.is_agent && mode === "chat" ? `给 ${pane.kind} 发消息（支持粘贴截图）…` : "输入终端命令…"}
              className="flex-1 bg-transparent text-[14px] outline-none"
            />
            <button type="submit" className="w-8 h-8 rounded-full btn-accent flex items-center justify-center cursor-pointer shrink-0">
              <ArrowUp className="w-4 h-4" />
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}

import { useState } from "react";
import { Check, ChevronDown, ChevronUp, Copy, FileCode2 } from "lucide-react";
import { parseWorkerUserEnvelope } from "../lib/workerEnvelope";

interface Props {
  text: string;
}

export function WorkerEnvelopeBubble({ text }: Props) {
  const [expanded, setExpanded] = useState(false);
  const [showRaw, setShowRaw] = useState(false);
  const [copiedCmd, setCopiedCmd] = useState("");
  const parsed = parseWorkerUserEnvelope(text);

  if (!parsed.isEnvelope) {
    return (
      <div className="flex justify-end">
        <div className="max-w-[80%] px-4 py-2.5 rounded-2xl rounded-br-sm text-[14px] pill-user whitespace-pre-wrap">
          {text}
        </div>
      </div>
    );
  }

  const copyCommand = (id: string) => {
    const cmd = `corgi show ${id}`;
    void navigator.clipboard?.writeText(cmd).then(() => {
      setCopiedCmd(id);
      window.setTimeout(() => setCopiedCmd((cur) => (cur === id ? "" : cur)), 1600);
    });
  };

  const displayText = showRaw ? parsed.raw : expanded || !parsed.shouldFold ? parsed.body : parsed.preview;

  return (
    <div className="flex justify-end" data-worker-envelope="true">
      <div className="max-w-[88%] sm:max-w-[82%] rounded-2xl rounded-br-sm border border-hairline surface-rail px-3.5 py-2.5 space-y-2 text-[13px]">
        <div className="flex flex-wrap items-center justify-between gap-1.5 text-[11px]">
          <div className="flex flex-wrap items-center gap-1.5 min-w-0">
            <span className="font-medium text-secondary">
              {parsed.batched ? `Corgi 批量派发 · ${parsed.eventCount} 条` : "Corgi 派发任务"}
            </span>
            {parsed.triggerIds.map((tid) => (
              <span
                key={tid}
                className="px-1.5 py-0.5 rounded font-mono-code text-[10.5px] border border-hairline surface-bg text-primary"
              >
                {tid}
              </span>
            ))}
            {parsed.constraint && (
              <span className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-[10.5px] border border-hairline surface-bg text-secondary">
                <span className="w-1.5 h-1.5 rounded-full" style={{ background: "var(--attn)" }} />
                <span>含会话红线</span>
              </span>
            )}
          </div>
          <button
            type="button"
            onClick={() => setShowRaw((v) => !v)}
            className="inline-flex items-center gap-1 text-[11px] text-muted hover:text-primary cursor-pointer shrink-0"
          >
            <FileCode2 className="w-3 h-3" />
            <span>{showRaw ? "返回精简视图" : "查看原始信封"}</span>
          </button>
        </div>

        {!showRaw && parsed.constraint && (
          <div className="px-2.5 py-1.5 rounded-lg surface-bg border border-hairline text-[11.5px] text-secondary leading-snug">
            <span className="font-medium text-primary mr-1">红线：</span>
            <span>{parsed.constraint}</span>
          </div>
        )}

        <div className="text-primary leading-relaxed whitespace-pre-wrap break-words font-normal">
          {displayText}
        </div>

        {parsed.spilledIds.length > 0 && (
          <div className="rounded-lg border border-strong surface-bg px-2.5 py-1.5 flex flex-wrap items-center justify-between gap-2 text-[11.5px]">
            <div className="flex items-center gap-1.5 min-w-0 text-secondary">
              <span className="w-1.5 h-1.5 rounded-full shrink-0" style={{ background: "var(--attn)" }} />
              <span className="truncate">
                正文已截断 · 查看完整任务（
                <code className="font-mono-code text-primary">
                  {parsed.spilledIds.map((id) => `corgi show ${id}`).join(" / ")}
                </code>
                ）
              </span>
            </div>
            <div className="flex items-center gap-1 shrink-0">
              {parsed.spilledIds.map((id) => (
                <button
                  key={id}
                  type="button"
                  onClick={() => copyCommand(id)}
                  className="inline-flex items-center gap-1 px-2 py-0.5 rounded border border-hairline surface-rail surface-hover text-[11px] font-mono-code text-primary cursor-pointer"
                >
                  {copiedCmd === id ? <Check className="w-3 h-3" /> : <Copy className="w-3 h-3" />}
                  <span>{copiedCmd === id ? "已复制" : `corgi show ${id}`}</span>
                </button>
              ))}
            </div>
          </div>
        )}

        {!showRaw && parsed.shouldFold && (
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="inline-flex items-center gap-1 text-[11.5px] text-secondary hover:text-primary cursor-pointer"
          >
            {expanded ? (
              <>
                <span>收起任务内容</span>
                <ChevronUp className="w-3.5 h-3.5" />
              </>
            ) : (
              <>
                <span>展开完整任务（共 {parsed.totalLines} 行）</span>
                <ChevronDown className="w-3.5 h-3.5" />
              </>
            )}
          </button>
        )}
      </div>
    </div>
  );
}

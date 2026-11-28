import { useEffect, useRef, useState } from "react";
import { Check, ChevronDown, ChevronUp, Copy, WrapText } from "lucide-react";

const FOLD_LINE_LIMIT = 18;

interface Props {
  lang: string;
  code: string;
}

export function countDiffLines(code: string): { added: number; removed: number } {
  let added = 0;
  let removed = 0;
  for (const line of code.split("\n")) {
    if (line.startsWith("+") && !line.startsWith("+++")) added++;
    else if (line.startsWith("-") && !line.startsWith("---")) removed++;
  }
  return { added, removed };
}

function renderCodeLine(line: string, idx: number, isDiff: boolean) {
  if (!isDiff) return <div key={idx}>{line || "\u00A0"}</div>;
  if (line.startsWith("+") && !line.startsWith("+++")) {
    return (
      <div key={idx} className="-mx-3 px-3" style={{ background: "var(--diff-add-bg)", color: "var(--diff-add-fg)" }}>
        {line}
      </div>
    );
  }
  if (line.startsWith("-") && !line.startsWith("---")) {
    return (
      <div key={idx} className="-mx-3 px-3" style={{ background: "var(--diff-del-bg)", color: "var(--diff-del-fg)" }}>
        {line}
      </div>
    );
  }
  if (line.startsWith("@@")) {
    return <div key={idx} className="text-muted">{line}</div>;
  }
  return <div key={idx}>{line || "\u00A0"}</div>;
}

export function CodeBlock({ lang, code }: Props) {
  const [copied, setCopied] = useState(false);
  const [softWrap, setSoftWrap] = useState(true);
  const [expanded, setExpanded] = useState(false);
  const hiddenRef = useRef<HTMLDivElement>(null);

  const lines = code.split("\n");
  const isDiff = lang === "diff" || lang === "patch";
  const diffStats = isDiff ? countDiffLines(code) : null;
  const foldable = lines.length > FOLD_LINE_LIMIT;
  const headLines = foldable ? lines.slice(0, FOLD_LINE_LIMIT) : lines;
  const tailLines = foldable ? lines.slice(FOLD_LINE_LIMIT) : [];

  // Support Cmd+F auto-expansion via beforematch when browser reveals hidden="until-found".
  useEffect(() => {
    const el = hiddenRef.current;
    if (!el) return;
    const onBeforeMatch = () => setExpanded(true);
    el.addEventListener("beforematch", onBeforeMatch);
    return () => el.removeEventListener("beforematch", onBeforeMatch);
  }, [foldable]);

  const copyCode = () => {
    void navigator.clipboard?.writeText(code).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1600);
    });
  };

  return (
    <div className="my-2.5 border-y border-hairline surface-rail">
      <div className="px-3 py-1 flex items-center justify-between gap-2 border-b border-hairline text-[10.5px] font-mono-code text-muted">
        <div className="flex items-center gap-2 min-w-0">
          <span className="truncate">{lang || "text"}</span>
          {diffStats && (diffStats.added > 0 || diffStats.removed > 0) && (
            <span className="inline-flex items-center gap-1.5 shrink-0">
              {diffStats.added > 0 && <span style={{ color: "var(--diff-add-fg)" }}>+{diffStats.added}</span>}
              {diffStats.removed > 0 && <span style={{ color: "var(--diff-del-fg)" }}>-{diffStats.removed}</span>}
            </span>
          )}
        </div>
        <div className="flex items-center gap-1 shrink-0">
          <button
            type="button"
            onClick={() => setSoftWrap((v) => !v)}
            title={softWrap ? "切换为横向滚动" : "切换为软换行"}
            className={`px-1.5 py-0.5 rounded inline-flex items-center gap-1 cursor-pointer surface-hover ${softWrap ? "text-primary" : "text-muted"}`}
          >
            <WrapText className="w-3 h-3" />
            <span className="hidden sm:inline">{softWrap ? "换行" : "横滚"}</span>
          </button>
          <button
            type="button"
            onClick={copyCode}
            title="复制代码"
            className="px-1.5 py-0.5 rounded inline-flex items-center gap-1 cursor-pointer surface-hover text-secondary hover:text-primary"
          >
            {copied ? <Check className="w-3 h-3" /> : <Copy className="w-3 h-3" />}
            <span>{copied ? "已复制" : "复制"}</span>
          </button>
        </div>
      </div>

      <pre
        className={`px-3 py-2.5 text-[12.5px] font-mono-code leading-relaxed text-secondary ${
          softWrap ? "whitespace-pre-wrap break-words [overflow-wrap:anywhere]" : "overflow-x-auto whitespace-pre"
        }`}
      >
        {headLines.map((ln, i) => renderCodeLine(ln, i, isDiff))}
        {foldable && (
          <div
            ref={hiddenRef}
            hidden={expanded ? undefined : ("until-found" as unknown as boolean)}
            data-code-folded={expanded ? "false" : "true"}
          >
            {tailLines.map((ln, i) => renderCodeLine(ln, FOLD_LINE_LIMIT + i, isDiff))}
          </div>
        )}
      </pre>

      {foldable && (
        <button
          type="button"
          onClick={() => setExpanded((v) => !v)}
          className="w-full px-3 py-1.5 border-t border-hairline flex items-center justify-center gap-1 text-[11px] font-mono-code text-muted hover:text-primary surface-hover cursor-pointer"
        >
          {expanded ? (
            <>
              <span>收起代码（共 {lines.length} 行）</span>
              <ChevronUp className="w-3 h-3" />
            </>
          ) : (
            <>
              <span>展开剩余 {tailLines.length} 行（共 {lines.length} 行）</span>
              <ChevronDown className="w-3 h-3" />
            </>
          )}
        </button>
      )}
    </div>
  );
}

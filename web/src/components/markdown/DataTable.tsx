import { useCallback, useEffect, useRef, useState } from "react";
import { Check, Copy } from "lucide-react";
import { renderInlineText, type InlineContext } from "./InlineText";

interface Props {
  headers: string[];
  rows: string[][];
  numericCols: boolean[];
  inlineCtx: InlineContext;
}

export function formatTableTSV(headers: string[], rows: string[][]): string {
  const line = (cells: string[]) => cells.map((c) => c.replace(/\t|\r?\n/g, " ")).join("\t");
  return [line(headers), ...rows.map(line)].join("\n");
}

export function formatTableMarkdown(headers: string[], rows: string[][]): string {
  const esc = (c: string) => c.replace(/\|/g, "\\|").replace(/\r?\n/g, " ");
  const h = `| ${headers.map(esc).join(" | ")} |`;
  const sep = `| ${headers.map(() => "---").join(" | ")} |`;
  const body = rows.map((r) => `| ${headers.map((_, i) => esc(r[i] ?? "")).join(" | ")} |`);
  return [h, sep, ...body].join("\n");
}

export function DataTable({ headers, rows, numericCols, inlineCtx }: Props) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const [scrollLeft, setScrollLeft] = useState(false);
  const [scrollRight, setScrollRight] = useState(false);
  const [copiedMode, setCopiedMode] = useState<"" | "tsv" | "md">("");

  const isWide = headers.length >= 4;

  const updateMask = useCallback(() => {
    const el = scrollRef.current;
    if (!el || !isWide) return;
    setScrollLeft(el.scrollLeft > 2);
    setScrollRight(el.scrollWidth - el.clientWidth - el.scrollLeft > 4);
  }, [isWide]);

  useEffect(() => {
    if (!isWide) return;
    const el = scrollRef.current;
    if (!el) return;
    updateMask();
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(updateMask);
    ro.observe(el);
    return () => ro.disconnect();
  }, [isWide, updateMask, headers.length, rows.length]);

  const copyAs = (mode: "tsv" | "md") => {
    const text = mode === "tsv" ? formatTableTSV(headers, rows) : formatTableMarkdown(headers, rows);
    void navigator.clipboard?.writeText(text).then(() => {
      setCopiedMode(mode);
      window.setTimeout(() => setCopiedMode(""), 1600);
    });
  };

  return (
    <div
      className="my-2.5 border-y border-hairline md-table-wrap"
      data-cols={headers.length}
      data-wide={isWide ? "true" : "false"}
      data-scroll-left={scrollLeft ? "true" : "false"}
    >
      <div className="px-1 py-1 flex items-center justify-end gap-1 text-[10.5px] font-mono-code text-muted">
        <button
          type="button"
          onClick={() => copyAs("tsv")}
          title="复制为 TSV（可直接粘贴表格软件）"
          className="px-1.5 py-0.5 rounded inline-flex items-center gap-1 surface-hover cursor-pointer hover:text-primary"
        >
          {copiedMode === "tsv" ? <Check className="w-3 h-3" /> : <Copy className="w-3 h-3" />}
          <span>{copiedMode === "tsv" ? "已复制" : "TSV"}</span>
        </button>
        <button
          type="button"
          onClick={() => copyAs("md")}
          title="复制为 Markdown 表格"
          className="px-1.5 py-0.5 rounded inline-flex items-center gap-1 surface-hover cursor-pointer hover:text-primary"
        >
          {copiedMode === "md" ? <Check className="w-3 h-3" /> : <Copy className="w-3 h-3" />}
          <span>{copiedMode === "md" ? "已复制" : "MD"}</span>
        </button>
      </div>

      <div className="relative">
        <div ref={scrollRef} onScroll={updateMask} className="md-table-scroll">
          <table className="md-table w-full text-left text-xs border-collapse">
            <thead>
              <tr className="border-b border-strong">
                {headers.map((h, hi) => (
                  <th
                    key={hi}
                    className={`px-2.5 py-2 font-semibold text-primary align-top break-words [overflow-wrap:anywhere] ${
                      numericCols[hi] ? "text-right tabular-nums" : "text-left"
                    }`}
                  >
                    {renderInlineText(h, inlineCtx)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y divide-[var(--line)]">
              {rows.map((r, ri) => (
                <tr key={ri}>
                  {headers.map((_, ci) => (
                    <td
                      key={ci}
                      className={`px-2.5 py-2 text-secondary align-top break-words [overflow-wrap:anywhere] ${
                        numericCols[ci] ? "text-right tabular-nums font-mono-code" : "text-left"
                      }`}
                    >
                      {renderInlineText(r[ci] ?? "", inlineCtx)}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {isWide && scrollRight && <div aria-hidden="true" className="md-table-fade-r" />}
      </div>
    </div>
  );
}

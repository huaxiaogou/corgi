import { useEffect, useId, useState } from "react";
import { Check, Code2, Copy, Eye, Maximize2, X } from "lucide-react";
import {
  buildMermaidInitConfig,
  detectMermaidDiagramKind,
  readMermaidTokenPalette,
  rewriteMermaidDirectionForMobile,
} from "../../lib/mermaidConfig";

interface Props {
  code: string;
}

type MermaidModule = typeof import("mermaid")["default"];

let mermaidPromise: Promise<MermaidModule> | null = null;
let renderSerial = 0;

function loadMermaid(): Promise<MermaidModule> {
  if (!mermaidPromise) {
    mermaidPromise = import("mermaid")
      .then((m) => m.default)
      .catch((err) => {
        mermaidPromise = null;
        throw err;
      });
  }
  return mermaidPromise;
}

export function MermaidBlock({ code }: Props) {
  const rawId = useId();
  const [svg, setSvg] = useState("");
  const [loading, setLoading] = useState(true);
  const [errorMsg, setErrorMsg] = useState("");
  const [showSource, setShowSource] = useState(false);
  const [copied, setCopied] = useState(false);
  const [zoomOpen, setZoomOpen] = useState(false);
  const [themeRev, setThemeRev] = useState(0);
  const [isMobile, setIsMobile] = useState(() =>
    typeof window !== "undefined" ? window.matchMedia("(max-width: 639px)").matches : false
  );

  const diagramKind = detectMermaidDiagramKind(code);
  const effectiveCode = rewriteMermaidDirectionForMobile(code, isMobile);

  useEffect(() => {
    if (typeof window === "undefined") return;
    const mq = window.matchMedia("(max-width: 639px)");
    const onChange = (e: MediaQueryListEvent) => setIsMobile(e.matches);
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  useEffect(() => {
    if (typeof document === "undefined" || typeof MutationObserver === "undefined") return;
    const observer = new MutationObserver(() => setThemeRev((v) => v + 1));
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    if (typeof window === "undefined") return;
    let cancelled = false;
    setLoading(true);
    setErrorMsg("");

    const renderId = `corgi-mmd-${rawId.replace(/[^a-zA-Z0-9_-]/g, "")}-${++renderSerial}`;

    void loadMermaid()
      .then(async (mermaid) => {
        if (cancelled) return;
        const palette = readMermaidTokenPalette();
        mermaid.initialize(buildMermaidInitConfig(palette));
        const result = await mermaid.render(renderId, effectiveCode);
        if (cancelled) return;
        setSvg(result.svg);
        setLoading(false);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        // Clean up any temporary error SVG element mermaid may have attached.
        document.getElementById(`d${renderId}`)?.remove();
        setSvg("");
        setLoading(false);
        const detail = err instanceof Error ? err.message : "";
        setErrorMsg(detail || "图表渲染失败");
      });

    return () => {
      cancelled = true;
    };
  }, [effectiveCode, themeRev, rawId]);

  useEffect(() => {
    if (!zoomOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setZoomOpen(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [zoomOpen]);

  const copySource = () => {
    void navigator.clipboard?.writeText(code).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1400);
    });
  };

  const fallbackToSource = Boolean(errorMsg) || showSource;

  return (
    <div
      data-mermaid-block="true"
      data-mermaid-kind={diagramKind.toLowerCase()}
      className="my-2 rounded-lg border border-hairline surface-rail overflow-hidden"
    >
      <div className="px-3 py-1.5 border-b border-hairline flex items-center justify-between gap-2 text-[11px] font-mono-code text-muted">
        <div className="flex items-center gap-2 min-w-0">
          <span className="uppercase tracking-wider text-secondary">MERMAID · {diagramKind}</span>
          {isMobile && effectiveCode !== code && (
            <span className="px-1.5 py-0.5 rounded border border-hairline surface-bg text-[10px]">TD 适配</span>
          )}
        </div>
        <div className="flex items-center gap-2 shrink-0">
          {svg && !errorMsg && (
            <>
              <button
                type="button"
                onClick={() => setShowSource((v) => !v)}
                className="inline-flex items-center gap-1 hover:text-primary cursor-pointer"
                title={showSource ? "切回图表" : "查看 Mermaid 源码"}
              >
                {showSource ? <Eye className="w-3 h-3" /> : <Code2 className="w-3 h-3" />}
                <span>{showSource ? "图表" : "源码"}</span>
              </button>
              {!showSource && (
                <button
                  type="button"
                  onClick={() => setZoomOpen(true)}
                  className="inline-flex items-center gap-1 hover:text-primary cursor-pointer"
                  title="放大查看图表"
                >
                  <Maximize2 className="w-3 h-3" />
                  <span>放大</span>
                </button>
              )}
            </>
          )}
          <button
            type="button"
            onClick={copySource}
            className="inline-flex items-center gap-1 hover:text-primary cursor-pointer"
          >
            {copied ? <Check className="w-3 h-3" /> : <Copy className="w-3 h-3" />}
            <span>{copied ? "已复制" : "复制"}</span>
          </button>
        </div>
      </div>

      {errorMsg && (
        <div className="px-3 py-1.5 border-b border-hairline surface-bg text-[11.5px] text-secondary flex items-center gap-2">
          <span className="w-1.5 h-1.5 rounded-full shrink-0" style={{ background: "var(--attn)" }} />
          <span className="truncate">当前离线或图表语法异常，已降级展示源码</span>
        </div>
      )}

      {loading && !svg && !errorMsg && (
        <div className="px-3.5 py-4 text-xs text-muted font-mono-code">正在按需加载 Mermaid 图表…</div>
      )}

      {!fallbackToSource && svg && (
        <div
          className="p-3.5 overflow-x-auto surface-bg md-mermaid-svg"
          dangerouslySetInnerHTML={{ __html: svg }}
        />
      )}

      {fallbackToSource && (
        <pre className="p-3.5 text-[12px] font-mono-code leading-relaxed overflow-x-auto surface-rail text-primary">
          <code>{code}</code>
        </pre>
      )}

      {zoomOpen && svg && (
        <div
          role="dialog"
          aria-modal="true"
          aria-label="Mermaid 图表放大预览"
          className="fixed inset-0 z-50 flex items-center justify-center p-4"
          style={{ background: "rgba(0,0,0,0.45)" }}
          onClick={() => setZoomOpen(false)}
        >
          <div
            onClick={(e) => e.stopPropagation()}
            className="w-full max-w-[92vw] max-h-[88vh] flex flex-col rounded-2xl card-floating overflow-hidden"
          >
            <div className="px-4 py-2.5 border-b border-hairline surface-rail flex items-center justify-between gap-2 text-xs">
              <span className="font-mono-code text-secondary">MERMAID · {diagramKind}</span>
              <button
                type="button"
                onClick={() => setZoomOpen(false)}
                aria-label="关闭预览"
                className="w-7 h-7 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
              >
                <X className="w-4 h-4" />
              </button>
            </div>
            <div
              className="flex-1 overflow-auto p-5 surface-bg md-mermaid-svg"
              dangerouslySetInnerHTML={{ __html: svg }}
            />
          </div>
        </div>
      )}
    </div>
  );
}

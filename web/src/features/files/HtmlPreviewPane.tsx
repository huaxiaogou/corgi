import { useEffect, useState } from "react";
import { RotateCw } from "lucide-react";
import { api } from "../../lib/api";
import { buildArtifactAPIURL, buildFileAPIURL } from "../../lib/previewLink";
import type { FileResponse } from "../../types/api";

interface Props {
  artifactId?: string;
  path?: string;
  machine?: string;
  title?: string;
  rawView: boolean;
  reloadSeq: number;
  resizing?: boolean;
}

type PreflightState =
  | { key: string; status: "loading" }
  | { key: string; status: "ready"; truncated: boolean }
  | { key: string; status: "error"; message: string };

function mapHeadStatusToError(status: number, isArtifact: boolean, machine?: string): string {
  if (status === 404) {
    if (isArtifact) return "该快照已过期或不可用";
    return machine
      ? `在 ${machine} 上找不到该 HTML 文件`
      : "找不到该 HTML 文件（若文件位于其他在线电脑，请在对应会话中打开）";
  }
  if (status === 400) return "仅支持 .html / .htm 文件内嵌预览";
  if (status === 401) return "登录状态已失效，请刷新页面重新登录";
  if (status === 503) return machine ? `电脑不在线：${machine}` : "目标电脑当前不在线";
  if (status === 502) return machine ? `转发到 ${machine} 失败` : "远程文件转发失败";
  return `HTML 预览加载失败 (${status})`;
}

export function HtmlPreviewPane({
  artifactId,
  path = "",
  machine,
  title,
  rawView,
  reloadSeq,
  resizing = false,
}: Props) {
  const isArtifact = Boolean(artifactId);
  const htmlSrc = isArtifact
    ? buildArtifactAPIURL(artifactId!, "raw")
    : buildFileAPIURL({ path, machine }, { as: "html", scripts: "1" });
  const rawSrc = isArtifact ? buildArtifactAPIURL(artifactId!) : buildFileAPIURL({ path, machine });

  const [preflight, setPreflight] = useState<PreflightState>({ key: "", status: "loading" });
  const [retrySeq, setRetrySeq] = useState(0);
  const [source, setSource] = useState<{ key: string; data: FileResponse } | null>(null);
  const [sourceErr, setSourceErr] = useState("");

  useEffect(() => {
    if (rawView) return;
    const ac = new AbortController();
    setPreflight({ key: htmlSrc, status: "loading" });

    void fetch(htmlSrc, {
      method: "HEAD",
      credentials: "same-origin",
      cache: "no-store",
      signal: ac.signal,
    })
      .then((res) => {
        if (ac.signal.aborted) return;
        if (!res.ok) {
          setPreflight({
            key: htmlSrc,
            status: "error",
            message: mapHeadStatusToError(res.status, isArtifact, machine),
          });
          return;
        }
        setPreflight({
          key: htmlSrc,
          status: "ready",
          truncated: res.headers.get("X-Corgi-Truncated") === "1",
        });
      })
      .catch((err) => {
        if (ac.signal.aborted) return;
        setPreflight({
          key: htmlSrc,
          status: "error",
          message: err instanceof Error ? err.message : "预览检查失败",
        });
      });

    return () => ac.abort();
  }, [htmlSrc, isArtifact, machine, rawView, reloadSeq, retrySeq]);

  useEffect(() => {
    if (!rawView) return;
    let cancelled = false;
    setSource(null);
    setSourceErr("");

    void api<FileResponse>(rawSrc)
      .then((res) => {
        if (!cancelled) setSource({ key: rawSrc, data: res });
      })
      .catch((err) => {
        if (!cancelled) setSourceErr(err instanceof Error ? err.message : "读取源码失败");
      });

    return () => {
      cancelled = true;
    };
  }, [rawSrc, rawView, reloadSeq]);

  if (rawView) {
    const currentSource = source?.key === rawSrc ? source.data : null;
    return (
      <div className="flex-1 overflow-y-auto p-4 text-xs surface-bg">
        {sourceErr && <div style={{ color: "var(--attn)" }}>{sourceErr}</div>}
        {!currentSource && !sourceErr && <div className="text-muted">加载源码中…</div>}
        {currentSource && (
          <pre className="font-mono-code leading-relaxed whitespace-pre-wrap text-secondary">
            {currentSource.content}
          </pre>
        )}
      </div>
    );
  }

  const activePreflight = preflight.key === htmlSrc ? preflight : { key: htmlSrc, status: "loading" as const };

  return (
    <div className="flex-1 flex flex-col min-h-0 overflow-hidden surface-bg">
      {activePreflight.status === "loading" && (
        <div className="flex-1 flex items-center justify-center text-xs text-muted">加载预览中…</div>
      )}

      {activePreflight.status === "error" && (
        <div className="flex-1 flex flex-col items-center justify-center gap-3 p-6 text-center">
          <div className="text-xs max-w-[320px] leading-relaxed" style={{ color: "var(--attn)" }}>
            {activePreflight.message}
          </div>
          <button
            type="button"
            onClick={() => setRetrySeq((n) => n + 1)}
            className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs border border-strong text-secondary surface-hover cursor-pointer"
          >
            <RotateCw className="w-3.5 h-3.5" />
            <span>重试</span>
          </button>
        </div>
      )}

      {activePreflight.status === "ready" && (
        <iframe
          key={`${htmlSrc}:${reloadSeq}:${retrySeq}`}
          src={htmlSrc}
          sandbox="allow-scripts"
          referrerPolicy="no-referrer"
          title={title || path || artifactId || "HTML Preview"}
          className={`w-full flex-1 min-h-0 border-0 surface-bg ${resizing ? "pointer-events-none" : ""}`}
        />
      )}
    </div>
  );
}

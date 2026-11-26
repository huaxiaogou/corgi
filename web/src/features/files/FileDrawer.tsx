import { useCallback, useEffect, useRef, useState } from "react";
import { ArrowLeft, Download, ExternalLink, Maximize2, Minimize2, RotateCw, X } from "lucide-react";
import { MarkdownText } from "../../components/MarkdownText";
import { api } from "../../lib/api";
import { clampSplitWidth, computeSplitBounds } from "../../lib/previewLayout";
import {
  buildArtifactAPIURL,
  buildFileAPIURL,
  buildPublicPreviewURL,
  fileBaseName,
  getPreviewFileKind,
} from "../../lib/previewLink";
import type { EntryWorkerRef, FileDrawerTarget, FileResponse } from "../../types/api";
import { HtmlPreviewPane } from "./HtmlPreviewPane";
import { SplitResizer } from "./SplitResizer";

interface Props {
  target: FileDrawerTarget | null;
  onClose: () => void;
  onJumpWorker?: (ref: EntryWorkerRef) => void;
}

const WIDTH_STORAGE_KEY_HTML = "corgi.preview.width.html";
const WIDTH_STORAGE_KEY_FILE = "corgi.preview.width.file";

function readSavedWidth(key: string): number | null {
  try {
    const raw = window.sessionStorage.getItem(key);
    const n = raw ? Number(raw) : NaN;
    return Number.isFinite(n) && n > 0 ? n : null;
  } catch {
    return null;
  }
}

export function FileDrawer({ target, onClose, onJumpWorker }: Props) {
  const asideRef = useRef<HTMLElement>(null);
  const [file, setFile] = useState<FileResponse | null>(null);
  const [err, setErr] = useState("");
  const [rawView, setRawView] = useState(false);
  const [lightbox, setLightbox] = useState(false);
  const [reloadSeq, setReloadSeq] = useState(0);
  const [resizing, setResizing] = useState(false);
  const [isDesktop, setIsDesktop] = useState(() =>
    typeof window !== "undefined" ? window.matchMedia("(min-width: 1024px)").matches : true
  );
  const [containerWidth, setContainerWidth] = useState(1120);
  const [customWidths, setCustomWidths] = useState<{ html: number | null; file: number | null }>(() => ({
    html: typeof window !== "undefined" ? readSavedWidth(WIDTH_STORAGE_KEY_HTML) : null,
    file: typeof window !== "undefined" ? readSavedWidth(WIDTH_STORAGE_KEY_FILE) : null,
  }));

  const kind = !target
    ? "text"
    : "artifactId" in target
      ? target.kind === "html" || target.kind === "image" || target.kind === "markdown"
        ? target.kind
        : target.kind === "binary"
          ? "binary"
          : getPreviewFileKind(target.name)
      : getPreviewFileKind(target.path);
  const isImage = kind === "image";
  const isHTML = kind === "html";
  const isMarkdown = kind === "markdown";

  useEffect(() => {
    const mql = window.matchMedia("(min-width: 1024px)");
    const sync = () => setIsDesktop(mql.matches);
    sync();
    mql.addEventListener("change", sync);
    return () => mql.removeEventListener("change", sync);
  }, []);

  useEffect(() => {
    if (!target) return;
    const parent = asideRef.current?.parentElement;
    if (!parent) return;
    const measure = () => {
      if (parent.clientWidth > 0) setContainerWidth(parent.clientWidth);
    };
    measure();
    if (typeof ResizeObserver !== "undefined") {
      const ro = new ResizeObserver(measure);
      ro.observe(parent);
      return () => ro.disconnect();
    }
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  }, [target, isDesktop]);

  useEffect(() => {
    if (!target) return;
    setFile(null);
    setErr("");
    setRawView(false);
    setLightbox(false);
    setReloadSeq(0);

    if (kind === "image" || kind === "html") return;
    let cancelled = false;
    const url = "artifactId" in target ? buildArtifactAPIURL(target.artifactId) : buildFileAPIURL(target);
    void api<FileResponse>(url)
      .then((res) => {
        if (!cancelled) setFile(res);
      })
      .catch((e) => {
        if (!cancelled) setErr(e instanceof Error ? e.message : "读取失败");
      });
    return () => {
      cancelled = true;
    };
  }, [target, kind]);

  useEffect(() => {
    if (!target) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      if (lightbox) setLightbox(false);
      else onClose();
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [target, lightbox, onClose]);

  const bounds = computeSplitBounds(containerWidth, isHTML);
  const savedForKind = isHTML ? customWidths.html : customWidths.file;
  const activeWidth = clampSplitWidth(savedForKind ?? bounds.defaultWidth, bounds.min, bounds.max);
  const isWide = isHTML && activeWidth >= bounds.wideWidth - 24;

  const updateWidth = useCallback(
    (next: number | null) => {
      const storageKey = isHTML ? WIDTH_STORAGE_KEY_HTML : WIDTH_STORAGE_KEY_FILE;
      const clamped = next === null ? null : clampSplitWidth(next, bounds.min, bounds.max);
      setCustomWidths((prev) => (isHTML ? { ...prev, html: clamped } : { ...prev, file: clamped }));
      try {
        if (clamped === null) window.sessionStorage.removeItem(storageKey);
        else window.sessionStorage.setItem(storageKey, String(clamped));
      } catch {
        // ignore storage restrictions
      }
    },
    [isHTML, bounds.min, bounds.max]
  );

  if (!target) return null;

  const displayName = "artifactId" in target ? target.name : fileBaseName(target.path);
  const subtitle = "artifactId" in target ? "Hub 在线快照" : target.path;
  const imgSrc =
    "artifactId" in target ? buildArtifactAPIURL(target.artifactId, "raw") : buildFileAPIURL(target, { as: "image" });
  const newTabHref =
    "artifactId" in target
      ? target.shareToken
        ? buildPublicPreviewURL(target.shareToken)
        : buildArtifactAPIURL(target.artifactId, "raw")
      : buildFileAPIURL(target, { as: "html", scripts: "1" });
  const downloadSrc =
    "artifactId" in target
      ? buildArtifactAPIURL(target.artifactId, "download")
      : buildFileAPIURL(target, { download: "1" });

  return (
    <>
      <div
        className="fixed inset-0 z-30 hidden md:block lg:hidden"
        style={{ background: "rgba(0,0,0,0.28)" }}
        onClick={onClose}
      />

      {isDesktop && (
        <SplitResizer
          width={activeWidth}
          bounds={bounds}
          onWidthChange={(w) => updateWidth(w)}
          onReset={() => updateWidth(null)}
          onDraggingChange={setResizing}
        />
      )}

      <aside
        ref={asideRef}
        aria-label="文件与 HTML 预览"
        style={isDesktop ? { width: `${activeWidth}px` } : undefined}
        className="fixed inset-0 z-40 md:left-auto md:w-[min(86vw,640px)] md:shadow-2xl lg:static lg:inset-auto lg:z-auto lg:shadow-none shrink-0 flex flex-col surface-rail border-l border-hairline"
      >
        <div className="h-14 px-3.5 flex items-center justify-between border-b border-hairline shrink-0 gap-2">
          <div className="flex items-center gap-2 min-w-0">
            <button
              type="button"
              onClick={onClose}
              className="md:hidden inline-flex items-center gap-1 px-2 py-1 rounded-lg text-xs text-secondary border border-hairline surface-bg surface-hover cursor-pointer shrink-0"
              title="返回对话"
            >
              <ArrowLeft className="w-3.5 h-3.5" />
              <span>返回</span>
            </button>
            <div className="min-w-0">
              <div className="flex items-center gap-1.5 min-w-0">
                <span className="text-[13px] font-semibold truncate">{displayName}</span>
                {"artifactId" in target ? (
                  <span className="text-[10.5px] px-1.5 py-0.5 rounded border border-hairline surface-bg text-muted shrink-0">
                    Hub 快照
                  </span>
                ) : (
                  target.machine && (
                    <span className="text-[10.5px] font-mono-code px-1.5 py-0.5 rounded border border-hairline surface-bg text-muted shrink-0">
                      {target.machine}
                    </span>
                  )
                )}
              </div>
              <div className="text-[11px] font-mono-code text-muted truncate" title={subtitle}>
                {subtitle}
              </div>
            </div>
          </div>

          <div className="flex items-center gap-1 shrink-0">
            {(isMarkdown || isHTML) && (
              <button
                type="button"
                onClick={() => setRawView((v) => !v)}
                className="px-2 py-1 rounded-md text-[11px] border border-hairline text-secondary surface-hover cursor-pointer"
              >
                {rawView ? (isHTML ? "预览" : "渲染") : isHTML ? "源码" : "原文"}
              </button>
            )}
            {isHTML && (
              <>
                <button
                  type="button"
                  onClick={() => setReloadSeq((n) => n + 1)}
                  className="w-7 h-7 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
                  title="刷新预览"
                >
                  <RotateCw className="w-3.5 h-3.5" />
                </button>
                <button
                  type="button"
                  onClick={() => updateWidth(isWide ? bounds.defaultWidth : bounds.wideWidth)}
                  className="hidden lg:flex w-7 h-7 rounded-lg items-center justify-center text-secondary surface-hover cursor-pointer"
                  title={isWide ? "恢复默认宽度" : "展开更宽预览"}
                >
                  {isWide ? <Minimize2 className="w-3.5 h-3.5" /> : <Maximize2 className="w-3.5 h-3.5" />}
                </button>
                <a
                  href={newTabHref}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="w-7 h-7 rounded-lg flex items-center justify-center text-secondary surface-hover"
                  title="在新标签页打开"
                >
                  <ExternalLink className="w-3.5 h-3.5" />
                </a>
              </>
            )}
            {isImage && (
              <button
                type="button"
                onClick={() => setLightbox(true)}
                className="w-7 h-7 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
                title="全屏灯箱查看"
              >
                <Maximize2 className="w-3.5 h-3.5" />
              </button>
            )}
            <a
              href={downloadSrc}
              className="w-7 h-7 rounded-lg flex items-center justify-center text-secondary surface-hover"
              title="下载原件"
            >
              <Download className="w-3.5 h-3.5" />
            </a>
            <button
              type="button"
              onClick={onClose}
              className="w-7 h-7 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
              title="关闭 (Esc)"
            >
              <X className="w-4 h-4" />
            </button>
          </div>
        </div>

        {isHTML ? (
          <HtmlPreviewPane
            artifactId={"artifactId" in target ? target.artifactId : undefined}
            path={"path" in target ? target.path : undefined}
            machine={"path" in target ? target.machine : undefined}
            title={displayName}
            rawView={rawView}
            reloadSeq={reloadSeq}
            resizing={resizing}
          />
        ) : (
          <div className="flex-1 overflow-y-auto p-4 text-xs">
            {err && <div style={{ color: "var(--attn)" }}>{err}</div>}
            {isImage && (
              <button type="button" onClick={() => setLightbox(true)} className="block cursor-zoom-in">
                <img src={imgSrc} alt={displayName} className="max-w-full rounded-lg border border-hairline" />
              </button>
            )}
            {file && file.kind === "text" &&
              (isMarkdown && !rawView ? (
                <div className="text-[13px] text-primary">
                  <MarkdownText text={file.content} />
                </div>
              ) : (
                <pre className="font-mono-code leading-relaxed whitespace-pre-wrap text-secondary">{file.content}</pre>
              ))}
            {file && file.kind === "binary" && (
              <div className="text-muted">二进制文件，请点击右上角下载原件。</div>
            )}
          </div>
        )}
        {"artifactId" in target && target.workerRef && onJumpWorker && (
          <div className="px-3.5 py-2.5 border-t border-hairline surface-bg flex items-center justify-between gap-2 shrink-0 text-xs">
            <span className="text-muted truncate">
              由 <strong className="font-medium text-secondary">{target.workerRef.name}</strong> 产出
              {target.workerRef.model ? ` · ${target.workerRef.model}` : ""}
            </span>
            <button
              type="button"
              onClick={() => onJumpWorker(target.workerRef!)}
              className="px-2.5 py-1 rounded-lg btn-accent text-[11.5px] font-medium cursor-pointer shrink-0"
            >
              继续问这个 Worker →
            </button>
          </div>
        )}
      </aside>

      {lightbox && isImage && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center p-6 cursor-zoom-out"
          style={{ background: "rgba(0,0,0,0.78)" }}
          onClick={() => setLightbox(false)}
        >
          <img src={imgSrc} alt={displayName} className="max-w-[92vw] max-h-[90vh] rounded-xl shadow-2xl" />
        </div>
      )}
    </>
  );
}

import { useEffect, useState } from "react";
import { Download, Maximize2, X } from "lucide-react";
import { MarkdownText } from "../../components/MarkdownText";
import { api } from "../../lib/api";
import type { FileResponse } from "../../types/api";

interface Props {
  target: { path: string; machine?: string } | null;
  onClose: () => void;
}

export function FileDrawer({ target, onClose }: Props) {
  const [file, setFile] = useState<FileResponse | null>(null);
  const [err, setErr] = useState("");
  const [rawView, setRawView] = useState(false);
  const [lightbox, setLightbox] = useState(false);

  useEffect(() => {
    if (!target) return;
    setFile(null);
    setErr("");
    setRawView(false);
    setLightbox(false);
    const ext = target.path.toLowerCase().split(".").pop() ?? "";
    if (["png", "jpg", "jpeg", "gif", "webp", "html", "htm"].includes(ext)) {
      return;
    }
    const qs = new URLSearchParams({ path: target.path });
    if (target.machine) qs.set("machine", target.machine);
    void api<FileResponse>(`/api/file?${qs.toString()}`)
      .then(setFile)
      .catch((e) => setErr(e instanceof Error ? e.message : "读取失败"));
  }, [target]);

  if (!target) return null;

  const ext = target.path.toLowerCase().split(".").pop() ?? "";
  const isImage = ["png", "jpg", "jpeg", "gif", "webp"].includes(ext);
  const isHTML = ext === "html" || ext === "htm";
  const isMarkdown = ext === "md";
  const qs = new URLSearchParams({ path: target.path });
  if (target.machine) qs.set("machine", target.machine);
  const imgSrc = `/api/file?${qs.toString()}&as=image`;

  return (
    <>
      <aside className="w-full sm:w-[400px] shrink-0 flex flex-col surface-rail border-l border-hairline">
        <div className="h-14 px-4 flex items-center justify-between border-b border-hairline shrink-0 gap-2">
          <div className="min-w-0">
            <div className="text-[13px] font-semibold truncate">{target.path.split("/").pop()}</div>
            <div className="text-[11px] font-mono-code text-muted truncate">{target.path}</div>
          </div>
          <div className="flex items-center gap-1 shrink-0">
            {isMarkdown && (
              <button
                type="button"
                onClick={() => setRawView((v) => !v)}
                className="px-2 py-1 rounded-md text-[11px] border border-hairline text-secondary surface-hover cursor-pointer"
              >
                {rawView ? "渲染" : "原文"}
              </button>
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
              href={`/api/file?${qs.toString()}&download=1`}
              className="w-7 h-7 rounded-lg flex items-center justify-center text-secondary surface-hover"
              title="下载原件"
            >
              <Download className="w-3.5 h-3.5" />
            </a>
            <button type="button" onClick={onClose} className="w-7 h-7 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer">
              <X className="w-4 h-4" />
            </button>
          </div>
        </div>

        <div className="flex-1 overflow-y-auto p-4 text-xs">
          {err && <div style={{ color: "var(--attn)" }}>{err}</div>}
          {isImage && (
            <button type="button" onClick={() => setLightbox(true)} className="block cursor-zoom-in">
              <img src={imgSrc} alt={target.path} className="max-w-full rounded-lg border border-hairline" />
            </button>
          )}
          {isHTML && (
            <iframe
              src={`/api/file?${qs.toString()}&as=html`}
              sandbox="allow-scripts allow-forms"
              title={target.path}
              className="w-full h-[520px] rounded-lg border border-hairline surface-bg"
            />
          )}
          {file && file.kind === "text" && (
            isMarkdown && !rawView ? (
              <div className="text-[13px] text-primary">
                <MarkdownText text={file.content} />
              </div>
            ) : (
              <pre className="font-mono-code leading-relaxed whitespace-pre-wrap text-secondary">{file.content}</pre>
            )
          )}
          {file && file.kind === "binary" && <div className="text-muted">二进制文件，请点击右上角下载原件。</div>}
        </div>
      </aside>

      {lightbox && isImage && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center p-6 cursor-zoom-out"
          style={{ background: "rgba(0,0,0,0.78)" }}
          onClick={() => setLightbox(false)}
        >
          <img src={imgSrc} alt={target.path} className="max-w-[92vw] max-h-[90vh] rounded-xl shadow-2xl" />
        </div>
      )}
    </>
  );
}

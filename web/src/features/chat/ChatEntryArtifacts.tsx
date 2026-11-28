import { Download, ExternalLink, Link2 } from "lucide-react";
import { ImageGallery } from "../../components/ImageGallery";
import { matchLiveWorkerForRef, resolveShortMachineName } from "../../lib/dispatchDefaults";
import { partitionEntryArtifacts, partitionEntryFiles, type GalleryImageItem } from "../../lib/imageGallery";
import { buildArtifactAPIURL, buildPublicPreviewURL, fileBaseName, formatByteSize } from "../../lib/previewLink";
import type { Entry, EntryWorkerRef, FileDrawerTarget, Machine, Worker } from "../../types/api";

function artifactBadgeLabel(kind: string): string {
  if (kind === "html") return "HTML";
  if (kind === "markdown") return "MD";
  if (kind === "image") return "IMG";
  if (kind === "text") return "TEXT";
  return "FILE";
}

interface Props {
  entry: Entry;
  workers: Worker[] | undefined;
  machines: Machine[];
  resumingWorkerKey: string;
  onSelectWorker: (paneId: string) => void;
  onOpenWorkerRecord: (ref: EntryWorkerRef) => void;
  onResumeWorker: (ref: EntryWorkerRef) => void;
  onOpenFile: (path: string, machine?: string) => void;
  onOpenArtifact: (target: FileDrawerTarget) => void;
  onOpenLightbox: (items: GalleryImageItem[], index: number) => void;
  onToast: (msg: string) => void;
}

export function ChatEntryArtifacts({
  entry,
  workers,
  machines,
  resumingWorkerKey,
  onSelectWorker,
  onOpenWorkerRecord,
  onResumeWorker,
  onOpenFile,
  onOpenArtifact,
  onOpenLightbox,
  onToast,
}: Props) {
  if (entry.artifacts && entry.artifacts.length > 0) {
    const { images: galleryImages, cards: artifactCards } = partitionEntryArtifacts(entry.artifacts, entry.worker_ref);
    return (
      <div className="space-y-2 pt-1">
        {galleryImages.length > 0 && (
          <ImageGallery
            items={galleryImages}
            onOpenLightbox={onOpenLightbox}
            onFallbackArtifact={(art) =>
              onOpenArtifact({ artifactId: art.id, name: art.name, kind: art.kind, shareToken: art.share_token })
            }
          />
        )}
        {artifactCards.map(({ art, isWorkerReport }) => {
          if (art.expired) {
            return (
              <div
                key={art.id}
                className="rounded-xl px-3 py-2 border border-hairline surface-rail flex items-center justify-between gap-2 text-xs"
              >
                <span className="font-mono-code text-muted line-through truncate">{art.name}</span>
                <span className="text-[10.5px] px-1.5 py-0.5 rounded border border-hairline surface-bg text-muted shrink-0">
                  快照已过期
                </span>
              </div>
            );
          }
          const workerRef = isWorkerReport ? entry.worker_ref : undefined;
          const liveBoundWorker = workerRef ? matchLiveWorkerForRef(workerRef, workers) : null;
          const openTarget: FileDrawerTarget = {
            artifactId: art.id,
            name: art.name,
            kind: art.kind,
            shareToken: art.share_token,
            workerRef: entry.worker_ref,
          };
          const newTabHref = art.share_token ? buildPublicPreviewURL(art.share_token) : buildArtifactAPIURL(art.id, "raw");
          const downloadHref = buildArtifactAPIURL(art.id, "download");
          const primaryLabel = art.kind === "markdown" || art.kind === "text" ? "查看完整报告" : "预览";
          const copyShare = () => {
            const full = `${window.location.origin}${buildPublicPreviewURL(art.share_token!)}`;
            void navigator.clipboard
              ?.writeText(full)
              .then(() => onToast("已复制在线预览链接"))
              .catch(() => onToast("复制失败"));
          };
          const actionIcons = (
            <div className="flex items-center gap-1 shrink-0">
              <a
                href={newTabHref}
                target="_blank"
                rel="noopener noreferrer"
                className="w-7 h-7 rounded-lg border border-hairline surface-bg surface-hover flex items-center justify-center text-secondary"
                title="在新标签页打开"
              >
                <ExternalLink className="w-3.5 h-3.5" strokeWidth={1.8} />
              </a>
              {art.share_token && (
                <button
                  type="button"
                  onClick={copyShare}
                  className="w-7 h-7 rounded-lg border border-hairline surface-bg surface-hover flex items-center justify-center text-secondary cursor-pointer"
                  title="复制在线预览链接"
                >
                  <Link2 className="w-3.5 h-3.5" strokeWidth={1.8} />
                </button>
              )}
              <a
                href={downloadHref}
                className="w-7 h-7 rounded-lg border border-hairline surface-bg surface-hover flex items-center justify-center text-secondary"
                title="下载原件"
              >
                <Download className="w-3.5 h-3.5" strokeWidth={1.8} />
              </a>
            </div>
          );

          if (workerRef) {
            const workerModelText = workerRef.model
              ? workerRef.effort
                ? `${workerRef.model} · ${workerRef.effort}`
                : workerRef.model
              : "";
            const workerMachineText = resolveShortMachineName(workerRef.machine ?? "", machines);
            const resumeKey = `${workerRef.name}:${workerRef.session ?? ""}`;
            const isResuming = Boolean(resumeKey && resumingWorkerKey === resumeKey);
            return (
              <div
                key={art.id}
                className="rounded-xl px-3.5 py-2.5 border border-strong surface-rail space-y-2.5 transition-colors"
              >
                <div className="flex items-start justify-between gap-2.5">
                  <button
                    type="button"
                    onClick={() => onOpenArtifact(openTarget)}
                    className="flex items-start gap-2.5 min-w-0 flex-1 text-left cursor-pointer group"
                  >
                    <span className="w-8 h-8 rounded-lg surface-bg border border-hairline flex items-center justify-center shrink-0 text-[10.5px] font-semibold font-mono-code text-secondary mt-0.5">
                      {artifactBadgeLabel(art.kind)}
                    </span>
                    <div className="min-w-0">
                      <div className="text-[13px] font-medium text-primary truncate group-hover:underline">{art.name}</div>
                      <div className="text-[11px] text-muted flex flex-wrap items-center gap-x-1.5 gap-y-0.5 mt-0.5">
                        <span className="font-mono-code">{formatByteSize(art.size) || "完整快照"}</span>
                        <span>·</span>
                        <span>
                          由 <strong className="font-medium text-secondary">{workerRef.name}</strong> 产出
                          {workerMachineText ? `（${workerMachineText}${workerModelText ? ` · ${workerModelText}` : ""}）` : ""}
                        </span>
                      </div>
                    </div>
                  </button>
                  <span
                    className={`text-[10.5px] px-2 py-0.5 rounded-full border shrink-0 ${
                      liveBoundWorker
                        ? "border-hairline surface-bg text-primary font-medium"
                        : "border-hairline surface-bg text-muted"
                    }`}
                  >
                    {liveBoundWorker ? "会话在线" : "已结束"}
                  </span>
                </div>
                <div className="pt-2 border-t border-hairline flex items-center justify-between gap-2 flex-wrap">
                  <div className="flex items-center gap-1.5 flex-wrap">
                    {art.kind !== "binary" && (
                      <button
                        type="button"
                        onClick={() => onOpenArtifact(openTarget)}
                        className="px-2.5 py-1 rounded-lg btn-accent text-[11.5px] font-medium cursor-pointer"
                      >
                        {primaryLabel}
                      </button>
                    )}
                    {liveBoundWorker?.pane_id ? (
                      <button
                        type="button"
                        onClick={() => onSelectWorker(liveBoundWorker.pane_id!)}
                        className="px-2.5 py-1 rounded-lg border border-strong surface-bg surface-hover text-[11.5px] font-medium text-primary cursor-pointer"
                      >
                        继续问这个 Worker →
                      </button>
                    ) : (
                      <>
                        <button
                          type="button"
                          onClick={() => onOpenWorkerRecord(workerRef)}
                          className="px-2.5 py-1 rounded-lg border border-strong surface-bg surface-hover text-[11.5px] font-medium text-secondary cursor-pointer"
                        >
                          查看记录
                        </button>
                        <button
                          type="button"
                          disabled={isResuming}
                          onClick={() => onResumeWorker(workerRef)}
                          className="px-2.5 py-1 rounded-lg border border-strong surface-bg surface-hover text-[11.5px] font-medium text-primary cursor-pointer disabled:opacity-50"
                        >
                          {isResuming ? "恢复中…" : "恢复并追问"}
                        </button>
                      </>
                    )}
                  </div>
                  {actionIcons}
                </div>
              </div>
            );
          }

          return (
            <div
              key={art.id}
              className="rounded-xl px-3 py-2 border border-hairline surface-rail flex items-center justify-between gap-2.5 transition-colors hover:border-[var(--line-2)]"
            >
              <button
                type="button"
                onClick={() => onOpenArtifact(openTarget)}
                className="flex items-center gap-2.5 min-w-0 flex-1 text-left cursor-pointer group"
              >
                <span className="w-8 h-8 rounded-lg surface-bg border border-hairline flex items-center justify-center shrink-0 text-[10.5px] font-semibold font-mono-code text-secondary">
                  {artifactBadgeLabel(art.kind)}
                </span>
                <div className="min-w-0">
                  <div className="text-[12.5px] font-medium text-primary truncate group-hover:underline">{art.name}</div>
                  <div className="text-[11px] text-muted font-mono-code">{formatByteSize(art.size) || "Hub 快照"}</div>
                </div>
              </button>
              <div className="flex items-center gap-1 shrink-0">
                {art.kind !== "binary" && (
                  <button
                    type="button"
                    onClick={() => onOpenArtifact(openTarget)}
                    className="px-2.5 py-1 rounded-lg border border-strong surface-bg surface-hover text-[11.5px] font-medium text-primary cursor-pointer"
                  >
                    {primaryLabel}
                  </button>
                )}
                {actionIcons}
              </div>
            </div>
          );
        })}
      </div>
    );
  }

  if (entry.files && entry.files.length > 0) {
    const { images: fileImages, nonImageFiles } = partitionEntryFiles(entry.files, entry.file_machine);
    return (
      <div className="space-y-1.5 pt-1">
        {fileImages.length > 0 && (
          <ImageGallery items={fileImages} onOpenLightbox={onOpenLightbox} onFallbackFile={onOpenFile} />
        )}
        {nonImageFiles.map((fp) => (
          <div
            key={fp}
            className="rounded-xl px-3 py-2 border border-hairline surface-rail flex items-center justify-between gap-2.5 text-xs"
          >
            <button
              type="button"
              onClick={() => onOpenFile(fp, entry.file_machine)}
              className="min-w-0 flex-1 text-left flex items-center gap-2 cursor-pointer group"
              title={fp}
            >
              <span className="font-mono-code text-primary truncate group-hover:underline">📄 {fileBaseName(fp)}</span>
              {entry.file_machine && (
                <span className="text-[10.5px] font-mono-code px-1.5 py-0.5 rounded border border-hairline surface-bg text-muted shrink-0">
                  {entry.file_machine}
                </span>
              )}
            </button>
            <button
              type="button"
              onClick={() => onOpenFile(fp, entry.file_machine)}
              className="px-2 py-0.5 rounded-lg border border-strong surface-bg surface-hover text-[11px] text-primary cursor-pointer shrink-0"
            >
              预览
            </button>
          </div>
        ))}
      </div>
    );
  }

  return null;
}

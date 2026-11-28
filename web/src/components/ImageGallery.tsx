import { useState } from "react";
import { Download } from "lucide-react";
import type { GalleryImageItem } from "../lib/imageGallery";
import { formatByteSize } from "../lib/previewLink";
import type { ArtifactSummary } from "../types/api";

const MAX_INLINE_IMAGES = 4;

interface Props {
  items: GalleryImageItem[];
  onOpenLightbox: (items: GalleryImageItem[], index: number) => void;
  onFallbackFile?: (path: string, machine?: string) => void;
  onFallbackArtifact?: (art: ArtifactSummary) => void;
}

export function ImageGallery({
  items,
  onOpenLightbox,
  onFallbackFile,
  onFallbackArtifact,
}: Props) {
  const [failedIds, setFailedIds] = useState<Set<string>>(() => new Set());

  if (items.length === 0) return null;

  const markFailed = (id: string) => {
    setFailedIds((prev) => {
      if (prev.has(id)) return prev;
      const next = new Set(prev);
      next.add(id);
      return next;
    });
  };

  const activeItems = items.filter((it) => !failedIds.has(it.id));
  const failedItems = items.filter((it) => failedIds.has(it.id));
  const visibleItems = activeItems.slice(0, MAX_INLINE_IMAGES);
  const overflowCount = Math.max(0, activeItems.length - MAX_INLINE_IMAGES);

  return (
    <div className="space-y-1.5 pt-1" data-image-gallery="true">
      {activeItems.length === 1 ? (
        <button
          type="button"
          onClick={() => onOpenLightbox(activeItems, 0)}
          title={activeItems[0].subtitle || activeItems[0].name}
          className="block w-fit max-w-full min-h-[140px] rounded-xl overflow-hidden surface-rail border border-hairline cursor-zoom-in text-left"
        >
          <img
            src={activeItems[0].src}
            alt={activeItems[0].name}
            loading="lazy"
            onError={() => markFailed(activeItems[0].id)}
            className="block max-w-full max-h-[320px] sm:max-h-[380px] w-auto h-auto object-contain"
          />
        </button>
      ) : activeItems.length > 1 ? (
        <div className="grid grid-cols-2 gap-1.5 max-w-[520px]" data-gallery-grid="true">
          {visibleItems.map((it, idx) => {
            const isOverflowTile = idx === MAX_INLINE_IMAGES - 1 && overflowCount > 0;
            return (
              <button
                key={it.id}
                type="button"
                onClick={() => onOpenLightbox(activeItems, idx)}
                title={it.subtitle || it.name}
                className="relative aspect-[4/3] w-full rounded-xl overflow-hidden surface-rail border border-hairline cursor-zoom-in block group"
              >
                <img
                  src={it.src}
                  alt={it.name}
                  loading="lazy"
                  onError={() => markFailed(it.id)}
                  className="w-full h-full object-cover object-top transition-opacity group-hover:opacity-90"
                />
                {isOverflowTile && (
                  <span
                    data-gallery-overflow={overflowCount}
                    className="absolute inset-0 flex items-center justify-center text-base font-semibold font-mono-code text-white"
                    style={{ background: "rgba(0,0,0,0.58)" }}
                  >
                    +{overflowCount}
                  </span>
                )}
              </button>
            );
          })}
        </div>
      ) : null}

      {failedItems.length > 0 && (
        <div className="space-y-1.5">
          {failedItems.map((it) => (
            <div
              key={it.id}
              data-gallery-fallback="true"
              className="rounded-xl px-3 py-2 border border-hairline surface-rail flex items-center justify-between gap-2.5 text-xs"
            >
              <button
                type="button"
                onClick={() => {
                  if (it.fallbackArtifact && onFallbackArtifact) onFallbackArtifact(it.fallbackArtifact);
                  else if (it.fallbackFile && onFallbackFile) onFallbackFile(it.fallbackFile.path, it.fallbackFile.machine);
                }}
                className="min-w-0 flex-1 text-left flex items-center gap-2 cursor-pointer group"
                title={it.subtitle || it.name}
              >
                <span className="font-mono-code text-primary truncate group-hover:underline">
                  {it.name}
                </span>
                <span className="text-[10.5px] text-muted shrink-0">
                  {it.size ? formatByteSize(it.size) : "图片暂不可预览"}
                </span>
              </button>
              {it.downloadUrl && (
                <a
                  href={it.downloadUrl}
                  className="inline-flex items-center gap-1 px-2 py-0.5 rounded-lg border border-strong surface-bg surface-hover text-[11px] text-primary shrink-0"
                  title="下载原件"
                >
                  <Download className="w-3 h-3" />
                  <span>下载</span>
                </a>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

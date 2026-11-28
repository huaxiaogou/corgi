import { buildArtifactAPIURL, buildFileAPIURL, fileBaseName, getPreviewFileKind } from "./previewLink";
import type { ArtifactSummary, EntryWorkerRef } from "../types/api";

export const LIGHTBOX_DAMP_FACTOR = 0.25;
export const LIGHTBOX_DAMP_MAX_PX = 48;

export interface GalleryImageItem {
  id: string;
  dedupKey: string;
  src: string;
  downloadUrl?: string;
  name: string;
  size?: number;
  subtitle?: string;
  fallbackFile?: { path: string; machine?: string };
  fallbackArtifact?: ArtifactSummary;
}

export interface PartitionedArtifacts {
  images: GalleryImageItem[];
  cards: { art: ArtifactSummary; isWorkerReport: boolean }[];
}

export interface PartitionedFiles {
  images: GalleryImageItem[];
  nonImageFiles: string[];
}

function normalizeFileDedupPath(raw: string): string {
  const trimmed = raw.trim();
  if (trimmed === "/") return "/";
  return trimmed.replace(/\/+$/, "");
}

/**
 * Splits entry.artifacts into valid inline gallery images vs regular/worker-report/expired cards.
 * Never swallows the primary Worker report card (idx === 0 with workerRef) or expired snapshots.
 * Deduplicates deterministically by artifact.id.
 */
export function partitionEntryArtifacts(
  artifacts: ArtifactSummary[],
  workerRef?: EntryWorkerRef
): PartitionedArtifacts {
  const images: GalleryImageItem[] = [];
  const cards: { art: ArtifactSummary; isWorkerReport: boolean }[] = [];
  const seenKeys = new Set<string>();

  artifacts.forEach((art, idx) => {
    const isWorkerReport = idx === 0 && Boolean(workerRef);
    const isImg = !art.expired && !isWorkerReport && (art.kind === "image" || getPreviewFileKind(art.name) === "image");
    if (!isImg) {
      cards.push({ art, isWorkerReport });
      return;
    }
    const dedupKey = `artifact:${art.id}`;
    if (seenKeys.has(dedupKey)) return;
    seenKeys.add(dedupKey);
    images.push({
      id: art.id,
      dedupKey,
      src: buildArtifactAPIURL(art.id, "raw"),
      downloadUrl: buildArtifactAPIURL(art.id, "download"),
      name: art.name,
      size: art.size,
      subtitle: "Hub 在线快照",
      fallbackArtifact: art,
    });
  });

  return { images, cards };
}

/**
 * Splits entry.files fallback paths into inline gallery images vs non-image file chips.
 * Deduplicates deterministically by machine + canonical full path (never filename alone).
 */
export function partitionEntryFiles(files: string[], machine?: string): PartitionedFiles {
  const images: GalleryImageItem[] = [];
  const nonImageFiles: string[] = [];
  const seenKeys = new Set<string>();
  const m = (machine ?? "").trim();

  for (const fp of files) {
    if (getPreviewFileKind(fp) !== "image") {
      nonImageFiles.push(fp);
      continue;
    }
    const norm = normalizeFileDedupPath(fp);
    const dedupKey = `file:${m}:${norm}`;
    if (seenKeys.has(dedupKey)) continue;
    seenKeys.add(dedupKey);
    const name = fileBaseName(fp) || fp;
    images.push({
      id: dedupKey,
      dedupKey,
      src: buildFileAPIURL({ path: fp, machine: m || undefined }, { as: "image" }),
      downloadUrl: buildFileAPIURL({ path: fp, machine: m || undefined }, { download: "1" }),
      name,
      subtitle: m ? `${m} · ${fp}` : fp,
      fallbackFile: { path: fp, machine: m || undefined },
    });
  }

  return { images, nonImageFiles };
}

/**
 * Computes horizontal drag offset in Lightbox.
 * Applies 0.25x resistance clamped at +/-48px on first/last boundary (no wrap-around).
 */
export function computeSwipeOffset(dx: number, index: number, total: number): number {
  if (dx === 0) return 0;
  const atStartEdge = index <= 0 && dx > 0;
  const atEndEdge = index >= total - 1 && dx < 0;
  if (total <= 1 || atStartEdge || atEndEdge) {
    const damped = Math.min(LIGHTBOX_DAMP_MAX_PX, Math.abs(dx) * LIGHTBOX_DAMP_FACTOR);
    return Math.sign(dx) * damped;
  }
  return dx;
}

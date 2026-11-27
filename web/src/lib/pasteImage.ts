export interface PasteSnapshot {
  files: File[];
  itemFiles: File[];
  html: string;
  plainText: string;
  hasMediaHint: boolean;
}

export interface AsyncClipboardLike {
  read?: () => Promise<Array<{ types: readonly string[]; getType: (type: string) => Promise<Blob> }>>;
}

export interface PasteRecoveryResult {
  files: File[];
  error?: string;
}

const HASH_SANDBOX_IMG_RE = /^[a-f0-9]{16,}\.(?:jpe?g|png|webp|gif)$/i;
const DATA_IMAGE_SRC_RE = /<img[^>]+src=["'](data:(image\/[a-z0-9.+-]+);base64,([^"']+))["']/gi;

function isImageFile(f: File): boolean {
  return (f.type || "").toLowerCase().startsWith("image/") || /\.(?:png|jpe?g|webp|gif|bmp)$/i.test(f.name || "");
}

function extForImageMime(mime: string): string {
  const sub = (mime.split("/")[1] || "png").toLowerCase().replace(/[^a-z0-9]/g, "");
  if (sub === "jpeg") return "jpg";
  return sub || "png";
}

/**
 * Synchronously captures clipboard files, items, HTML, and media hints inside a React onPaste event
 * BEFORE any async boundary (browsers clear DataTransfer once the paste event tick completes).
 */
export function capturePasteSnapshot(cd: DataTransfer | null | undefined): PasteSnapshot {
  if (!cd) {
    return { files: [], itemFiles: [], html: "", plainText: "", hasMediaHint: false };
  }
  const files = cd.files ? Array.from(cd.files) : [];
  const itemFiles: File[] = [];
  let hasImageItem = false;
  if (cd.items) {
    for (const item of Array.from(cd.items)) {
      if (item.kind === "file") {
        if ((item.type || "").toLowerCase().startsWith("image/")) {
          hasImageItem = true;
        }
        const f = item.getAsFile?.();
        if (f) itemFiles.push(f);
      }
    }
  }
  let html = "";
  let plainText = "";
  try {
    html = cd.getData?.("text/html") ?? "";
  } catch {
    html = "";
  }
  try {
    plainText = cd.getData?.("text/plain") ?? "";
  } catch {
    plainText = "";
  }
  const hasDataImage = /data:image\/[a-z0-9.+-]+;base64,/i.test(html);
  const hasMediaHint = files.length > 0 || itemFiles.length > 0 || hasImageItem || hasDataImage;
  return { files, itemFiles, html, plainText, hasMediaHint };
}

async function isFileReadable(file: File): Promise<boolean> {
  if (!file || file.size <= 0) return false;
  try {
    const slice = file.slice(0, Math.min(file.size, 16));
    if (typeof slice.arrayBuffer === "function") {
      const buf = await slice.arrayBuffer();
      return buf.byteLength > 0;
    }
    return true;
  } catch {
    return false;
  }
}

/**
 * Groups desktop messaging app duplicate paste candidates (e.g. macOS WeChat/WeCom pasting BOTH
 * a hash-named sandbox `.jpg` file reference and an in-memory `image.png` bitmap for one screenshot)
 * and falls back cleanly across:
 *   1. clipboardData files / items (readable candidates only, deduplicated)
 *   2. navigator.clipboard.read() across all image/* types
 *   3. text/html embedded data:image/*;base64 payloads
 */
export async function resolvePastedFiles(
  snap: PasteSnapshot,
  asyncClipboard?: AsyncClipboardLike | null
): Promise<PasteRecoveryResult> {
  // 1. Collect & deduplicate synchronous candidates (preferring itemFiles + files without double-counting).
  const raw: File[] = [];
  const seenKeys = new Set<string>();
  for (const f of [...snap.itemFiles, ...snap.files]) {
    if (!f) continue;
    const key = `${f.name}|${f.type}|${f.size}`;
    if (seenKeys.has(key)) continue;
    seenKeys.add(key);
    raw.push(f);
  }

  // Filter to readable files (macOS sandbox file references from messaging apps often fail read with NotReadableError).
  const readable: File[] = [];
  for (const f of raw) {
    if (await isFileReadable(f)) {
      readable.push(f);
    }
  }

  // Deduplicate desktop messaging app dual-representation screenshot pair:
  // a hash-named sandbox .jpg paired with an in-memory image/png bitmap from the same paste.
  const deduped = collapseDesktopDuplicatePair(readable);
  if (deduped.length > 0) {
    return { files: deduped };
  }

  // 2. Fallback to Async Clipboard API (navigator.clipboard.read) across all image/* types.
  if (snap.hasMediaHint && asyncClipboard?.read) {
    try {
      const clipItems = await asyncClipboard.read();
      const recovered: File[] = [];
      let idx = 0;
      for (const item of clipItems) {
        const imgType = item.types.find((t) => t.toLowerCase().startsWith("image/"));
        if (!imgType) continue;
        const blob = await item.getType(imgType);
        if (blob && blob.size > 0) {
          idx += 1;
          const ext = extForImageMime(imgType);
          recovered.push(new File([blob], `pasted-image-${idx}.${ext}`, { type: imgType }));
        }
      }
      if (recovered.length > 0) {
        return { files: recovered };
      }
    } catch {
      // Ignore permission errors and continue to HTML data URL fallback.
    }
  }

  // 3. Fallback to embedded data:image/*;base64 in text/html.
  if (snap.html) {
    const htmlFiles = extractDataImageFilesFromHTML(snap.html);
    if (htmlFiles.length > 0) {
      return { files: htmlFiles };
    }
  }

  if (snap.hasMediaHint) {
    return { files: [], error: "剪贴板里的图片已失效或无法读取，请重新截图或拖入文件" };
  }
  return { files: [] };
}

function collapseDesktopDuplicatePair(files: File[]): File[] {
  if (files.length !== 2) return files;
  const [a, b] = files;
  if (!isImageFile(a) || !isImageFile(b)) return files;
  const aHash = HASH_SANDBOX_IMG_RE.test(a.name || "");
  const bHash = HASH_SANDBOX_IMG_RE.test(b.name || "");
  if (aHash && !bHash) return [b];
  if (bHash && !aHash) return [a];
  return files;
}

export function extractDataImageFilesFromHTML(html: string): File[] {
  const out: File[] = [];
  DATA_IMAGE_SRC_RE.lastIndex = 0;
  let match: RegExpExecArray | null;
  let idx = 0;
  while ((match = DATA_IMAGE_SRC_RE.exec(html)) !== null) {
    const mime = match[2].toLowerCase();
    const b64 = match[3].replace(/\s+/g, "");
    try {
      const bytes = decodeBase64(b64);
      if (bytes.byteLength === 0) continue;
      idx += 1;
      const ext = extForImageMime(mime);
      const copy = new Uint8Array(bytes.byteLength);
      copy.set(bytes);
      out.push(new File([copy.buffer], `pasted-image-${idx}.${ext}`, { type: mime }));
    } catch {
      // Skip malformed base64 payload.
    }
  }
  return out;
}

function decodeBase64(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) {
    out[i] = bin.charCodeAt(i);
  }
  return out;
}

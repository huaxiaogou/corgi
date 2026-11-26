export type PreviewLinkTarget =
  | { kind: "file"; path: string; machine?: string }
  | { kind: "external"; href: string }
  | { kind: "unsafe" };

export type PreviewFileKind = "html" | "image" | "markdown" | "text";

const IMAGE_EXTS = new Set(["png", "jpg", "jpeg", "gif", "webp"]);
const TRAILING_PUNCT_RE = /[.,;:!?。，；：！？】》'"”’）]+$/;

export function fileHrefToPath(href: string): string {
  const withoutScheme = href
    .replace(/^file:\/\/(?:localhost)?/i, "")
    .replace(/[?#].*$/, "");
  try {
    return decodeURIComponent(withoutScheme);
  } catch {
    return withoutScheme;
  }
}

export function splitBareURLTrailing(raw: string): { url: string; trailing: string } {
  let url = raw;
  let trailing = "";
  const m = TRAILING_PUNCT_RE.exec(url);
  if (m) {
    trailing = m[0];
    url = url.slice(0, -m[0].length);
  }
  while (url.endsWith(")") || url.endsWith("]")) {
    const close = url[url.length - 1];
    const open = close === ")" ? "(" : "[";
    let balance = 0;
    for (const ch of url) {
      if (ch === open) balance++;
      else if (ch === close) balance--;
    }
    if (balance >= 0) break;
    trailing = close + trailing;
    url = url.slice(0, -1);
  }
  return { url, trailing };
}

function extractFileAPITarget(u: URL): PreviewLinkTarget | null {
  if (u.pathname !== "/api/file") return null;
  if (u.searchParams.get("download") === "1") return null;
  const rawPath = u.searchParams.get("path")?.trim() ?? "";
  if (!rawPath) return null;
  const machine = u.searchParams.get("machine")?.trim() || undefined;
  return { kind: "file", path: rawPath, machine };
}

/**
 * Classifies a link href against the current app origin.
 * Only local file paths (`file://`) and same-origin `/api/file` targets resolve
 * to in-app preview; all cross-origin HTTP(S) links stay external new-tab links;
 * non-HTTP/file schemes are rejected as unsafe.
 */
export function classifyPreviewLink(href: string, currentOrigin?: string): PreviewLinkTarget {
  const trimmed = href.trim();
  if (!trimmed) return { kind: "unsafe" };

  if (/^file:\/\//i.test(trimmed)) {
    const path = fileHrefToPath(trimmed).trim();
    if (path.startsWith("/") || path.startsWith("~/")) {
      return { kind: "file", path };
    }
    return { kind: "unsafe" };
  }

  if (trimmed.startsWith("/api/file?")) {
    try {
      const u = new URL(trimmed, "http://corgi.local");
      return extractFileAPITarget(u) ?? { kind: "unsafe" };
    } catch {
      return { kind: "unsafe" };
    }
  }

  if (!/^https?:\/\//i.test(trimmed)) {
    return { kind: "unsafe" };
  }

  try {
    const u = new URL(trimmed);
    if (u.protocol !== "http:" && u.protocol !== "https:") {
      return { kind: "unsafe" };
    }
    const origin = currentOrigin ?? (typeof window !== "undefined" ? window.location.origin : "");
    if (origin && u.origin === origin) {
      const inSite = extractFileAPITarget(u);
      if (inSite) return inSite;
    }
    return { kind: "external", href: u.toString() };
  } catch {
    return { kind: "unsafe" };
  }
}

export function getPreviewFileKind(filePath: string): PreviewFileKind {
  const clean = filePath.split(/[?#]/)[0].toLowerCase();
  const dot = clean.lastIndexOf(".");
  const ext = dot >= 0 ? clean.slice(dot + 1) : "";
  if (ext === "html" || ext === "htm") return "html";
  if (IMAGE_EXTS.has(ext)) return "image";
  if (ext === "md") return "markdown";
  return "text";
}

export function buildFileAPIURL(
  target: { path: string; machine?: string },
  extra?: Record<string, string>
): string {
  const qs = new URLSearchParams({ path: target.path });
  if (target.machine) qs.set("machine", target.machine);
  if (extra) {
    for (const [k, v] of Object.entries(extra)) {
      qs.set(k, v);
    }
  }
  return `/api/file?${qs.toString()}`;
}

export function buildArtifactAPIURL(id: string, subpath?: "raw" | "download"): string {
  const base = `/api/artifacts/${encodeURIComponent(id)}`;
  return subpath ? `${base}/${subpath}` : base;
}

export function buildPublicPreviewURL(shareToken: string): string {
  return `/p/${encodeURIComponent(shareToken)}/`;
}

export function formatByteSize(bytes?: number): string {
  if (!bytes || bytes <= 0) return "";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(2)} MB`;
}

export function fileBaseName(filePath: string): string {
  const parts = filePath.split("/").filter(Boolean);
  return parts[parts.length - 1] ?? filePath;
}

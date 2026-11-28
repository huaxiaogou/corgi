import type { ReactNode } from "react";
import { buildFileAPIURL, classifyPreviewLink, fileBaseName, splitBareURLTrailing } from "../../lib/previewLink";
import type { ArtifactSummary } from "../../types/api";

export interface MarkdownImagePayload {
  src: string;
  name: string;
  downloadUrl?: string;
  subtitle?: string;
}

export interface InlineContext {
  defaultMachine?: string;
  entryArtifacts?: ArtifactSummary[];
  onOpenFile?: (path: string, machine?: string) => void;
  onOpenArtifact?: (art: ArtifactSummary) => void;
  onOpenImage?: (img: MarkdownImagePayload) => void;
}

const FILE_EXTS = "tsx|ts|jsx|js|mjs|cjs|json|yaml|yml|html|htm|jpeg|jpg|png|webp|svg|css|scss|md|go|py|sh|sql|rs|rb|java|kt|cpp|hpp|cc|hh|toml|ini|txt|log|csv|c|h";
const BACKTICK_PATH_RE = new RegExp(`^(?:~\\/|\\/[\\p{L}\\p{N}._-]+\\/|[\\p{L}\\p{N}._-]+\\/)[\\p{L}\\p{N}./_-]+\\.(?:${FILE_EXTS})(?::\\d+(?:-\\d+)?)?$`, "u");
const INLINE_RE = new RegExp(
  `(` +
    `\\*\\*[^*\\n]+\\*\\*|` +
    `~~[^~\\n]+~~|` +
    `==[^=\\n]+==|` +
    `<kbd>[^<\\n]+<\\/kbd>|` +
    "`[^`\\n]+`|" +
    `\\[[^\\]]+\\]\\((?:(?:https?|file):\\/\\/[^)\\s]+|\\/api\\/file\\?[^)\\s]+)\\)|` +
    `\\[image:[^\\]]+\\]|` +
    `https?:\\/\\/[^\\s<>()[\\]"'\`]+|` +
    `\\/api\\/file\\?[^\\s<>()[\\]"'\`]+|` +
    `(?:~\\/|\\/[\\p{L}\\p{N}._-]+\\/)[\\p{L}\\p{N}./_-]+\\.(?:${FILE_EXTS})(?::\\d+(?:-\\d+)?)?(?![\\p{L}\\p{N}_])|` +
    `(?:(?:PR|Issue)\\s+#\\d+|[a-zA-Z0-9_.-]+\\/[a-zA-Z0-9_.-]+#\\d+)|` +
    `\\*[^*\\s\\n](?:[^*\\n]*[^*\\s\\n])?\\*` +
  `)`,
  "giu"
);

function stripLineRef(rawPath: string): string {
  return rawPath.replace(/:\d+(?:-\d+)?$/, "");
}

function matchArtifact(rawPath: string, artifacts?: ArtifactSummary[]): ArtifactSummary | undefined {
  if (!artifacts?.length) return undefined;
  const base = fileBaseName(stripLineRef(rawPath));
  return base ? artifacts.find((a) => !a.expired && a.name === base) : undefined;
}

function renderFileChip(key: number, label: string, rawPath: string, ctx: InlineContext, bordered = false): ReactNode {
  const cleanPath = stripLineRef(rawPath);
  const title = ctx.defaultMachine ? `${ctx.defaultMachine} · ${rawPath}` : rawPath;
  const cls = `font-mono-code text-[12.5px] px-1.5 py-0.5 rounded surface-rail ${bordered ? "border border-hairline " : ""}hover:underline cursor-pointer break-words [overflow-wrap:anywhere] text-left max-w-full`;
  const art = matchArtifact(cleanPath, ctx.entryArtifacts);
  if (art && ctx.onOpenArtifact) {
    return <button key={key} type="button" onClick={() => ctx.onOpenArtifact!(art)} title={title} className={cls}>{label}</button>;
  }
  if (ctx.onOpenFile) {
    return <button key={key} type="button" onClick={() => ctx.onOpenFile!(cleanPath, ctx.defaultMachine)} title={title} className={cls}>{label}</button>;
  }
  return <code key={key} title={title} className="font-mono-code text-[12.5px] px-1.5 py-0.5 rounded surface-rail border border-hairline break-words [overflow-wrap:anywhere] max-w-full">{label}</code>;
}

function renderLink(key: number, label: string, href: string, ctx: InlineContext): ReactNode {
  const target = classifyPreviewLink(href);
  if (target.kind === "file") {
    return renderFileChip(key, label, target.path, { ...ctx, defaultMachine: target.machine ?? ctx.defaultMachine });
  }
  if (target.kind === "external") {
    return (
      <a key={key} href={target.href} target="_blank" rel="noopener noreferrer" className="inline-flex items-baseline gap-0.5 underline underline-offset-2 text-primary hover:opacity-75">
        <span>{label}</span>
        <span aria-hidden="true" className="text-[11px] text-muted no-underline">↗</span>
      </a>
    );
  }
  return <span key={key}>{label}</span>;
}

export function renderInlineText(line: string, ctx: InlineContext): ReactNode[] {
  return line.split(INLINE_RE).map((part, idx) => {
    if (!part) return null;
    if (part.startsWith("**") && part.endsWith("**") && part.length > 4) {
      return <strong key={idx} className="font-semibold text-primary">{renderInlineText(part.slice(2, -2), ctx)}</strong>;
    }
    if (part.startsWith("~~") && part.endsWith("~~") && part.length > 4) {
      return <del key={idx} className="text-muted line-through">{renderInlineText(part.slice(2, -2), ctx)}</del>;
    }
    if (part.startsWith("==") && part.endsWith("==") && part.length > 4) {
      return <mark key={idx} className="md-mark">{renderInlineText(part.slice(2, -2), ctx)}</mark>;
    }
    if (/^<kbd>[^<\n]+<\/kbd>$/i.test(part)) {
      return <kbd key={idx} className="md-kbd">{part.slice(5, -6)}</kbd>;
    }
    if (part.startsWith("`") && part.endsWith("`") && part.length > 2) {
      const inner = part.slice(1, -1);
      if (BACKTICK_PATH_RE.test(inner)) {
        return renderFileChip(idx, inner, inner, ctx, true);
      }
      return <code key={idx} className="font-mono-code text-[12.5px] px-1.5 py-0.5 rounded surface-rail border border-hairline break-words [overflow-wrap:anywhere] max-w-full">{inner}</code>;
    }
    if (part.startsWith("[image:") && part.endsWith("]")) {
      const imgPath = part.slice(7, -1).trim();
      const src = buildFileAPIURL({ path: imgPath, machine: ctx.defaultMachine }, { as: "image" });
      const downloadUrl = buildFileAPIURL({ path: imgPath, machine: ctx.defaultMachine }, { download: "1" });
      const name = fileBaseName(imgPath) || imgPath;
      return (
        <button
          key={idx}
          type="button"
          onClick={() => {
            if (ctx.onOpenImage) {
              ctx.onOpenImage({ src, name, downloadUrl, subtitle: ctx.defaultMachine ? `${ctx.defaultMachine} · ${imgPath}` : imgPath });
            } else {
              ctx.onOpenFile?.(imgPath, ctx.defaultMachine);
            }
          }}
          className="block my-2 cursor-pointer"
          title="点击放大预览"
        >
          <img src={src} alt={name} className="max-h-56 rounded-xl border border-hairline" />
        </button>
      );
    }
    if (part.startsWith("[") && part.includes("](") && part.endsWith(")")) {
      const sep = part.indexOf("](");
      return renderLink(idx, part.slice(1, sep), part.slice(sep + 2, -1), ctx);
    }
    if (/^https?:\/\//i.test(part) || part.startsWith("/api/file?")) {
      const { url, trailing } = splitBareURLTrailing(part);
      const target = classifyPreviewLink(url);
      return <span key={idx}>{renderLink(idx, target.kind === "file" ? target.path : url, url, ctx)}{trailing}</span>;
    }
    if (part.startsWith("/") || part.startsWith("~/")) {
      return renderFileChip(idx, part, part, ctx);
    }
    if (/^(?:(?:PR|Issue)\s+#\d+|[a-zA-Z0-9_.-]+\/[a-zA-Z0-9_.-]+#\d+)$/i.test(part)) {
      return <span key={idx} className="inline-flex items-center px-1.5 py-0.5 rounded font-mono-code text-[11px] surface-rail border border-hairline text-secondary align-baseline">{part}</span>;
    }
    if (part.startsWith("*") && part.endsWith("*") && part.length > 2) {
      return <em key={idx} className="italic">{renderInlineText(part.slice(1, -1), ctx)}</em>;
    }
    return <span key={idx}>{part}</span>;
  });
}

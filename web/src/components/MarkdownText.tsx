import type { ReactNode } from "react";
import { classifyPreviewLink, fileBaseName, splitBareURLTrailing } from "../lib/previewLink";
import { normalizeEscapedNewlines } from "../lib/text";
import type { ArtifactSummary } from "../types/api";

interface Props {
  text: string;
  defaultMachine?: string;
  entryArtifacts?: ArtifactSummary[];
  onOpenFile?: (path: string, machine?: string) => void;
  onOpenArtifact?: (art: ArtifactSummary) => void;
}

const BACKTICK_PATH_RE = /^(?:~\/|\/[\p{L}\p{N}._-]+\/)[\p{L}\p{N}./_-]+\.(?:tsx|ts|jsx|js|mjs|cjs|json|yaml|yml|html|htm|jpeg|jpg|png|webp|svg|css|scss|md|go|py|sh|sql|rs|rb|java|kt|cpp|hpp|cc|hh|toml|ini|txt|log|csv|c|h)$/u;

const INLINE_RE =
  /(\*\*[^*\n]+\*\*|`[^`\n]+`|\[[^\]]+\]\((?:(?:https?|file):\/\/[^)\s]+|\/api\/file\?[^)\s]+)\)|\[image:[^\]]+\]|https?:\/\/[^\s<>()[\]"'`]+|\/api\/file\?[^\s<>()[\]"'`]+|(?:~\/|\/[\p{L}\p{N}._-]+\/)[\p{L}\p{N}./_-]+\.(?:tsx|ts|jsx|js|mjs|cjs|json|yaml|yml|html|htm|jpeg|jpg|png|webp|svg|css|scss|md|go|py|sh|sql|rs|rb|java|kt|cpp|hpp|cc|hh|toml|ini|txt|log|csv|c|h)(?![\p{L}\p{N}_]))/gu;

const UNORDERED_ITEM_RE = /^\s*[-*•]\s+/;
const ORDERED_ITEM_RE = /^\s*(\d+)(?:[.)]\s+|）\s*)(.+)$/;

function matchArtifactForPath(rawPath: string, artifacts?: ArtifactSummary[]): ArtifactSummary | undefined {
  if (!artifacts || artifacts.length === 0) return undefined;
  const base = fileBaseName(rawPath);
  if (!base) return undefined;
  return artifacts.find((a) => !a.expired && a.name === base);
}

interface InlineCtx {
  defaultMachine?: string;
  entryArtifacts?: ArtifactSummary[];
  onOpenFile?: (path: string, machine?: string) => void;
  onOpenArtifact?: (art: ArtifactSummary) => void;
}

function renderLinkNode(key: number, label: string, href: string, ctx: InlineCtx): ReactNode {
  const target = classifyPreviewLink(href);
  if (target.kind === "file") {
    const machine = target.machine ?? ctx.defaultMachine;
    const title = machine ? `${machine} · ${target.path}` : target.path;
    const matchedArt = matchArtifactForPath(target.path, ctx.entryArtifacts);
    if (matchedArt && ctx.onOpenArtifact) {
      return (
        <button
          key={key}
          type="button"
          onClick={() => ctx.onOpenArtifact!(matchedArt)}
          title={title}
          className="font-mono-code text-[12.5px] px-1.5 py-0.5 rounded surface-rail hover:underline cursor-pointer mx-0.5 break-words [overflow-wrap:anywhere] text-left max-w-full"
        >
          {label}
        </button>
      );
    }
    if (ctx.onOpenFile) {
      return (
        <button
          key={key}
          type="button"
          onClick={() => ctx.onOpenFile!(target.path, machine)}
          title={title}
          className="font-mono-code text-[12.5px] px-1.5 py-0.5 rounded surface-rail hover:underline cursor-pointer mx-0.5 break-words [overflow-wrap:anywhere] text-left max-w-full"
        >
          {label}
        </button>
      );
    }
    return (
      <code key={key} title={title} className="font-mono-code text-[12.5px] px-1.5 py-0.5 rounded surface-rail border border-hairline break-words [overflow-wrap:anywhere] max-w-full">
        {label}
      </code>
    );
  }
  if (target.kind === "external") {
    return (
      <a
        key={key}
        href={target.href}
        target="_blank"
        rel="noopener noreferrer"
        className="inline-flex items-baseline gap-0.5 underline underline-offset-2 text-primary hover:opacity-75"
      >
        <span>{label}</span>
        <span aria-hidden="true" className="text-[11px] text-muted no-underline">
          ↗
        </span>
      </a>
    );
  }
  return <span key={key}>{label}</span>;
}

function renderInline(line: string, ctx: InlineCtx): ReactNode[] {
  const parts = line.split(INLINE_RE);
  return parts.map((part, idx) => {
    if (!part) return null;
    if (part.startsWith("**") && part.endsWith("**") && part.length > 4) {
      return (
        <strong key={idx} className="font-semibold text-primary">
          {renderInline(part.slice(2, -2), ctx)}
        </strong>
      );
    }
    if (part.startsWith("`") && part.endsWith("`") && part.length > 2) {
      const inner = part.slice(1, -1);
      if (BACKTICK_PATH_RE.test(inner)) {
        const matchedArt = matchArtifactForPath(inner, ctx.entryArtifacts);
        if (matchedArt && ctx.onOpenArtifact) {
          return (
            <button
              key={idx}
              type="button"
              onClick={() => ctx.onOpenArtifact!(matchedArt)}
              title={inner}
              className="font-mono-code text-[12.5px] px-1.5 py-0.5 rounded surface-rail border border-hairline hover:underline cursor-pointer break-words [overflow-wrap:anywhere] text-left max-w-full"
            >
              {inner}
            </button>
          );
        }
        if (ctx.onOpenFile) {
          return (
            <button
              key={idx}
              type="button"
              onClick={() => ctx.onOpenFile!(inner, ctx.defaultMachine)}
              title={ctx.defaultMachine ? `${ctx.defaultMachine} · ${inner}` : inner}
              className="font-mono-code text-[12.5px] px-1.5 py-0.5 rounded surface-rail border border-hairline hover:underline cursor-pointer break-words [overflow-wrap:anywhere] text-left max-w-full"
            >
              {inner}
            </button>
          );
        }
      }
      return (
        <code key={idx} className="font-mono-code text-[12.5px] px-1.5 py-0.5 rounded surface-rail border border-hairline break-words [overflow-wrap:anywhere] max-w-full">
          {inner}
        </code>
      );
    }
    if (part.startsWith("[image:") && part.endsWith("]")) {
      const imgPath = part.slice(7, -1);
      return (
        <button
          key={idx}
          type="button"
          onClick={() => ctx.onOpenFile?.(imgPath, ctx.defaultMachine)}
          className="block my-2 cursor-pointer"
          title="点击放大预览"
        >
          <img
            src={`/api/file?path=${encodeURIComponent(imgPath)}&as=image`}
            alt={imgPath}
            className="max-h-56 rounded-xl border border-hairline"
          />
        </button>
      );
    }
    if (part.startsWith("[") && part.includes("](") && part.endsWith(")")) {
      const sep = part.indexOf("](");
      const label = part.slice(1, sep);
      const href = part.slice(sep + 2, -1);
      return renderLinkNode(idx, label, href, ctx);
    }
    if (/^https?:\/\//i.test(part) || part.startsWith("/api/file?")) {
      const { url, trailing } = splitBareURLTrailing(part);
      const target = classifyPreviewLink(url);
      const displayLabel = target.kind === "file" ? target.path : url;
      return (
        <span key={idx}>
          {renderLinkNode(idx, displayLabel, url, ctx)}
          {trailing}
        </span>
      );
    }
    if (part.startsWith("/") || part.startsWith("~/")) {
      const matchedArt = matchArtifactForPath(part, ctx.entryArtifacts);
      if (matchedArt && ctx.onOpenArtifact) {
        return (
          <button
            key={idx}
            type="button"
            onClick={() => ctx.onOpenArtifact!(matchedArt)}
            className="font-mono-code text-[12.5px] px-1.5 py-0.5 rounded surface-rail hover:underline cursor-pointer mx-0.5"
          >
            {part}
          </button>
        );
      }
      if (ctx.onOpenFile) {
        return (
          <button
            key={idx}
            type="button"
            onClick={() => ctx.onOpenFile!(part, ctx.defaultMachine)}
            className="font-mono-code text-[12.5px] px-1.5 py-0.5 rounded surface-rail hover:underline cursor-pointer mx-0.5"
          >
            {part}
          </button>
        );
      }
    }
    return <span key={idx}>{part}</span>;
  });
}

export function MarkdownText({ text, defaultMachine, entryArtifacts, onOpenFile, onOpenArtifact }: Props) {
  const ctx: InlineCtx = { defaultMachine, entryArtifacts, onOpenFile, onOpenArtifact };
  const lines = normalizeEscapedNewlines(text).replace(/\r\n/g, "\n").split("\n");
  const blocks: ReactNode[] = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    // Fenced code block
    if (line.trimStart().startsWith("```")) {
      const lang = line.trimStart().slice(3).trim();
      const codeLines: string[] = [];
      i++;
      while (i < lines.length && !lines[i].trimStart().startsWith("```")) {
        codeLines.push(lines[i]);
        i++;
      }
      i++;
      blocks.push(
        <div key={blocks.length} className="my-2 rounded-xl border border-hairline surface-rail overflow-hidden">
          {lang && <div className="px-3 py-1 text-[10.5px] font-mono-code text-muted border-b border-hairline">{lang}</div>}
          <pre className="p-3 text-[12.5px] font-mono-code overflow-x-auto leading-relaxed text-secondary">{codeLines.join("\n")}</pre>
        </div>
      );
      continue;
    }

    // Markdown table
    if (line.includes("|") && i + 1 < lines.length && /^\s*\|?[\s:-]+\|[\s|:-]*$/.test(lines[i + 1])) {
      const parseRow = (r: string) =>
        r
          .trim()
          .replace(/^\|/, "")
          .replace(/\|$/, "")
          .split("|")
          .map((c) => c.trim());
      const headers = parseRow(line);
      i += 2;
      const rows: string[][] = [];
      while (i < lines.length && lines[i].includes("|") && lines[i].trim() !== "") {
        rows.push(parseRow(lines[i]));
        i++;
      }
      blocks.push(
        <div key={blocks.length} className="my-2 overflow-x-auto rounded-xl border border-hairline">
          <table className="w-full text-left text-xs border-collapse">
            <thead>
              <tr className="surface-rail border-b border-hairline">
                {headers.map((h, hi) => (
                  <th key={hi} className="px-3 py-2 font-semibold text-primary align-top break-words [overflow-wrap:anywhere]">
                    {renderInline(h, ctx)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y divide-[var(--line)]">
              {rows.map((r, ri) => (
                <tr key={ri}>
                  {r.map((c, ci) => (
                    <td key={ci} className="px-3 py-2 text-secondary align-top break-words [overflow-wrap:anywhere]">
                      {renderInline(c, ctx)}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      );
      continue;
    }

    // Heading
    const heading = /^(#{1,3})\s+(.+)$/.exec(line.trim());
    if (heading) {
      const level = heading[1].length;
      const cls = level === 1 ? "text-[16px] font-semibold mt-3 mb-1" : level === 2 ? "text-[15px] font-semibold mt-2.5 mb-1" : "text-[14px] font-semibold mt-2";
      blocks.push(
        <div key={blocks.length} className={cls}>
          {renderInline(heading[2], ctx)}
        </div>
      );
      i++;
      continue;
    }

    // Unordered list (-, *, •)
    if (UNORDERED_ITEM_RE.test(line)) {
      const items: string[] = [];
      while (i < lines.length && UNORDERED_ITEM_RE.test(lines[i])) {
        items.push(lines[i].replace(UNORDERED_ITEM_RE, ""));
        i++;
      }
      blocks.push(
        <ul key={blocks.length} className="my-1.5 pl-5 space-y-1 list-disc">
          {items.map((it, idx) => (
            <li key={idx} className="leading-[1.65]">
              {renderInline(it, ctx)}
            </li>
          ))}
        </ul>
      );
      continue;
    }

    // Ordered list (1. / 1) / 1）)
    const firstOrdered = ORDERED_ITEM_RE.exec(line);
    if (firstOrdered) {
      const startNum = Number.parseInt(firstOrdered[1], 10);
      const items: string[] = [];
      while (i < lines.length) {
        const m = ORDERED_ITEM_RE.exec(lines[i]);
        if (!m) break;
        items.push(m[2]);
        i++;
      }
      blocks.push(
        <ol
          key={blocks.length}
          start={Number.isFinite(startNum) && startNum !== 1 ? startNum : undefined}
          className="my-1.5 pl-5 space-y-1 list-decimal"
        >
          {items.map((it, idx) => (
            <li key={idx} className="leading-[1.65]">
              {renderInline(it, ctx)}
            </li>
          ))}
        </ol>
      );
      continue;
    }

    if (line.trim() === "") {
      i++;
      continue;
    }

    blocks.push(
      <p key={blocks.length} className="leading-[1.7] whitespace-pre-wrap">
        {renderInline(line, ctx)}
      </p>
    );
    i++;
  }

  return <div className="space-y-1.5">{blocks}</div>;
}

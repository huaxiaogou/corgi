import type { ReactNode } from "react";

interface Props {
  text: string;
  onOpenFile?: (path: string) => void;
}

const INLINE_RE =
  /(\*\*[^*\n]+\*\*|`[^`\n]+`|\[[^\]]+\]\(https?:\/\/[^)\s]+\)|\[image:[^\]]+\]|(?:~\/|\/[\w.-]+\/)[\w./-]+\.(?:md|go|ts|tsx|js|json|sql|py|sh|yaml|yml|html|htm|png|jpg|jpeg|webp))/g;

function renderInline(line: string, onOpenFile?: (path: string) => void): ReactNode[] {
  const parts = line.split(INLINE_RE);
  return parts.map((part, idx) => {
    if (!part) return null;
    if (part.startsWith("**") && part.endsWith("**") && part.length > 4) {
      return (
        <strong key={idx} className="font-semibold text-primary">
          {part.slice(2, -2)}
        </strong>
      );
    }
    if (part.startsWith("`") && part.endsWith("`") && part.length > 2) {
      return (
        <code key={idx} className="font-mono-code text-[12.5px] px-1.5 py-0.5 rounded surface-rail border border-hairline">
          {part.slice(1, -1)}
        </code>
      );
    }
    if (part.startsWith("[image:") && part.endsWith("]")) {
      const imgPath = part.slice(7, -1);
      return (
        <button
          key={idx}
          type="button"
          onClick={() => onOpenFile?.(imgPath)}
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
      return (
        <a key={idx} href={href} target="_blank" rel="noreferrer" className="underline underline-offset-2 text-primary">
          {label}
        </a>
      );
    }
    if ((part.startsWith("/") || part.startsWith("~/")) && onOpenFile) {
      return (
        <button
          key={idx}
          type="button"
          onClick={() => onOpenFile(part)}
          className="font-mono-code text-[12.5px] px-1.5 py-0.5 rounded surface-rail hover:underline cursor-pointer mx-0.5"
        >
          {part}
        </button>
      );
    }
    return <span key={idx}>{part}</span>;
  });
}

export function MarkdownText({ text, onOpenFile }: Props) {
  const lines = text.replace(/\r\n/g, "\n").split("\n");
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
                  <th key={hi} className="px-3 py-2 font-semibold text-primary">
                    {renderInline(h, onOpenFile)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y divide-[var(--line)]">
              {rows.map((r, ri) => (
                <tr key={ri}>
                  {r.map((c, ci) => (
                    <td key={ci} className="px-3 py-2 text-secondary">
                      {renderInline(c, onOpenFile)}
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
          {renderInline(heading[2], onOpenFile)}
        </div>
      );
      i++;
      continue;
    }

    // Bullet / numbered list
    if (/^\s*([-*•]|\d+\.)\s+/.test(line)) {
      const items: string[] = [];
      while (i < lines.length && /^\s*([-*•]|\d+\.)\s+/.test(lines[i])) {
        items.push(lines[i].replace(/^\s*([-*•]|\d+\.)\s+/, ""));
        i++;
      }
      blocks.push(
        <ul key={blocks.length} className="my-1.5 pl-4 space-y-1 list-disc">
          {items.map((it, idx) => (
            <li key={idx} className="leading-[1.65]">
              {renderInline(it, onOpenFile)}
            </li>
          ))}
        </ul>
      );
      continue;
    }

    if (line.trim() === "") {
      i++;
      continue;
    }

    blocks.push(
      <p key={blocks.length} className="leading-[1.7] whitespace-pre-wrap">
        {renderInline(line, onOpenFile)}
      </p>
    );
    i++;
  }

  return <div className="space-y-1.5">{blocks}</div>;
}

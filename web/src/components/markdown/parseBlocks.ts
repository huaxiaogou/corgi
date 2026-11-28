export type CalloutKind = "note" | "tip" | "important" | "warning" | "caution";
export type StepStatus = "done" | "active" | "warn" | "todo";

export interface MetricItem {
  label: string;
  value: string;
  delta?: string;
}

export interface StepItem {
  status: StepStatus;
  text: string;
}

export interface ListItem {
  text: string;
  checked?: boolean;
}

export type MarkdownBlock =
  | { type: "code"; lang: string; code: string }
  | { type: "metrics"; items: MetricItem[] }
  | { type: "steps"; items: StepItem[] }
  | { type: "callout"; kind: CalloutKind; title?: string; blocks: MarkdownBlock[] }
  | { type: "blockquote"; blocks: MarkdownBlock[] }
  | { type: "details"; summary: string; blocks: MarkdownBlock[] }
  | { type: "table"; headers: string[]; rows: string[][]; numericCols: boolean[] }
  | { type: "heading"; level: 1 | 2 | 3 | 4; text: string }
  | { type: "list"; ordered: boolean; start?: number; items: ListItem[] }
  | { type: "hr" }
  | { type: "paragraph"; text: string };

const UNORDERED_RE = /^\s*[-*•]\s+(.*)$/;
const TASK_RE = /^\[( |x|X)\]\s+(.*)$/;
const ORDERED_RE = /^\s*(\d+)(?:[.)]\s+|）\s*)(.+)$/;
const TABLE_SEP_RE = /^\s*\|?[\s:-]+\|[\s|:-]*$/;
const HEADING_RE = /^(#{1,4})\s+(.+)$/;
const HR_RE = /^\s*(?:---+|\*\*\*+|___+)\s*$/;
const CALLOUT_RE = /^\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\](?:\s+(.*))?$/i;
const DETAILS_OPEN_RE = /^<details(?:\s[^>]*)?>\s*(.*)$/i;
const DETAILS_CLOSE_RE = /<\/details>/i;
const SUMMARY_RE = /<summary(?:\s[^>]*)?>([\s\S]*?)<\/summary>/i;
const NUMERIC_CELL_RE = /^[+-]?(?:¥|\$|€)?\d[\d,]*(?:\.\d+)?(?:\s*(?:%|ms|s|KiB|MiB|GiB|KB|MB|GB|B|x|×))?$/;

function matchFenceOpen(line: string): { ch: string; len: number; info: string } | null {
  const trimmed = line.trimStart();
  const ch = trimmed[0];
  if (ch !== "`" && ch !== "~") return null;
  let len = 0;
  while (len < trimmed.length && trimmed[len] === ch) len++;
  if (len < 3) return null;
  const info = trimmed.slice(len).trim();
  if (ch === "`" && info.includes("`")) return null;
  return { ch, len, info };
}

function isFenceClose(line: string, ch: string, minLen: number): boolean {
  const trimmed = line.trim();
  if (trimmed.length < minLen) return false;
  for (let i = 0; i < trimmed.length; i++) {
    if (trimmed[i] !== ch) return false;
  }
  return true;
}

function parseTableRow(line: string): string[] {
  return line.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((c) => c.trim());
}

function detectNumericColumns(colCount: number, rows: string[][]): boolean[] {
  return Array.from({ length: colCount }, (_, ci) => {
    let seen = 0;
    for (const r of rows) {
      const cell = (r[ci] ?? "").trim();
      if (!cell || cell === "-" || cell === "—" || cell === "N/A") continue;
      seen++;
      if (!NUMERIC_CELL_RE.test(cell)) return false;
    }
    return seen > 0;
  });
}

function parseMetrics(lines: string[]): MetricItem[] {
  const items: MetricItem[] = [];
  for (const raw of lines) {
    const s = raw.trim();
    if (!s) continue;
    const parts = s.split("|").map((p) => p.trim());
    if (parts.length >= 2 && parts[0]) {
      items.push({ label: parts[0], value: parts[1], delta: parts[2] || undefined });
    }
  }
  return items;
}

function parseSteps(lines: string[]): StepItem[] {
  const items: StepItem[] = [];
  for (const raw of lines) {
    const s = raw.trim().replace(/^[-*•]\s+/, "");
    if (!s) continue;
    if (/^\[x\]\s*/i.test(s)) items.push({ status: "done", text: s.replace(/^\[x\]\s*/i, "") });
    else if (/^\[~\]\s*/.test(s)) items.push({ status: "active", text: s.replace(/^\[~\]\s*/, "") });
    else if (/^\[!\]\s*/.test(s)) items.push({ status: "warn", text: s.replace(/^\[!\]\s*/, "") });
    else if (/^\[\s?\]\s*/.test(s)) items.push({ status: "todo", text: s.replace(/^\[\s?\]\s*/, "") });
    else items.push({ status: "todo", text: s });
  }
  return items;
}

export function parseMarkdownBlocks(rawText: string, depth = 0): MarkdownBlock[] {
  const lines = rawText.replace(/\r\n/g, "\n").split("\n");
  const blocks: MarkdownBlock[] = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    // 1. CommonMark §4.5 multi-backtick / tilde fenced blocks
    const fence = matchFenceOpen(line);
    if (fence) {
      const lang = fence.info.split(/\s+/)[0]?.toLowerCase() ?? "";
      const codeLines: string[] = [];
      i++;
      while (i < lines.length && !isFenceClose(lines[i], fence.ch, fence.len)) {
        codeLines.push(lines[i]);
        i++;
      }
      if (i < lines.length) i++;
      if (lang === "metrics") {
        const items = parseMetrics(codeLines);
        if (items.length > 0) {
          blocks.push({ type: "metrics", items });
          continue;
        }
      }
      if (lang === "steps") {
        const items = parseSteps(codeLines);
        if (items.length > 0) {
          blocks.push({ type: "steps", items });
          continue;
        }
      }
      blocks.push({ type: "code", lang, code: codeLines.join("\n") });
      continue;
    }

    // 2. Whitelisted <details><summary>...</summary>...</details> (pure structural AST, 0 raw HTML)
    if (depth < 3 && DETAILS_OPEN_RE.test(line.trim())) {
      const chunk: string[] = [];
      while (i < lines.length) {
        chunk.push(lines[i]);
        const closed = DETAILS_CLOSE_RE.test(lines[i]);
        i++;
        if (closed) break;
      }
      const joined = chunk.join("\n").trim().replace(/^<details(?:\s[^>]*)?>/i, "").replace(/<\/details>\s*$/i, "");
      const sm = SUMMARY_RE.exec(joined);
      const summary = sm ? sm[1].trim() || "展开详情" : "展开详情";
      const body = (sm ? joined.slice(0, sm.index) + joined.slice(sm.index + sm[0].length) : joined).trim();
      blocks.push({ type: "details", summary, blocks: body ? parseMarkdownBlocks(body, depth + 1) : [] });
      continue;
    }

    // 3. Callouts (> [!NOTE|TIP|IMPORTANT|WARNING|CAUTION]) and standard blockquotes
    if (/^\s*>\s?/.test(line)) {
      const quoted: string[] = [];
      while (i < lines.length && /^\s*>\s?/.test(lines[i])) {
        quoted.push(lines[i].replace(/^\s*>\s?/, ""));
        i++;
      }
      const cm = CALLOUT_RE.exec(quoted[0]?.trim() ?? "");
      if (cm && depth < 3) {
        const kind = cm[1].toLowerCase() as CalloutKind;
        const title = cm[2]?.trim() || undefined;
        const inner = quoted.slice(1).join("\n").trim();
        blocks.push({ type: "callout", kind, title, blocks: inner ? parseMarkdownBlocks(inner, depth + 1) : [] });
      } else {
        const inner = quoted.join("\n").trim();
        blocks.push({ type: "blockquote", blocks: inner && depth < 3 ? parseMarkdownBlocks(inner, depth + 1) : [{ type: "paragraph", text: inner }] });
      }
      continue;
    }

    // 4. Markdown tables
    if (line.includes("|") && i + 1 < lines.length && TABLE_SEP_RE.test(lines[i + 1])) {
      const headers = parseTableRow(line);
      i += 2;
      const rows: string[][] = [];
      while (i < lines.length && lines[i].includes("|") && lines[i].trim() !== "") {
        rows.push(parseTableRow(lines[i]));
        i++;
      }
      blocks.push({ type: "table", headers, rows, numericCols: detectNumericColumns(headers.length, rows) });
      continue;
    }

    // 5. Horizontal rule
    if (HR_RE.test(line)) {
      blocks.push({ type: "hr" });
      i++;
      continue;
    }

    // 6. Headings
    const hm = HEADING_RE.exec(line.trim());
    if (hm) {
      blocks.push({ type: "heading", level: hm[1].length as 1 | 2 | 3 | 4, text: hm[2] });
      i++;
      continue;
    }

    // 7. Unordered & task lists
    if (UNORDERED_RE.test(line)) {
      const items: ListItem[] = [];
      while (i < lines.length) {
        const um = UNORDERED_RE.exec(lines[i]);
        if (!um) break;
        const tm = TASK_RE.exec(um[1]);
        if (tm) items.push({ text: tm[2], checked: tm[1].toLowerCase() === "x" });
        else items.push({ text: um[1] });
        i++;
      }
      blocks.push({ type: "list", ordered: false, items });
      continue;
    }

    // 8. Ordered lists (1. / 1) / 1）)
    const om = ORDERED_RE.exec(line);
    if (om) {
      const startNum = Number.parseInt(om[1], 10);
      const items: ListItem[] = [];
      while (i < lines.length) {
        const m = ORDERED_RE.exec(lines[i]);
        if (!m) break;
        items.push({ text: m[2] });
        i++;
      }
      blocks.push({ type: "list", ordered: true, start: Number.isFinite(startNum) && startNum !== 1 ? startNum : undefined, items });
      continue;
    }

    if (line.trim() === "") {
      i++;
      continue;
    }

    blocks.push({ type: "paragraph", text: line });
    i++;
  }

  return blocks;
}

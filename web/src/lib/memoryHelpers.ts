import type { MemoryFile } from "../types/api";

export interface InstructionItem {
  lineIndex: number;
  raw: string;
  text: string;
  inferred: boolean;
}

export interface ParsedInstructions {
  explicit: InstructionItem[];
  inferred: InstructionItem[];
}

export interface DreamSummary {
  lead: string;
  bullets: string[];
}

export interface MemorySearchHit {
  name: string;
  matches: { line: number; text: string }[];
}

const LIST_ITEM_RE = /^\s*(?:[-*+]|\d+[.)])\s+(.*)$/;
const INFERRED_RE = /(?:\(|\（)\s*(?:inferred|推断)\s*(?:\)|\）)/i;

/**
 * Parses instructions.md list items into explicit rules vs AI-inferred rules
 * without mutating the underlying Markdown file format.
 */
export function parseInstructions(text: string): ParsedInstructions {
  const explicit: InstructionItem[] = [];
  const inferred: InstructionItem[] = [];
  const lines = text.split(/\r?\n/);

  for (let i = 0; i < lines.length; i++) {
    const raw = lines[i];
    const m = LIST_ITEM_RE.exec(raw);
    if (!m) continue;
    const body = m[1].trim();
    if (!body) continue;
    const isInferred = INFERRED_RE.test(body);
    const cleanText = isInferred ? body.replace(INFERRED_RE, "").replace(/\s{2,}/g, " ").trim() : body;
    const item: InstructionItem = {
      lineIndex: i,
      raw,
      text: cleanText || body,
      inferred: isInferred,
    };
    if (isInferred) {
      inferred.push(item);
    } else {
      explicit.push(item);
    }
  }
  return { explicit, inferred };
}

/**
 * Removes one exact line (0-based) from a Markdown document while preserving all other lines.
 */
export function removeInstructionLine(text: string, lineIndex: number): string {
  const lines = text.split(/\r?\n/);
  if (lineIndex < 0 || lineIndex >= lines.length) return text;
  lines.splice(lineIndex, 1);
  return lines.join("\n").replace(/\n{3,}/g, "\n\n").trimEnd() + "\n";
}

/**
 * Extracts a concise lead sentence and up to 3 bullet lines from a dream journal.
 */
export function extractDreamSummary(text: string): DreamSummary {
  let lead = "";
  const bullets: string[] = [];
  for (const raw of text.split(/\r?\n/)) {
    const t = raw.trim();
    if (!t || t.startsWith("#")) continue;
    const listMatch = LIST_ITEM_RE.exec(raw);
    if (!lead && !listMatch) {
      lead = t.replace(/^>\s*/, "").trim();
      continue;
    }
    if (listMatch) {
      const b = listMatch[1].trim();
      if (!b) continue;
      if (!lead) {
        lead = b;
      } else if (bullets.length < 3) {
        bullets.push(b);
      }
      continue;
    }
    if (bullets.length < 3) {
      bullets.push(t);
    }
  }
  return { lead, bullets };
}

/**
 * Searches memory file names and lines case-insensitively.
 */
export function searchMemoryFiles(files: MemoryFile[], query: string): MemorySearchHit[] {
  const q = query.trim().toLowerCase();
  if (!q) return [];
  const hits: MemorySearchHit[] = [];
  for (const f of files) {
    const nameMatch = f.name.toLowerCase().includes(q);
    const matches: { line: number; text: string }[] = [];
    const lines = f.text.split(/\r?\n/);
    for (let i = 0; i < lines.length; i++) {
      if (lines[i].toLowerCase().includes(q)) {
        matches.push({ line: i + 1, text: lines[i].trim() });
        if (matches.length >= 3) break;
      }
    }
    if (nameMatch || matches.length > 0) {
      hits.push({ name: f.name, matches });
    }
  }
  return hits;
}

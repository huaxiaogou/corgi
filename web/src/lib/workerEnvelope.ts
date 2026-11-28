export interface ParsedWorkerEnvelope {
  isEnvelope: boolean;
  raw: string;
  constraint: string;
  eventCount: number;
  batched: boolean;
  triggerIds: string[];
  spilledIds: string[];
  body: string;
  preview: string;
  shouldFold: boolean;
  totalLines: number;
}

const FOLD_LINE_LIMIT = 4;
const EVENT_HEADER_RE = /^\[corgi\]\s+(\d+)\s+new\s+events?(?:\s+\(batched\))?\s*$/i;
const TRIGGER_LINE_RE = /^#(t\d+)\s*·/i;
const TASK_HEADER_RE = /^\[corgi task\b[^\]]*\]\s*$/i;
const CONSTRAINT_PREFIX = "【本会话红线】";
const SPILL_RE = /\[spilled;\s*run\s+`?corgi show (t\d+)`?\]/gi;

function pushUnique(list: string[], value: string) {
  if (value && !list.includes(value)) list.push(value);
}

function stripBoilerplateFooter(lines: string[]): string[] {
  const out: string[] = [];
  for (let i = 0; i < lines.length; i++) {
    const trimmed = lines[i].trim();
    if (/^How this works:\s*$/i.test(trimmed)) {
      break;
    }
    if (
      /^-\s*Report completion with\b/i.test(trimmed) ||
      /^-\s*If blocked on a decision\b/i.test(trimmed) ||
      /^-\s*Git Worktree Isolation:/i.test(trimmed)
    ) {
      continue;
    }
    out.push(lines[i]);
  }
  while (out.length > 0 && out[out.length - 1].trim() === "") out.pop();
  while (out.length > 0 && out[0].trim() === "") out.shift();
  return out;
}

export function parseWorkerUserEnvelope(raw: string): ParsedWorkerEnvelope {
  const normalized = raw.replace(/\r\n/g, "\n");
  const rawLines = normalized.split("\n");

  let cursor = 0;
  let constraint = "";
  while (cursor < rawLines.length && rawLines[cursor].trim() === "") cursor++;

  if (cursor < rawLines.length && rawLines[cursor].trim().startsWith(CONSTRAINT_PREFIX)) {
    constraint = rawLines[cursor].trim().slice(CONSTRAINT_PREFIX.length).trim();
    cursor++;
    while (cursor < rawLines.length && rawLines[cursor].trim() === "") cursor++;
  }

  let eventCount = 0;
  let batched = false;
  let matchedEnvelope = false;

  if (cursor < rawLines.length) {
    const firstTrimmed = rawLines[cursor].trim();
    const evMatch = firstTrimmed.match(EVENT_HEADER_RE);
    if (evMatch) {
      matchedEnvelope = true;
      eventCount = Number.parseInt(evMatch[1], 10) || 1;
      batched = /\(batched\)/i.test(firstTrimmed) || eventCount > 1;
      cursor++;
    } else if (TASK_HEADER_RE.test(firstTrimmed)) {
      matchedEnvelope = true;
      eventCount = 1;
      cursor++;
    } else if (constraint && rawLines.slice(cursor).some((l) => TASK_HEADER_RE.test(l.trim()) || EVENT_HEADER_RE.test(l.trim()))) {
      matchedEnvelope = true;
    }
  }

  if (!matchedEnvelope) {
    return {
      isEnvelope: false,
      raw,
      constraint: "",
      eventCount: 0,
      batched: false,
      triggerIds: [],
      spilledIds: [],
      body: raw,
      preview: raw,
      shouldFold: false,
      totalLines: rawLines.length,
    };
  }

  const triggerIds: string[] = [];
  const spilledIds: string[] = [];
  let match: RegExpExecArray | null;
  SPILL_RE.lastIndex = 0;
  while ((match = SPILL_RE.exec(normalized)) !== null) {
    pushUnique(spilledIds, match[1]);
    pushUnique(triggerIds, `#${match[1]}`);
  }

  const bodyLines: string[] = [];
  for (let i = cursor; i < rawLines.length; i++) {
    const line = rawLines[i];
    const trimmed = line.trim();
    if (EVENT_HEADER_RE.test(trimmed)) continue;
    const trigMatch = trimmed.match(TRIGGER_LINE_RE);
    if (trigMatch) {
      pushUnique(triggerIds, `#${trigMatch[1]}`);
      continue;
    }
    const deindented = line.startsWith("  ") ? line.slice(2) : line;
    if (TASK_HEADER_RE.test(deindented.trim())) continue;
    bodyLines.push(deindented);
  }

  const cleaned = stripBoilerplateFooter(bodyLines);
  const body = cleaned.join("\n").trim() || raw.trim();
  const displayLines = body.split("\n");
  const shouldFold = displayLines.length > FOLD_LINE_LIMIT;
  const preview = shouldFold ? displayLines.slice(0, FOLD_LINE_LIMIT).join("\n") : body;

  return {
    isEnvelope: true,
    raw,
    constraint,
    eventCount: Math.max(eventCount, triggerIds.length, 1),
    batched: batched || triggerIds.length > 1,
    triggerIds,
    spilledIds,
    body,
    preview,
    shouldFold,
    totalLines: displayLines.length,
  };
}

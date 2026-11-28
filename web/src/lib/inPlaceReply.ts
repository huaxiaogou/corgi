import type { Entry } from "../types/api";

export interface InPlacePartition {
  timelineEntries: Entry[];
  inPlaceByTarget: Map<string, Entry[]>;
}

/**
 * Separates valid in-place user replies under their loaded target entry while
 * falling back to the main timeline whenever the target entry is outside the
 * currently loaded window (never hiding or dropping messages).
 */
export function partitionInPlaceReplies(entries: readonly Entry[]): InPlacePartition {
  const targetIds = new Set<string>();
  for (const en of entries) {
    const isInPlaceCandidate = en.role === "user" && Boolean(en.in_place) && Boolean(en.re?.entry);
    if (!isInPlaceCandidate) {
      targetIds.add(en.id);
    }
  }

  const timelineEntries: Entry[] = [];
  const inPlaceByTarget = new Map<string, Entry[]>();

  for (const en of entries) {
    const targetId = en.re?.entry ?? "";
    if (en.role === "user" && en.in_place && targetId && targetId !== en.id && targetIds.has(targetId)) {
      const list = inPlaceByTarget.get(targetId);
      if (list) {
        list.push(en);
      } else {
        inPlaceByTarget.set(targetId, [en]);
      }
      continue;
    }
    timelineEntries.push(en);
  }

  return { timelineEntries, inPlaceByTarget };
}

/** D6 viewport split: >=768px renders inline reply box; <768px routes to bottom ComposerBar. */
export function shouldUseInlineReplyBox(viewportWidth: number): boolean {
  return viewportWidth >= 768;
}

export function setDraftEntry(
  drafts: Readonly<Record<string, string>>,
  key: string,
  value: string
): Record<string, string> {
  if (!key) return { ...drafts };
  if (value === "") {
    if (!(key in drafts)) return { ...drafts };
    const next = { ...drafts };
    delete next[key];
    return next;
  }
  if (drafts[key] === value) return { ...drafts };
  return { ...drafts, [key]: value };
}

export function hasDraftText(drafts: Readonly<Record<string, string>>, key: string): boolean {
  return Boolean(key && drafts[key] && drafts[key].trim().length > 0);
}

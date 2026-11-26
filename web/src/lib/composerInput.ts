export interface IMETracker {
  composing: boolean;
  lastCompositionEndAt: number;
}

export interface EnterKeyEventLike {
  key: string;
  shiftKey: boolean;
  ctrlKey?: boolean;
  metaKey?: boolean;
  altKey?: boolean;
  keyCode?: number;
  isComposing?: boolean;
  timeStamp?: number;
}

export type EnterKeyAction = "submit" | "ignore" | "swallow_safari_post_ime";

export interface TextareaLayout {
  height: number;
  overflowY: "hidden" | "auto";
  scrollTop: number | null;
}

// Safari on macOS fires compositionend synchronously before the confirming Enter keydown.
export const SAFARI_POST_COMPOSITION_GUARD_MS = 30;

export function createIMETracker(): IMETracker {
  return { composing: false, lastCompositionEndAt: 0 };
}

export function isActiveIMEComposition(ev: EnterKeyEventLike, tracker?: IMETracker): boolean {
  if (ev.isComposing) return true;
  if (ev.key === "Process" || ev.keyCode === 229) return true;
  return Boolean(tracker?.composing);
}

export function isPostCompositionSafariEnter(ev: EnterKeyEventLike, tracker?: IMETracker): boolean {
  if (!tracker || ev.key !== "Enter" || ev.shiftKey) return false;
  if (tracker.lastCompositionEndAt <= 0 || typeof ev.timeStamp !== "number") return false;
  const delta = ev.timeStamp - tracker.lastCompositionEndAt;
  return delta >= 0 && delta <= SAFARI_POST_COMPOSITION_GUARD_MS;
}

/**
 * Classifies an Enter keydown event in an IME-aware input:
 * - "ignore": active IME composition, Shift/modifier+Enter, or non-Enter (do NOT preventDefault or submit)
 * - "swallow_safari_post_ime": Safari's spurious Enter immediately after compositionend (preventDefault, do NOT submit)
 * - "submit": normal plain Enter outside IME composition
 */
export function classifyEnterKey(ev: EnterKeyEventLike, tracker?: IMETracker): EnterKeyAction {
  if (ev.key !== "Enter") return "ignore";
  if (ev.shiftKey || ev.ctrlKey || ev.metaKey || ev.altKey) return "ignore";
  if (isActiveIMEComposition(ev, tracker)) return "ignore";
  if (isPostCompositionSafariEnter(ev, tracker)) return "swallow_safari_post_ime";
  return "submit";
}

export function computeTextareaLayout(params: {
  scrollHeight: number;
  minHeight: number;
  maxHeight: number;
  prevScrollTop: number;
  atEnd: boolean;
  collapsedForMeasure: boolean;
}): TextareaLayout {
  const { scrollHeight, minHeight, maxHeight, prevScrollTop, atEnd, collapsedForMeasure } = params;
  const height = Math.min(maxHeight, Math.max(minHeight, Math.ceil(scrollHeight)));
  const overflows = scrollHeight > maxHeight;
  if (!overflows) {
    return { height, overflowY: "hidden", scrollTop: 0 };
  }
  if (atEnd) {
    return { height, overflowY: "auto", scrollTop: scrollHeight };
  }
  if (collapsedForMeasure) {
    return {
      height,
      overflowY: "auto",
      scrollTop: Math.max(0, Math.min(prevScrollTop, scrollHeight - height)),
    };
  }
  return { height, overflowY: "auto", scrollTop: null };
}

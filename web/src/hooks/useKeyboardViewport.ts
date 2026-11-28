import { useEffect, type RefObject } from "react";

export interface KeyboardViewportInput {
  innerHeight: number;
  viewportHeight: number;
  offsetTop: number;
  scale: number;
  editorFocused: boolean;
  prevInset?: number;
}

export interface KeyboardMetrics {
  kbInset: number;
  kbTop: number;
}

const KEYBOARD_MIN_INSET_PX = 80;
const PINCH_ZOOM_THRESHOLD = 1.02;
const JITTER_HYSTERESIS_PX = 2;

export function computeKeyboardMetrics(input: KeyboardViewportInput): KeyboardMetrics {
  const { innerHeight, viewportHeight, offsetTop, scale, editorFocused, prevInset = 0 } = input;
  if (!Number.isFinite(innerHeight) || !Number.isFinite(viewportHeight) || innerHeight <= 0 || viewportHeight <= 0) {
    return { kbInset: 0, kbTop: 0 };
  }
  // Pinch-zoom shrinks visualViewport height without opening a soft keyboard.
  if (Number.isFinite(scale) && scale > PINCH_ZOOM_THRESHOLD) {
    return { kbInset: 0, kbTop: 0 };
  }

  const safeTop = Number.isFinite(offsetTop) && offsetTop > 0 ? Math.round(offsetTop) : 0;
  const rawInset = Math.round(Math.max(0, innerHeight - viewportHeight - safeTop));

  if (!editorFocused && rawInset < KEYBOARD_MIN_INSET_PX) {
    return { kbInset: 0, kbTop: 0 };
  }
  if (rawInset < KEYBOARD_MIN_INSET_PX) {
    return { kbInset: 0, kbTop: 0 };
  }

  const stabilizedInset =
    prevInset > 0 && Math.abs(rawInset - prevInset) <= JITTER_HYSTERESIS_PX ? prevInset : rawInset;

  return {
    kbInset: stabilizedInset,
    kbTop: safeTop,
  };
}

function isEditableElement(el: Element | null): boolean {
  if (!el) return false;
  const tag = el.tagName;
  if (tag === "TEXTAREA") return true;
  if (tag === "INPUT") {
    const type = ((el as HTMLInputElement).type || "text").toLowerCase();
    return type !== "button" && type !== "submit" && type !== "checkbox" && type !== "radio" && type !== "file";
  }
  return Boolean((el as HTMLElement).isContentEditable);
}

/**
 * Isolates visualViewport keyboard inset writes to the targeted interactive container
 * (never mutating :root / documentElement) with rAF coalescing and integer-px dedup.
 */
export function useKeyboardViewport(targetRef: RefObject<HTMLElement | null>, enabled = true): void {
  useEffect(() => {
    if (!enabled || typeof window === "undefined") return;
    const vv = window.visualViewport;
    if (!vv) return;

    let rafId: number | null = null;
    let lastInset = -1;
    let lastTop = -1;

    const apply = () => {
      rafId = null;
      const el = targetRef.current;
      if (!el) return;
      const metrics = computeKeyboardMetrics({
        innerHeight: window.innerHeight,
        viewportHeight: vv.height,
        offsetTop: vv.offsetTop,
        scale: vv.scale,
        editorFocused: isEditableElement(document.activeElement),
        prevInset: lastInset > 0 ? lastInset : 0,
      });
      if (metrics.kbInset !== lastInset) {
        lastInset = metrics.kbInset;
        el.style.setProperty("--kb-inset", `${metrics.kbInset}px`);
      }
      if (metrics.kbTop !== lastTop) {
        lastTop = metrics.kbTop;
        el.style.setProperty("--kb-top", `${metrics.kbTop}px`);
      }
    };

    const schedule = () => {
      if (rafId !== null) return;
      rafId = window.requestAnimationFrame(apply);
    };

    schedule();
    vv.addEventListener("resize", schedule);
    vv.addEventListener("scroll", schedule);
    window.addEventListener("focusin", schedule);
    window.addEventListener("focusout", schedule);

    return () => {
      if (rafId !== null) window.cancelAnimationFrame(rafId);
      vv.removeEventListener("resize", schedule);
      vv.removeEventListener("scroll", schedule);
      window.removeEventListener("focusin", schedule);
      window.removeEventListener("focusout", schedule);
      const el = targetRef.current;
      if (el) {
        el.style.removeProperty("--kb-inset");
        el.style.removeProperty("--kb-top");
      }
    };
  }, [targetRef, enabled]);
}

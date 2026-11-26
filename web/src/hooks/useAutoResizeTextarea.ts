import { useCallback, useEffect, useLayoutEffect, type RefObject } from "react";
import { computeTextareaLayout } from "../lib/composerInput";

interface Options {
  minHeight?: number;
  maxHeight?: number;
}

/**
 * Auto-resizes a textarea smoothly between minHeight and maxHeight while preserving
 * internal scrollability and keeping the caret visible when typing/pasting at the end.
 */
export function useAutoResizeTextarea(
  ref: RefObject<HTMLTextAreaElement | null>,
  value: string,
  { minHeight = 24, maxHeight = 180 }: Options = {}
) {
  const sync = useCallback(() => {
    const el = ref.current;
    if (!el) return;

    const prevScrollTop = el.scrollTop;
    const atEnd =
      el.selectionStart === el.value.length &&
      el.selectionEnd === el.value.length &&
      el.value.length > 0;

    // Fast path: already clamped at maxHeight and content still overflows.
    // Avoid collapsing height to 0px so user scrolling and middle-edit caret position never reset.
    if (el.offsetHeight >= maxHeight && el.scrollHeight > maxHeight) {
      const layout = computeTextareaLayout({
        scrollHeight: el.scrollHeight,
        minHeight,
        maxHeight,
        prevScrollTop,
        atEnd,
        collapsedForMeasure: false,
      });
      el.style.height = `${layout.height}px`;
      el.style.overflowY = layout.overflowY;
      if (layout.scrollTop !== null) {
        el.scrollTop = layout.scrollTop;
      }
      return;
    }

    el.style.height = "0px";
    const layout = computeTextareaLayout({
      scrollHeight: el.scrollHeight,
      minHeight,
      maxHeight,
      prevScrollTop,
      atEnd,
      collapsedForMeasure: true,
    });
    el.style.height = `${layout.height}px`;
    el.style.overflowY = layout.overflowY;
    if (layout.scrollTop !== null) {
      el.scrollTop = layout.scrollTop;
    }
  }, [ref, minHeight, maxHeight]);

  useLayoutEffect(() => {
    sync();
  }, [value, sync]);

  useEffect(() => {
    const el = ref.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    let lastWidth = el.clientWidth;
    const ro = new ResizeObserver(() => {
      if (el.clientWidth !== lastWidth) {
        lastWidth = el.clientWidth;
        sync();
      }
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [ref, sync]);
}

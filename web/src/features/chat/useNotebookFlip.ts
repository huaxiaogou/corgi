import { useLayoutEffect, useRef, type RefObject } from "react";
import type { NotebookMode } from "../../lib/workItems";

export function prefersReducedMotion(): boolean {
  return typeof window !== "undefined" && window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
}

export function useNotebookFlip({
  mode,
  isDesktop,
  miniListRef,
  activeListRef,
  miniOrderSig,
  activeOrderSig,
  resetKey,
}: {
  mode: NotebookMode;
  isDesktop: boolean;
  miniListRef: RefObject<HTMLDivElement | null>;
  activeListRef: RefObject<HTMLDivElement | null>;
  miniOrderSig: string;
  activeOrderSig: string;
  resetKey: string;
}) {
  const prevMiniTopsRef = useRef<Map<string, number>>(new Map());
  const prevActiveTopsRef = useRef<Map<string, number>>(new Map());
  const prevModeRef = useRef<NotebookMode>(mode);
  const prevResetRef = useRef(resetKey);

  useLayoutEffect(() => {
    if (prevResetRef.current !== resetKey) {
      prevResetRef.current = resetKey;
      prevMiniTopsRef.current = new Map();
      prevActiveTopsRef.current = new Map();
    }
    const modeChanged = prevModeRef.current !== mode;
    prevModeRef.current = mode;

    const runFlip = (container: HTMLDivElement | null, prevTopsRef: { current: Map<string, number> }) => {
      if (!container) {
        prevTopsRef.current = new Map();
        return;
      }
      const nodes = Array.from(container.querySelectorAll<HTMLElement>("[data-flip-id]"));
      const nextTops = new Map<string, number>();
      for (const el of nodes) {
        nextTops.set(el.dataset.flipId!, el.offsetTop);
      }

      const prevTops = prevTopsRef.current;
      prevTopsRef.current = nextTops;

      if (modeChanged || prevTops.size === 0 || prefersReducedMotion()) return;

      const toAnimate: { el: HTMLElement; kind: "move" | "enter" }[] = [];
      for (const el of nodes) {
        const id = el.dataset.flipId!;
        const oldTop = prevTops.get(id);
        const newTop = nextTops.get(id)!;
        if (oldTop !== undefined) {
          const dy = oldTop - newTop;
          if (Math.abs(dy) > 0.5) {
            el.style.transition = "none";
            el.style.transform = `translateY(${dy}px)`;
            toAnimate.push({ el, kind: "move" });
          }
        } else {
          el.style.transition = "none";
          el.style.opacity = "0";
          el.style.transform = "translateY(-4px)";
          toAnimate.push({ el, kind: "enter" });
        }
      }
      if (toAnimate.length === 0) return;

      container.getBoundingClientRect();
      requestAnimationFrame(() => {
        for (const { el, kind } of toAnimate) {
          if (kind === "move") {
            el.style.transition = "transform 300ms var(--ease-quiet)";
            el.style.transform = "translateY(0)";
          } else {
            el.style.transition = "opacity 240ms var(--ease-quiet), transform 240ms var(--ease-quiet)";
            el.style.opacity = "";
            el.style.transform = "translateY(0)";
          }
          const cleanup = () => {
            el.style.transition = "";
            el.style.transform = "";
          };
          el.addEventListener("transitionend", cleanup, { once: true });
        }
      });
    };

    if (mode === "mini" && isDesktop) {
      runFlip(miniListRef.current, prevMiniTopsRef);
      prevActiveTopsRef.current = new Map();
    } else if (mode === "expanded") {
      runFlip(activeListRef.current, prevActiveTopsRef);
      prevMiniTopsRef.current = new Map();
    } else {
      prevMiniTopsRef.current = new Map();
      prevActiveTopsRef.current = new Map();
    }
  }, [mode, isDesktop, miniOrderSig, activeOrderSig, resetKey, miniListRef, activeListRef]);
}

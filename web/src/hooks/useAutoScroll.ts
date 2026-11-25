import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

interface UseAutoScrollOptions {
  /** Changes when switching/re-selecting conversations, view modes, or receiving SSE reset. */
  resetKey: string;
  /** Changes when bottom-facing content grows or updates (new messages, streaming tail, approval, log). */
  contentKey: string;
  /** Changes when older history is prepended at the top so the current viewport stays anchored. */
  prependKey: number;
  /** True once real content for the current resetKey is rendered and ready to scroll. */
  hasContent: boolean;
  /** Set false when another renderer (e.g. xterm.js) owns scrolling. */
  enabled?: boolean;
}

const STICK_THRESHOLD_PX = 80;
const JUMP_BUTTON_THRESHOLD_PX = 180;
const INITIAL_SETTLE_MS = 650;
const USER_GESTURE_WINDOW_MS = 800;

export function useAutoScroll({
  resetKey,
  contentKey,
  prependKey,
  hasContent,
  enabled = true,
}: UseAutoScrollOptions) {
  const containerRef = useRef<HTMLDivElement>(null);
  const contentRef = useRef<HTMLDivElement>(null);
  const pendingInitialRef = useRef(true);
  const stickToBottomRef = useRef(true);
  const userScrolledUpRef = useRef(false);
  const userGestureUntilRef = useRef(0);
  const initialSettleUntilRef = useRef(0);
  const lastScrollTopRef = useRef(0);
  const jumpingRef = useRef(false);
  const jumpTimerRef = useRef<number | null>(null);
  const raf1Ref = useRef<number | null>(null);
  const raf2Ref = useRef<number | null>(null);
  const prependSnapRef = useRef<{ scrollHeight: number; scrollTop: number } | null>(null);
  const [showJumpBottom, setShowJumpBottom] = useState(false);

  const cancelRafs = useCallback(() => {
    if (raf1Ref.current !== null) {
      window.cancelAnimationFrame(raf1Ref.current);
      raf1Ref.current = null;
    }
    if (raf2Ref.current !== null) {
      window.cancelAnimationFrame(raf2Ref.current);
      raf2Ref.current = null;
    }
  }, []);

  const clearJumpGuard = useCallback(() => {
    jumpingRef.current = false;
    if (jumpTimerRef.current !== null) {
      window.clearTimeout(jumpTimerRef.current);
      jumpTimerRef.current = null;
    }
  }, []);

  const snapNow = useCallback((el: HTMLDivElement) => {
    el.scrollTop = el.scrollHeight;
    lastScrollTopRef.current = el.scrollTop;
  }, []);

  const scheduleSnapConfirm = useCallback(() => {
    cancelRafs();
    raf1Ref.current = window.requestAnimationFrame(() => {
      raf1Ref.current = null;
      const el = containerRef.current;
      if (!el || !stickToBottomRef.current || userScrolledUpRef.current || prependSnapRef.current) return;
      snapNow(el);
      raf2Ref.current = window.requestAnimationFrame(() => {
        raf2Ref.current = null;
        const el2 = containerRef.current;
        if (!el2 || !stickToBottomRef.current || userScrolledUpRef.current || prependSnapRef.current) return;
        snapNow(el2);
      });
    });
  }, [cancelRafs, snapNow]);

  const updateScrollState = useCallback((el: HTMLDivElement, allowAutoResnap: boolean) => {
    const gap = Math.max(0, el.scrollHeight - el.scrollTop - el.clientHeight);
    const movedUp = el.scrollTop < lastScrollTopRef.current - 2;
    lastScrollTopRef.current = el.scrollTop;

    if (jumpingRef.current) {
      if (gap <= STICK_THRESHOLD_PX) {
        clearJumpGuard();
        stickToBottomRef.current = true;
        userScrolledUpRef.current = false;
        setShowJumpBottom(false);
      }
      return;
    }

    if (gap <= STICK_THRESHOLD_PX) {
      stickToBottomRef.current = true;
      userScrolledUpRef.current = false;
      setShowJumpBottom(false);
      return;
    }

    const userActive = movedUp || performance.now() < userGestureUntilRef.current;
    if (allowAutoResnap && stickToBottomRef.current && !userScrolledUpRef.current && !userActive) {
      snapNow(el);
      setShowJumpBottom(false);
      return;
    }

    stickToBottomRef.current = false;
    userScrolledUpRef.current = true;
    setShowJumpBottom(gap > JUMP_BUTTON_THRESHOLD_PX);
  }, [clearJumpGuard, snapNow]);

  const onScroll = useCallback(() => {
    const el = containerRef.current;
    if (!enabled || !el) return;
    if (pendingInitialRef.current || prependSnapRef.current) return;
    updateScrollState(el, true);
  }, [enabled, updateScrollState]);

  const capturePrependAnchor = useCallback(() => {
    const el = containerRef.current;
    if (!el) return;
    cancelRafs();
    clearJumpGuard();
    pendingInitialRef.current = false;
    stickToBottomRef.current = false;
    userScrolledUpRef.current = true;
    initialSettleUntilRef.current = 0;
    prependSnapRef.current = {
      scrollHeight: el.scrollHeight,
      scrollTop: el.scrollTop,
    };
  }, [cancelRafs, clearJumpGuard]);

  const scrollToBottom = useCallback((smooth = false) => {
    const el = containerRef.current;
    stickToBottomRef.current = true;
    userScrolledUpRef.current = false;
    prependSnapRef.current = null;
    setShowJumpBottom(false);
    if (!el) return;
    if (smooth) {
      cancelRafs();
      clearJumpGuard();
      jumpingRef.current = true;
      jumpTimerRef.current = window.setTimeout(() => {
        jumpingRef.current = false;
        jumpTimerRef.current = null;
      }, 650);
      el.scrollTo({ top: el.scrollHeight, behavior: "smooth" });
    } else {
      clearJumpGuard();
      snapNow(el);
      scheduleSnapConfirm();
    }
  }, [cancelRafs, clearJumpGuard, snapNow, scheduleSnapConfirm]);

  // Re-arm initial jump to bottom whenever the active conversation, mode, or reset sequence changes.
  useLayoutEffect(() => {
    cancelRafs();
    clearJumpGuard();
    pendingInitialRef.current = true;
    stickToBottomRef.current = true;
    userScrolledUpRef.current = false;
    userGestureUntilRef.current = 0;
    initialSettleUntilRef.current = 0;
    prependSnapRef.current = null;
    setShowJumpBottom(false);
  }, [resetKey, enabled, cancelRafs, clearJumpGuard]);

  // Preserve reading position after prepending older messages at the top.
  useLayoutEffect(() => {
    const el = containerRef.current;
    const snap = prependSnapRef.current;
    if (!enabled || !el || !snap) return;
    prependSnapRef.current = null;
    const delta = el.scrollHeight - snap.scrollHeight;
    el.scrollTop = snap.scrollTop + Math.max(0, delta);
    lastScrollTopRef.current = el.scrollTop;
    updateScrollState(el, false);
  }, [prependKey, enabled, updateScrollState]);

  // Synchronously snap to bottom before paint on initial load and while stuck to bottom.
  useLayoutEffect(() => {
    const el = containerRef.current;
    if (!enabled || !el || !hasContent) return;
    if (prependSnapRef.current) return;
    if (pendingInitialRef.current) {
      pendingInitialRef.current = false;
      stickToBottomRef.current = true;
      userScrolledUpRef.current = false;
      initialSettleUntilRef.current = performance.now() + INITIAL_SETTLE_MS;
      setShowJumpBottom(false);
      snapNow(el);
      scheduleSnapConfirm();
      return;
    }
    if (stickToBottomRef.current && !userScrolledUpRef.current) {
      setShowJumpBottom(false);
      snapNow(el);
      scheduleSnapConfirm();
    }
  }, [resetKey, contentKey, hasContent, enabled, snapNow, scheduleSnapConfirm]);

  // Keep pinned to bottom across container resize, initial layout settle, and inline image loads.
  useEffect(() => {
    const el = containerRef.current;
    if (!enabled || !el) return;

    const markUserGesture = () => {
      userGestureUntilRef.current = performance.now() + USER_GESTURE_WINDOW_MS;
      if (jumpingRef.current) {
        clearJumpGuard();
      }
    };
    const onWheel = (ev: WheelEvent) => {
      if (ev.deltaY < 0) {
        markUserGesture();
      } else if (jumpingRef.current) {
        clearJumpGuard();
      }
    };
    const onKeyDown = (ev: KeyboardEvent) => {
      if (ev.key === "ArrowUp" || ev.key === "PageUp" || ev.key === "Home") {
        markUserGesture();
      }
    };
    const onMediaLoad = () => {
      if (stickToBottomRef.current && !userScrolledUpRef.current && !prependSnapRef.current) {
        snapNow(el);
      }
    };

    const viewportRO = new ResizeObserver(() => {
      if (stickToBottomRef.current && !userScrolledUpRef.current && !prependSnapRef.current && !pendingInitialRef.current) {
        snapNow(el);
      }
    });
    viewportRO.observe(el);

    const contentEl = contentRef.current;
    const contentRO = contentEl
      ? new ResizeObserver(() => {
          if (
            performance.now() < initialSettleUntilRef.current &&
            stickToBottomRef.current &&
            !userScrolledUpRef.current &&
            !prependSnapRef.current
          ) {
            snapNow(el);
          }
        })
      : null;
    if (contentEl && contentRO) {
      contentRO.observe(contentEl);
    }

    el.addEventListener("load", onMediaLoad, true);
    el.addEventListener("wheel", onWheel, { passive: true });
    el.addEventListener("touchstart", markUserGesture, { passive: true });
    el.addEventListener("touchmove", markUserGesture, { passive: true });
    el.addEventListener("pointerdown", markUserGesture, { passive: true });
    el.addEventListener("keydown", onKeyDown);

    return () => {
      viewportRO.disconnect();
      contentRO?.disconnect();
      el.removeEventListener("load", onMediaLoad, true);
      el.removeEventListener("wheel", onWheel);
      el.removeEventListener("touchstart", markUserGesture);
      el.removeEventListener("touchmove", markUserGesture);
      el.removeEventListener("pointerdown", markUserGesture);
      el.removeEventListener("keydown", onKeyDown);
      cancelRafs();
      clearJumpGuard();
    };
  }, [enabled, resetKey, cancelRafs, clearJumpGuard, snapNow]);

  return {
    containerRef,
    contentRef,
    onScroll,
    showJumpBottom,
    scrollToBottom,
    capturePrependAnchor,
  };
}

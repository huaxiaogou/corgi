import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

interface UseAutoScrollOptions {
  /** Changes when switching conversations or view modes; re-arms initial jump to bottom. */
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

export function useAutoScroll({
  resetKey,
  contentKey,
  prependKey,
  hasContent,
  enabled = true,
}: UseAutoScrollOptions) {
  const containerRef = useRef<HTMLDivElement>(null);
  const pendingInitialRef = useRef(true);
  const stickToBottomRef = useRef(true);
  const jumpingRef = useRef(false);
  const jumpTimerRef = useRef<number | null>(null);
  const prependSnapRef = useRef<{ scrollHeight: number; scrollTop: number } | null>(null);
  const [showJumpBottom, setShowJumpBottom] = useState(false);

  const clearJumpGuard = useCallback(() => {
    jumpingRef.current = false;
    if (jumpTimerRef.current !== null) {
      window.clearTimeout(jumpTimerRef.current);
      jumpTimerRef.current = null;
    }
  }, []);

  const updateScrollState = useCallback((el: HTMLDivElement) => {
    const gap = Math.max(0, el.scrollHeight - el.scrollTop - el.clientHeight);
    if (jumpingRef.current) {
      if (gap <= STICK_THRESHOLD_PX) {
        clearJumpGuard();
        stickToBottomRef.current = true;
        setShowJumpBottom(false);
      }
      return;
    }
    stickToBottomRef.current = gap <= STICK_THRESHOLD_PX;
    setShowJumpBottom(gap > JUMP_BUTTON_THRESHOLD_PX);
  }, [clearJumpGuard]);

  const onScroll = useCallback(() => {
    const el = containerRef.current;
    if (!enabled || !el) return;
    if (pendingInitialRef.current || prependSnapRef.current) return;
    updateScrollState(el);
  }, [enabled, updateScrollState]);

  const capturePrependAnchor = useCallback(() => {
    const el = containerRef.current;
    if (!el) return;
    clearJumpGuard();
    pendingInitialRef.current = false;
    stickToBottomRef.current = false;
    prependSnapRef.current = {
      scrollHeight: el.scrollHeight,
      scrollTop: el.scrollTop,
    };
  }, [clearJumpGuard]);

  const scrollToBottom = useCallback((smooth = false) => {
    const el = containerRef.current;
    stickToBottomRef.current = true;
    prependSnapRef.current = null;
    setShowJumpBottom(false);
    if (!el) return;
    if (smooth) {
      clearJumpGuard();
      jumpingRef.current = true;
      jumpTimerRef.current = window.setTimeout(() => {
        jumpingRef.current = false;
        jumpTimerRef.current = null;
      }, 650);
      el.scrollTo({ top: el.scrollHeight, behavior: "smooth" });
    } else {
      clearJumpGuard();
      el.scrollTop = el.scrollHeight;
    }
  }, [clearJumpGuard]);

  // Re-arm initial jump to bottom whenever the active conversation or view mode changes.
  useLayoutEffect(() => {
    clearJumpGuard();
    pendingInitialRef.current = true;
    stickToBottomRef.current = true;
    prependSnapRef.current = null;
    setShowJumpBottom(false);
  }, [resetKey, enabled, clearJumpGuard]);

  // Preserve reading position after prepending older messages at the top.
  useLayoutEffect(() => {
    const el = containerRef.current;
    const snap = prependSnapRef.current;
    if (!enabled || !el || !snap) return;
    prependSnapRef.current = null;
    const delta = el.scrollHeight - snap.scrollHeight;
    el.scrollTop = snap.scrollTop + Math.max(0, delta);
    updateScrollState(el);
  }, [prependKey, enabled, updateScrollState]);

  // Synchronously snap to bottom before paint on initial load and while stuck to bottom.
  useLayoutEffect(() => {
    const el = containerRef.current;
    if (!enabled || !el || !hasContent) return;
    if (prependSnapRef.current) return;
    if (pendingInitialRef.current) {
      el.scrollTop = el.scrollHeight;
      pendingInitialRef.current = false;
      stickToBottomRef.current = true;
      setShowJumpBottom(false);
      return;
    }
    if (stickToBottomRef.current) {
      el.scrollTop = el.scrollHeight;
      setShowJumpBottom(false);
    }
  }, [resetKey, contentKey, hasContent, enabled]);

  // Keep pinned to bottom when inline images finish loading, and release smooth-jump guard on user input.
  useEffect(() => {
    const el = containerRef.current;
    if (!enabled || !el) return;
    const onMediaLoad = () => {
      if (stickToBottomRef.current && !prependSnapRef.current) {
        el.scrollTop = el.scrollHeight;
      }
    };
    const onUserInterrupt = () => {
      if (jumpingRef.current) {
        clearJumpGuard();
      }
    };
    el.addEventListener("load", onMediaLoad, true);
    el.addEventListener("wheel", onUserInterrupt, { passive: true });
    el.addEventListener("touchstart", onUserInterrupt, { passive: true });
    return () => {
      el.removeEventListener("load", onMediaLoad, true);
      el.removeEventListener("wheel", onUserInterrupt);
      el.removeEventListener("touchstart", onUserInterrupt);
      clearJumpGuard();
    };
  }, [enabled, resetKey, clearJumpGuard]);

  return {
    containerRef,
    onScroll,
    showJumpBottom,
    scrollToBottom,
    capturePrependAnchor,
  };
}

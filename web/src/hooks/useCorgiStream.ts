import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError, onUnauthorized } from "../lib/api";
import {
  clearAllSnapshotCaches,
  readSnapshotCache,
  writeSnapshotCache,
} from "../lib/snapshotCache";
import type { PaneState, SuperView } from "../types/api";

const SNAPSHOT_DEBOUNCE_MS = 350;
const SESSION_RECHECK_COOLDOWN_MS = 3000;

export interface StreamData {
  authed: boolean | null;
  connected: boolean;
  superView: SuperView | null;
  paneState: PaneState | null;
  superReady: boolean;
  stateReady: boolean;
  refreshAuth: () => void;
}

interface SessionResponse {
  ok: boolean;
  version?: string;
  scope?: string;
}

export function useCorgiStream(thread: string): StreamData {
  const [authed, setAuthed] = useState<boolean | null>(null);
  const [sessionScope, setSessionScope] = useState("");
  const [connected, setConnected] = useState(false);
  const [superView, setSuperView] = useState<SuperView | null>(null);
  const [paneState, setPaneState] = useState<PaneState | null>(null);
  const [superReady, setSuperReady] = useState(false);
  const [stateReady, setStateReady] = useState(false);
  const [authSeq, setAuthSeq] = useState(0);

  const threadRef = useRef(thread);
  threadRef.current = thread;
  const loadedThreadRef = useRef("");
  const scopeRef = useRef(sessionScope);
  scopeRef.current = sessionScope;
  const latestSuperRef = useRef<SuperView | null>(null);
  const latestStateRef = useRef<PaneState | null>(null);
  const liveDirtyRef = useRef(false);
  const writeTimerRef = useRef<number | null>(null);
  const lastRecheckRef = useRef(0);

  const cancelPendingWrite = useCallback(() => {
    if (writeTimerRef.current !== null) {
      window.clearTimeout(writeTimerRef.current);
      writeTimerRef.current = null;
    }
  }, []);

  const flushSnapshotNow = useCallback(() => {
    cancelPendingWrite();
    if (!liveDirtyRef.current) return;
    const scope = scopeRef.current;
    if (!scope) return;
    liveDirtyRef.current = false;
    writeSnapshotCache(window.location.origin, scope, threadRef.current, {
      superView: latestSuperRef.current,
      paneState: latestStateRef.current,
    });
  }, [cancelPendingWrite]);

  const scheduleSnapshotWrite = useCallback(() => {
    if (!scopeRef.current) return;
    liveDirtyRef.current = true;
    cancelPendingWrite();
    writeTimerRef.current = window.setTimeout(() => {
      writeTimerRef.current = null;
      flushSnapshotNow();
    }, SNAPSHOT_DEBOUNCE_MS);
  }, [cancelPendingWrite, flushSnapshotNow]);

  const resetSessionState = useCallback(() => {
    cancelPendingWrite();
    liveDirtyRef.current = false;
    latestSuperRef.current = null;
    latestStateRef.current = null;
    scopeRef.current = "";
    clearAllSnapshotCaches();
    setSessionScope("");
    setSuperView(null);
    setPaneState(null);
    setSuperReady(false);
    setStateReady(false);
    setConnected(false);
    setAuthed(false);
  }, [cancelPendingWrite]);

  // React immediately to any 401 or logout triggered anywhere in the app.
  useEffect(() => onUnauthorized(resetSessionState), [resetSessionState]);

  // Stage 1 -> Stage 2: authenticate first; ONLY read local snapshot after /api/session passes.
  useEffect(() => {
    let active = true;
    const params = new URLSearchParams(window.location.search);
    const pairCode = params.get("pair");
    const hadPairParam = Boolean(pairCode);
    const init = async () => {
      try {
        if (pairCode) {
          await api("/api/pair", { code: pairCode, kind: "browser", name: "Browser" });
          window.history.replaceState({}, "", window.location.pathname + window.location.hash);
        }
        const res = await api<SessionResponse>("/api/session");
        if (!active) return;
        const nextScope = typeof res.scope === "string" ? res.scope.trim() : "";
        scopeRef.current = nextScope;
        loadedThreadRef.current = threadRef.current;
        setSessionScope(nextScope);
        setSuperReady(false);
        setStateReady(false);
        if (nextScope && !hadPairParam) {
          const cached = readSnapshotCache(window.location.origin, nextScope, threadRef.current);
          if (cached) {
            latestSuperRef.current = cached.superView;
            latestStateRef.current = cached.paneState;
            setSuperView(cached.superView);
            setPaneState(cached.paneState);
          }
        } else {
          latestSuperRef.current = null;
          latestStateRef.current = null;
          setSuperView(null);
          setPaneState(null);
        }
        setAuthed(true);
      } catch (err) {
        if (!active) return;
        if (err instanceof ApiError && err.status === 401) {
          resetSessionState();
        } else {
          resetSessionState();
        }
      }
    };
    void init();
    return () => {
      active = false;
    };
  }, [authSeq, resetSessionState]);

  // Stage 3: live SSE stream, per-thread cache swap, and error-triggered session re-check.
  useEffect(() => {
    if (!authed) return;
    setSuperReady(false);
    if (loadedThreadRef.current !== thread) {
      loadedThreadRef.current = thread;
      if (scopeRef.current) {
        const cached = readSnapshotCache(window.location.origin, scopeRef.current, thread);
        latestSuperRef.current = cached?.superView ?? null;
        setSuperView(cached?.superView ?? null);
      } else {
        latestSuperRef.current = null;
        setSuperView(null);
      }
    }

    let closed = false;
    const qs = thread ? `?thread=${encodeURIComponent(thread)}` : "";
    const es = new EventSource(`/api/events${qs}`);
    es.onopen = () => setConnected(true);
    es.onerror = () => {
      setConnected(false);
      const now = Date.now();
      if (closed || now - lastRecheckRef.current < SESSION_RECHECK_COOLDOWN_MS) return;
      lastRecheckRef.current = now;
      void api<SessionResponse>("/api/session").catch((err) => {
        if (!closed && err instanceof ApiError && err.status === 401) {
          resetSessionState();
        }
      });
    };
    es.addEventListener("super", (ev) => {
      try {
        const next = JSON.parse((ev as MessageEvent).data) as SuperView;
        latestSuperRef.current = next;
        setSuperView(next);
        setSuperReady(true);
        setConnected(true);
        scheduleSnapshotWrite();
      } catch {
        // ignore malformed frame
      }
    });
    es.addEventListener("state", (ev) => {
      try {
        const next = JSON.parse((ev as MessageEvent).data) as PaneState;
        latestStateRef.current = next;
        setPaneState(next);
        setStateReady(true);
        setConnected(true);
        scheduleSnapshotWrite();
      } catch {
        // ignore malformed frame
      }
    });
    return () => {
      closed = true;
      flushSnapshotNow();
      es.close();
      setConnected(false);
    };
  }, [authed, thread, scheduleSnapshotWrite, flushSnapshotNow, resetSessionState]);

  // Flush debounced cache write when tab hides or page unloads.
  useEffect(() => {
    if (!authed) return;
    const onVis = () => {
      if (document.visibilityState === "hidden") flushSnapshotNow();
    };
    const onPageHide = () => flushSnapshotNow();
    document.addEventListener("visibilitychange", onVis);
    window.addEventListener("pagehide", onPageHide);
    return () => {
      document.removeEventListener("visibilitychange", onVis);
      window.removeEventListener("pagehide", onPageHide);
    };
  }, [authed, flushSnapshotNow]);

  useEffect(() => {
    if (!authed) return;
    const ping = () => {
      if (document.visibilityState === "visible") {
        void api("/api/super/seen", {}).catch(() => {});
      }
    };
    ping();
    const timer = window.setInterval(ping, 30000);
    document.addEventListener("visibilitychange", ping);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", ping);
    };
  }, [authed]);

  return {
    authed,
    connected,
    superView,
    paneState,
    superReady,
    stateReady,
    refreshAuth: () => setAuthSeq((n) => n + 1),
  };
}

import { useEffect, useState } from "react";
import { api, ApiError } from "../lib/api";
import type { PaneState, SuperView } from "../types/api";

export interface StreamData {
  authed: boolean | null;
  connected: boolean;
  superView: SuperView | null;
  paneState: PaneState | null;
  refreshAuth: () => void;
}

export function useCorgiStream(thread: string): StreamData {
  const [authed, setAuthed] = useState<boolean | null>(null);
  const [connected, setConnected] = useState(false);
  const [superView, setSuperView] = useState<SuperView | null>(null);
  const [paneState, setPaneState] = useState<PaneState | null>(null);
  const [authSeq, setAuthSeq] = useState(0);

  useEffect(() => {
    let active = true;
    // Handle ?pair=<code> URL parameter automatically on scan.
    const params = new URLSearchParams(window.location.search);
    const pairCode = params.get("pair");
    const init = async () => {
      try {
        if (pairCode) {
          await api("/api/pair", { code: pairCode, kind: "browser", name: "Browser" });
          window.history.replaceState({}, "", window.location.pathname + window.location.hash);
        }
        await api("/api/session");
        if (active) setAuthed(true);
      } catch (err) {
        if (active && err instanceof ApiError && err.status === 401) {
          setAuthed(false);
        } else if (active) {
          setAuthed(false);
        }
      }
    };
    void init();
    return () => {
      active = false;
    };
  }, [authSeq]);

  useEffect(() => {
    if (!authed) return;
    const qs = thread ? `?thread=${encodeURIComponent(thread)}` : "";
    const es = new EventSource(`/api/events${qs}`);
    es.onopen = () => setConnected(true);
    es.onerror = () => setConnected(false);
    es.addEventListener("super", (ev) => {
      try {
        setSuperView(JSON.parse((ev as MessageEvent).data) as SuperView);
        setConnected(true);
      } catch {
        // ignore malformed frame
      }
    });
    es.addEventListener("state", (ev) => {
      try {
        setPaneState(JSON.parse((ev as MessageEvent).data) as PaneState);
        setConnected(true);
      } catch {
        // ignore malformed frame
      }
    });
    return () => {
      es.close();
      setConnected(false);
    };
  }, [authed, thread]);

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
    refreshAuth: () => setAuthSeq((n) => n + 1),
  };
}

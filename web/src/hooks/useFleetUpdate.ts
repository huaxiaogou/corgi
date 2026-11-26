import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api } from "../lib/api";
import { isOldVersion, selectOutdatedOnline, type OutdatedTarget } from "../lib/fleetUpdate";
import type { FleetUpdateView, Machine, VersionStatus } from "../types/api";

export interface FleetUpdateController {
  verInfo: VersionStatus | null;
  outdatedOnline: OutdatedTarget[];
  latest: string;
  busy: boolean;
  phase: "idle" | "updating" | "reconnecting" | "restarting" | "done" | "failed";
  index: number;
  total: number;
  currentId: string | null;
  currentName: string;
  failedName: string;
  error: string;
  errorFor: (id: string) => string;
  checkVersions: (soon?: boolean) => Promise<void>;
  startFleetUpdate: () => Promise<void>;
  startSingleUpdate: (machineId: string, name: string) => Promise<void>;
}

const POLL_INTERVAL_MS = 30 * 60 * 1000;
const BUSY_POLL_MS = 1200;

export function useFleetUpdate(
  authed: boolean,
  machines: Machine[],
  onToast: (msg: string) => void
): FleetUpdateController {
  const [verInfo, setVerInfo] = useState<VersionStatus | null>(null);
  const [singleBusyId, setSingleBusyId] = useState<string | null>(null);
  const [singleBusyName, setSingleBusyName] = useState("");
  const [singlePhase, setSinglePhase] = useState<"idle" | "updating" | "reconnecting" | "restarting" | "failed">("idle");
  const [localErrors, setLocalErrors] = useState<Record<string, string>>({});
  const [restartingTarget, setRestartingTarget] = useState("");

  const retriesRef = useRef(0);
  const retryTimerRef = useRef<number | null>(null);
  const waitingRestartRef = useRef(false);
  const toastRef = useRef(onToast);
  toastRef.current = onToast;

  const checkVersions = useCallback(async (soon = false) => {
    if (!authed) return;
    try {
      const data = await api<VersionStatus>(`/api/version${soon ? "?soon=1" : ""}`);
      setVerInfo(data);
      if (data.latest) {
        retriesRef.current = 0;
        if (retryTimerRef.current !== null) {
          window.clearTimeout(retryTimerRef.current);
          retryTimerRef.current = null;
        }
      } else if (retriesRef.current < 6) {
        retriesRef.current += 1;
        if (retryTimerRef.current !== null) window.clearTimeout(retryTimerRef.current);
        retryTimerRef.current = window.setTimeout(() => {
          void checkVersions(true);
        }, 5000);
      }
    } catch {
      // Transient error while restarting or offline; ignore silently.
    }
  }, [authed]);

  const waitForHubRestartAndReload = useCallback(async (targetVersion: string) => {
    if (waitingRestartRef.current) return;
    waitingRestartRef.current = true;
    setRestartingTarget(targetVersion);
    setSinglePhase("restarting");

    await new Promise((r) => window.setTimeout(r, 1200));
    let sawDown = false;
    for (let i = 0; i < 60; i++) {
      try {
        const res = await fetch("/api/version", { cache: "no-store", credentials: "same-origin" });
        if (res.ok) {
          const data = (await res.json().catch(() => ({}))) as Partial<VersionStatus>;
          const curVer = data.version ?? "";
          if (curVer && targetVersion && !isOldVersion(curVer, targetVersion)) {
            window.location.reload();
            return;
          }
          if (sawDown && curVer && (!targetVersion || !isOldVersion(curVer, targetVersion))) {
            window.location.reload();
            return;
          }
        } else if (res.status >= 500) {
          sawDown = true;
        }
      } catch {
        sawDown = true;
      }
      await new Promise((r) => window.setTimeout(r, 1500));
    }

    waitingRestartRef.current = false;
    setRestartingTarget("");
    setSingleBusyId(null);
    setSinglePhase("failed");
    const msg = "等待 Hub 重启恢复超时，请手动刷新页面查看版本";
    setLocalErrors((prev) => ({ ...prev, "": msg }));
    toastRef.current(msg);
  }, []);

  // Initial check + 30-minute periodic poll after login.
  useEffect(() => {
    if (!authed) return;
    retriesRef.current = 0;
    void checkVersions(true);
    const id = window.setInterval(() => {
      void checkVersions(false);
    }, POLL_INTERVAL_MS);
    return () => {
      window.clearInterval(id);
      if (retryTimerRef.current !== null) {
        window.clearTimeout(retryTimerRef.current);
        retryTimerRef.current = null;
      }
    };
  }, [authed, checkVersions]);

  // Refresh version snapshot whenever fleet machine attachments/versions change over SSE.
  const machineSig = useMemo(
    () => machines.map((m) => `${m.id}:${m.online ? 1 : 0}:${m.version ?? ""}`).join("|"),
    [machines]
  );
  useEffect(() => {
    if (!authed || !machineSig) return;
    void checkVersions(false);
  }, [authed, machineSig, checkVersions]);

  const hubUpdate: FleetUpdateView | undefined = verInfo?.update;

  // Fast-poll while a Hub-owned fleet update is active; trigger reload waiter when Hub enters restarting.
  useEffect(() => {
    if (!authed || !hubUpdate) return;
    if (hubUpdate.phase === "restarting" && hubUpdate.target) {
      void waitForHubRestartAndReload(hubUpdate.target);
      return;
    }
    if (!hubUpdate.busy) return;
    const id = window.setInterval(() => {
      void checkVersions(false);
    }, BUSY_POLL_MS);
    return () => window.clearInterval(id);
  }, [authed, hubUpdate?.busy, hubUpdate?.phase, hubUpdate?.target, checkVersions, waitForHubRestartAndReload]);

  const outdatedOnline = useMemo(() => selectOutdatedOnline(verInfo, machines), [verInfo, machines]);

  const startFleetUpdate = useCallback(async () => {
    if (hubUpdate?.busy || singleBusyId !== null || waitingRestartRef.current) return;
    setLocalErrors({});
    setSinglePhase("idle");
    try {
      const res = await api<{ ok: boolean; update?: FleetUpdateView }>("/api/update/fleet", {});
      if (res.update) {
        setVerInfo((prev) => (prev ? { ...prev, update: res.update } : prev));
        if (res.update.phase === "done") {
          toastRef.current("所有在线设备已是最新版本");
          void checkVersions(false);
        } else if (res.update.phase === "restarting" && res.update.target) {
          void waitForHubRestartAndReload(res.update.target);
        }
      }
    } catch (err) {
      const msg = err instanceof Error ? err.message : "启动升级失败";
      toastRef.current(msg);
      setLocalErrors((prev) => ({ ...prev, _fleet: msg }));
      void checkVersions(false);
    }
  }, [hubUpdate?.busy, singleBusyId, checkVersions, waitForHubRestartAndReload]);

  const startSingleUpdate = useCallback(
    async (machineId: string, name: string) => {
      if (hubUpdate?.busy || singleBusyId !== null || waitingRestartRef.current) return;
      setSingleBusyId(machineId);
      setSingleBusyName(name);
      setSinglePhase("updating");
      setLocalErrors((prev) => {
        const next = { ...prev };
        delete next[machineId];
        return next;
      });
      try {
        const qs = machineId ? `?machine=${encodeURIComponent(machineId)}` : "";
        const res = await api<{ version?: string; up_to_date?: boolean; relayed?: boolean }>(`/api/update${qs}`, {});
        if (res.up_to_date) {
          toastRef.current(`${name} 已是最新版本`);
          setSingleBusyId(null);
          setSinglePhase("idle");
          void checkVersions(false);
          return;
        }
        const ver = res.version || verInfo?.latest || "";
        if (!machineId) {
          toastRef.current(`Hub 已升级至 ${ver || "最新版"}，正在重启并自动刷新…`);
          await waitForHubRestartAndReload(ver);
          return;
        }
        toastRef.current(
          `${name} 已${res.relayed ? "通过 Hub 中转" : ""}升级至 ${ver || "最新版"}，等待重新上线…`
        );
        setSinglePhase("reconnecting");
        for (let i = 0; i < 16; i++) {
          await new Promise((r) => window.setTimeout(r, 1500));
          await checkVersions(false);
        }
        setSingleBusyId(null);
        setSinglePhase("idle");
      } catch (err) {
        const msg = err instanceof Error ? err.message : "升级失败";
        setSingleBusyId(null);
        setSinglePhase("failed");
        setLocalErrors((prev) => ({ ...prev, [machineId]: msg }));
        toastRef.current(`更新 ${name} 失败：${msg}`);
        void checkVersions(false);
      }
    },
    [hubUpdate?.busy, singleBusyId, verInfo?.latest, checkVersions, waitForHubRestartAndReload]
  );

  const mergedErrors = useMemo(
    () => ({ ...(hubUpdate?.errors ?? {}), ...localErrors }),
    [hubUpdate?.errors, localErrors]
  );

  const hubBusy = Boolean(hubUpdate?.busy);
  const singleBusy = singleBusyId !== null || waitingRestartRef.current;
  const busy = hubBusy || singleBusy;

  let phase: FleetUpdateController["phase"] = "idle";
  if (waitingRestartRef.current || hubUpdate?.phase === "restarting" || singlePhase === "restarting") {
    phase = "restarting";
  } else if (hubBusy) {
    phase = hubUpdate?.phase ?? "updating";
  } else if (singleBusy) {
    phase = singlePhase === "reconnecting" ? "reconnecting" : "updating";
  } else if (hubUpdate?.phase === "failed" || singlePhase === "failed") {
    phase = "failed";
  }

  const latest = restartingTarget || hubUpdate?.target || verInfo?.latest || "";
  const currentId = hubBusy ? (hubUpdate?.current_id ?? "") : singleBusyId;
  const currentName = hubBusy ? (hubUpdate?.current_name ?? "") : singleBusyName;
  const failedName = hubUpdate?.failed_name || singleBusyName || "设备";
  const error = hubUpdate?.error || localErrors._fleet || Object.values(localErrors)[0] || "";

  return {
    verInfo,
    outdatedOnline,
    latest,
    busy,
    phase,
    index: hubBusy ? (hubUpdate?.index ?? 0) : singleBusy ? 1 : 0,
    total: hubBusy ? (hubUpdate?.total ?? 0) : singleBusy ? 1 : 0,
    currentId,
    currentName,
    failedName,
    error,
    errorFor: (id: string) => mergedErrors[id] ?? "",
    checkVersions,
    startFleetUpdate,
    startSingleUpdate,
  };
}

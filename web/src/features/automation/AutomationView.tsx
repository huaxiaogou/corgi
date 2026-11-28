import { useCallback, useEffect, useState } from "react";
import type { FleetUpdateController } from "../../hooks/useFleetUpdate";
import { api } from "../../lib/api";
import type {
  DeviceView,
  Machine,
  MemorySnapshot,
  PendingWeComUser,
  Policy,
  RouterAPISecretStatus,
  Settings,
  Timer,
  TriggerView,
} from "../../types/api";
import { DevicesTab } from "./DevicesTab";
import { HooksTab } from "./HooksTab";
import { MemoryTab } from "./MemoryTab";
import { ProjectsTab } from "./ProjectsTab";
import { SettingsTab } from "./SettingsTab";
import { TimersTab } from "./TimersTab";

type TabKey = "settings" | "projects" | "hooks" | "memory" | "timers" | "devices";

interface SettingsResp {
  settings: Settings;
  has_router_api_key?: boolean;
  router_api_key_masked?: string;
  router_api_key_bound_origin?: string;
  router_api_key_origin_match?: boolean;
  effective_public_url?: string;
  flag_public_url?: string;
  policies: Policy[];
  wecom_online: boolean;
  wecom_error?: string;
  wecom_pending?: PendingWeComUser[];
  machines?: Machine[];
  hook_key: string;
}

const TABS: { key: TabKey; label: string }[] = [
  { key: "settings", label: "模型与运行" },
  { key: "projects", label: "项目工作区" },
  { key: "hooks", label: "企微与接入" },
  { key: "memory", label: "记忆与做梦" },
  { key: "timers", label: "定时与巡检" },
  { key: "devices", label: "设备与更新" },
];

export function AutomationView({
  fleetUpdate,
  onToast,
}: {
  fleetUpdate: FleetUpdateController;
  onToast: (msg: string) => void;
}) {
  const [tab, setTab] = useState<TabKey>("settings");
  const [settings, setSettings] = useState<Settings | null>(null);
  const [routerAPISecret, setRouterAPISecret] = useState<RouterAPISecretStatus>({ has_router_api_key: false });
  const [effectivePublicURL, setEffectivePublicURL] = useState("");
  const [flagPublicURL, setFlagPublicURL] = useState("");
  const [wecomOnline, setWecomOnline] = useState(false);
  const [wecomError, setWecomError] = useState("");
  const [wecomPending, setWecomPending] = useState<PendingWeComUser[]>([]);
  const [hookKey, setHookKey] = useState("");
  const [policies, setPolicies] = useState<Policy[]>([]);
  const [triggers, setTriggers] = useState<TriggerView[]>([]);
  const [memory, setMemory] = useState<MemorySnapshot | null>(null);
  const [timers, setTimers] = useState<Timer[]>([]);
  const [devices, setDevices] = useState<DeviceView[]>([]);
  const [machines, setMachines] = useState<Machine[]>([]);
  const [pairResult, setPairResult] = useState<{ code: string; kind: string; pair_url: string; addr: string } | null>(null);

  const loadSettingsMeta = useCallback(async () => {
    const r = await api<SettingsResp>("/api/super/settings");
    setSettings(r.settings);
    setRouterAPISecret({
      has_router_api_key: Boolean(r.has_router_api_key),
      router_api_key_masked: r.router_api_key_masked ?? "",
      router_api_key_bound_origin: r.router_api_key_bound_origin ?? "",
      router_api_key_origin_match: Boolean(r.router_api_key_origin_match),
    });
    setEffectivePublicURL(r.effective_public_url ?? "");
    setFlagPublicURL(r.flag_public_url ?? "");
    setWecomOnline(r.wecom_online);
    setWecomError(r.wecom_error ?? "");
    setWecomPending(r.wecom_pending ?? []);
    if (r.machines) setMachines(r.machines);
    setPolicies(r.policies ?? []);
    setHookKey(r.hook_key ?? "");
    return r.settings;
  }, []);

  const loadTab = useCallback(
    async (activeTab: TabKey) => {
      await loadSettingsMeta();
      if (activeTab === "hooks") {
        setTriggers((await api<{ triggers: TriggerView[] }>("/api/super/triggers")).triggers ?? []);
      } else if (activeTab === "memory") {
        setMemory(await api<MemorySnapshot>("/api/super/memory"));
      } else if (activeTab === "timers") {
        setTimers((await api<{ timers: Timer[] }>("/api/super/timers")).timers ?? []);
      } else if (activeTab === "devices") {
        const dr = await api<{ devices: DeviceView[]; machines: Machine[] }>("/api/devices");
        setDevices(dr.devices ?? []);
        setMachines(dr.machines ?? []);
      }
    },
    [loadSettingsMeta]
  );

  useEffect(() => {
    void loadTab(tab);
  }, [tab, loadTab]);

  // Live-poll WeCom status & pending single-chat claims on the WeCom & Integrations tab.
  useEffect(() => {
    if (tab !== "hooks" || !settings?.wecom_enabled) return;
    const id = window.setInterval(() => {
      void api<SettingsResp>("/api/super/settings").then((r) => {
        setWecomOnline(r.wecom_online);
        setWecomError(r.wecom_error ?? "");
        setWecomPending(r.wecom_pending ?? []);
      });
    }, 2500);
    return () => window.clearInterval(id);
  }, [tab, settings?.wecom_enabled]);

  const runAction = async (fn: () => Promise<void>, okMsg?: string) => {
    try {
      await fn();
      if (okMsg) onToast(okMsg);
      await loadTab(tab);
    } catch (err) {
      onToast(err instanceof Error ? err.message : "操作失败");
    }
  };

  // Patch only the fields owned by the active card against the latest authoritative Settings
  // so saving one tab never overwrites settings managed in another tab.
  const patchSettings = async (patch: Partial<Settings>, okMsg: string) => {
    await runAction(async () => {
      const base = settings ?? (await loadSettingsMeta());
      const merged: Settings = { ...base, ...patch };
      await api("/api/super/settings", { settings: merged });
      setSettings(merged);
    }, okMsg);
  };

  const approveWeCom = (userId: string, dismiss = false) =>
    runAction(() => api("/api/super/wecom/approve", { user_id: userId, dismiss }), dismiss ? "已忽略该请求" : `已授权绑定 ${userId}`);

  const genPair = async (kind: "browser" | "machine") => {
    const r = await api<{ code: string; kind: string; pair_url: string; addresses: string[] }>("/api/pair/new", { kind });
    setPairResult({ code: r.code, kind: r.kind, pair_url: r.pair_url, addr: r.addresses[0] ?? "" });
  };

  return (
    <div className="flex-1 flex flex-col min-h-0">
      <header className="h-14 px-5 flex items-center justify-between gap-2 border-b border-hairline shrink-0">
        <span className="font-semibold text-[15px] shrink-0">自动化与设置</span>
        <div className="inline-flex p-0.5 rounded-lg text-xs surface-rail border border-hairline overflow-x-auto">
          {TABS.map((t) => (
            <button
              key={t.key}
              type="button"
              onClick={() => setTab(t.key)}
              className={`px-2.5 py-1 rounded-md cursor-pointer shrink-0 ${tab === t.key ? "surface-bg font-medium text-primary" : "text-secondary"}`}
            >
              {t.label}
            </button>
          ))}
        </div>
      </header>
      <div className="flex-1 overflow-y-auto px-5 py-6">
        <div className="max-w-[740px] mx-auto text-xs">
          {tab === "settings" && settings && (
            <SettingsTab
              settings={settings}
              routerAPISecret={routerAPISecret}
              machines={machines}
              onSavePatch={patchSettings}
              onUpdateAPISecret={setRouterAPISecret}
              onToast={onToast}
            />
          )}
          {tab === "projects" && settings && (
            <ProjectsTab settings={settings} machines={machines} onSavePatch={patchSettings} />
          )}
          {tab === "hooks" && settings && (
            <HooksTab
              key={hookKey + policies.length}
              settings={settings}
              wecomOnline={wecomOnline}
              wecomError={wecomError}
              wecomPending={wecomPending}
              hookKey={hookKey}
              policies={policies}
              triggers={triggers}
              onSaveSettingsPatch={patchSettings}
              onApproveWeCom={approveWeCom}
              onSavePolicies={(next) => runAction(() => api("/api/super/settings", { policies: next }), "过滤策略已保存")}
              onToast={onToast}
            />
          )}
          {tab === "memory" && memory && (
            <MemoryTab
              memory={memory}
              settings={settings}
              onSaveFile={(path, text) => runAction(() => api("/api/super/memory", { path, text }), "已保存记忆文件")}
              onSaveSettingsPatch={patchSettings}
              onStartDream={() => runAction(() => api("/api/super/dream", {}), "Corgi 已进入做梦整理记忆")}
            />
          )}
          {tab === "timers" && (
            <TimersTab
              timers={timers}
              settings={settings}
              onAdd={(spec, text) => runAction(() => api("/api/super/timers", { spec, text }), "已添加定时任务")}
              onDelete={(id) => runAction(() => api(`/api/super/timers/${encodeURIComponent(id)}/delete`, {}))}
              onSaveSettingsPatch={patchSettings}
            />
          )}
          {tab === "devices" && (
            <DevicesTab
              devices={devices}
              machines={machines}
              settings={settings}
              effectivePublicURL={effectivePublicURL}
              flagPublicURL={flagPublicURL}
              fleetUpdate={fleetUpdate}
              pairResult={pairResult}
              onGenPair={genPair}
              onRevoke={(id) => runAction(() => api(`/api/devices/${encodeURIComponent(id)}/delete`, {}))}
              onSaveSettingsPatch={patchSettings}
              onToast={onToast}
            />
          )}
        </div>
      </div>
    </div>
  );
}

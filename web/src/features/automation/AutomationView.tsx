import { useEffect, useState } from "react";
import { api } from "../../lib/api";
import type { DeviceView, Machine, MemorySnapshot, PendingWeComUser, Policy, Settings, Timer, TriggerView } from "../../types/api";
import { DevicesTab } from "./DevicesTab";
import { HooksTab } from "./HooksTab";
import { MemoryTab } from "./MemoryTab";
import { SettingsTab } from "./SettingsTab";
import { TimersTab } from "./TimersTab";

type TabKey = "settings" | "hooks" | "memory" | "timers" | "devices";

interface SettingsResp {
  settings: Settings;
  policies: Policy[];
  wecom_online: boolean;
  wecom_error?: string;
  wecom_pending?: PendingWeComUser[];
  hub_agents?: string[];
  hook_key: string;
}

const TABS: { key: TabKey; label: string }[] = [
  { key: "settings", label: "企微与偏好" },
  { key: "hooks", label: "接入与策略" },
  { key: "memory", label: "记忆与做梦" },
  { key: "timers", label: "定时" },
  { key: "devices", label: "设备与多机" },
];

export function AutomationView({ onToast }: { onToast: (msg: string) => void }) {
  const [tab, setTab] = useState<TabKey>("settings");
  const [settings, setSettings] = useState<Settings | null>(null);
  const [allowText, setAllowText] = useState("");
  const [wecomOnline, setWecomOnline] = useState(false);
  const [wecomError, setWecomError] = useState("");
  const [wecomPending, setWecomPending] = useState<PendingWeComUser[]>([]);
  const [hubAgents, setHubAgents] = useState<string[]>([]);
  const [hookKey, setHookKey] = useState("");
  const [policies, setPolicies] = useState<Policy[]>([]);
  const [triggers, setTriggers] = useState<TriggerView[]>([]);
  const [memory, setMemory] = useState<MemorySnapshot | null>(null);
  const [timers, setTimers] = useState<Timer[]>([]);
  const [devices, setDevices] = useState<DeviceView[]>([]);
  const [machines, setMachines] = useState<Machine[]>([]);
  const [pairResult, setPairResult] = useState<{ code: string; kind: string; pair_url: string; addr: string } | null>(null);

  const load = async () => {
    if (tab === "settings" || tab === "hooks") {
      const r = await api<SettingsResp>("/api/super/settings");
      setSettings(r.settings);
      setAllowText((r.settings.wecom_allow_users ?? []).join(", "));
      setWecomOnline(r.wecom_online);
      setWecomError(r.wecom_error ?? "");
      setWecomPending(r.wecom_pending ?? []);
      setHubAgents(r.hub_agents ?? []);
      setPolicies(r.policies ?? []);
      setHookKey(r.hook_key ?? "");
      if (tab === "hooks") setTriggers((await api<{ triggers: TriggerView[] }>("/api/super/triggers")).triggers ?? []);
    } else if (tab === "memory") {
      setMemory(await api<MemorySnapshot>("/api/super/memory"));
    } else if (tab === "timers") {
      setTimers((await api<{ timers: Timer[] }>("/api/super/timers")).timers ?? []);
    } else if (tab === "devices") {
      const dr = await api<{ devices: DeviceView[]; machines: Machine[] }>("/api/devices");
      setDevices(dr.devices ?? []);
      setMachines(dr.machines ?? []);
    }
  };

  useEffect(() => {
    void load();
  }, [tab]);

  // Live-poll WeCom status & pending single-chat claims without overwriting unsaved inputs.
  useEffect(() => {
    if (tab !== "settings" || !settings?.wecom_enabled) return;
    const id = window.setInterval(() => {
      void api<SettingsResp>("/api/super/settings").then((r) => {
        setWecomOnline(r.wecom_online);
        setWecomError(r.wecom_error ?? "");
        setWecomPending(r.wecom_pending ?? []);
        setHubAgents(r.hub_agents ?? []);
      });
    }, 2500);
    return () => window.clearInterval(id);
  }, [tab, settings?.wecom_enabled]);

  const runAction = async (fn: () => Promise<void>, okMsg?: string) => {
    try {
      await fn();
      if (okMsg) onToast(okMsg);
      await load();
    } catch (err) {
      onToast(err instanceof Error ? err.message : "操作失败");
    }
  };

  const saveSettings = async () => {
    if (!settings) return;
    const allow = allowText.split(/[,，\s]+/).map((s) => s.trim()).filter(Boolean);
    await runAction(() => api("/api/super/settings", { settings: { ...settings, wecom_allow_users: allow } }), "设置已保存");
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
              allowText={allowText}
              wecomOnline={wecomOnline}
              wecomError={wecomError}
              wecomPending={wecomPending}
              hubAgents={hubAgents}
              onChange={setSettings}
              onAllowChange={setAllowText}
              onSave={saveSettings}
              onApproveWeCom={approveWeCom}
            />
          )}
          {tab === "hooks" && (
            <HooksTab
              key={hookKey + policies.length}
              hookKey={hookKey}
              policies={policies}
              triggers={triggers}
              onSavePolicies={(next) => runAction(() => api("/api/super/settings", { policies: next }), "过滤策略已保存")}
              onToast={onToast}
            />
          )}
          {tab === "memory" && memory && (
            <MemoryTab
              memory={memory}
              onSaveFile={(path, text) => runAction(() => api("/api/super/memory", { path, text }), "已保存记忆文件")}
              onStartDream={() => runAction(() => api("/api/super/dream", {}), "Corgi 已进入做梦整理记忆")}
            />
          )}
          {tab === "timers" && (
            <TimersTab
              timers={timers}
              onAdd={(spec, text) => runAction(() => api("/api/super/timers", { spec, text }), "已添加定时任务")}
              onDelete={(id) => runAction(() => api(`/api/super/timers/${encodeURIComponent(id)}/delete`, {}))}
            />
          )}
          {tab === "devices" && (
            <DevicesTab
              devices={devices}
              machines={machines}
              pairResult={pairResult}
              onGenPair={genPair}
              onRevoke={(id) => runAction(() => api(`/api/devices/${encodeURIComponent(id)}/delete`, {}))}
              onToast={onToast}
            />
          )}
        </div>
      </div>
    </div>
  );
}

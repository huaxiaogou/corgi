import { useEffect, useState } from "react";
import { api } from "../../lib/api";
import type { DeviceView, MemorySnapshot, Policy, Settings, Timer, TriggerView } from "../../types/api";
import { DevicesTab } from "./DevicesTab";
import { HooksTab } from "./HooksTab";
import { MemoryTab } from "./MemoryTab";
import { SettingsTab } from "./SettingsTab";
import { TimersTab } from "./TimersTab";

interface Props {
  onToast: (msg: string) => void;
}

type TabKey = "settings" | "hooks" | "memory" | "timers" | "devices";

const TABS: { key: TabKey; label: string }[] = [
  { key: "settings", label: "企微与偏好" },
  { key: "hooks", label: "接入与策略" },
  { key: "memory", label: "记忆与做梦" },
  { key: "timers", label: "定时" },
  { key: "devices", label: "设备与多机" },
];

export function AutomationView({ onToast }: Props) {
  const [tab, setTab] = useState<TabKey>("settings");
  const [settings, setSettings] = useState<Settings | null>(null);
  const [allowText, setAllowText] = useState("");
  const [wecomOnline, setWecomOnline] = useState(false);
  const [hookKey, setHookKey] = useState("");
  const [policies, setPolicies] = useState<Policy[]>([]);
  const [triggers, setTriggers] = useState<TriggerView[]>([]);
  const [memory, setMemory] = useState<MemorySnapshot | null>(null);
  const [timers, setTimers] = useState<Timer[]>([]);
  const [devices, setDevices] = useState<DeviceView[]>([]);
  const [pairResult, setPairResult] = useState<{ code: string; kind: string; pair_url: string; addr: string } | null>(null);

  const load = async () => {
    if (tab === "settings" || tab === "hooks") {
      const r = await api<{ settings: Settings; policies: Policy[]; wecom_online: boolean; hook_key: string }>("/api/super/settings");
      setSettings(r.settings);
      setAllowText((r.settings.wecom_allow_users ?? []).join(", "));
      setWecomOnline(r.wecom_online);
      setPolicies(r.policies ?? []);
      setHookKey(r.hook_key ?? "");
      if (tab === "hooks") {
        const tr = await api<{ triggers: TriggerView[] }>("/api/super/triggers");
        setTriggers(tr.triggers ?? []);
      }
    } else if (tab === "memory") {
      setMemory(await api<MemorySnapshot>("/api/super/memory"));
    } else if (tab === "timers") {
      const r = await api<{ timers: Timer[] }>("/api/super/timers");
      setTimers(r.timers ?? []);
    } else if (tab === "devices") {
      const r = await api<{ devices: DeviceView[] }>("/api/devices");
      setDevices(r.devices ?? []);
    }
  };

  useEffect(() => {
    void load();
  }, [tab]);

  const saveSettings = async () => {
    if (!settings) return;
    const allow = allowText
      .split(/[,，\s]+/)
      .map((s) => s.trim())
      .filter(Boolean);
    try {
      await api("/api/super/settings", { settings: { ...settings, wecom_allow_users: allow } });
      onToast("设置已保存");
      await load();
    } catch (err) {
      onToast(err instanceof Error ? err.message : "保存失败");
    }
  };

  const savePolicies = async (next: Policy[]) => {
    try {
      await api("/api/super/settings", { policies: next });
      onToast("过滤策略已保存");
      await load();
    } catch (err) {
      onToast(err instanceof Error ? err.message : "保存失败");
    }
  };

  const saveMemory = async (path: string, text: string) => {
    try {
      await api("/api/super/memory", { path, text });
      onToast("已保存记忆文件");
      await load();
    } catch (err) {
      onToast(err instanceof Error ? err.message : "保存失败");
    }
  };

  const startDream = async () => {
    try {
      await api("/api/super/dream", {});
      onToast("Corgi 已进入做梦整理记忆");
      await load();
    } catch (err) {
      onToast(err instanceof Error ? err.message : "启动失败");
    }
  };

  const addTimer = async (spec: string, text: string) => {
    try {
      await api("/api/super/timers", { spec, text });
      onToast("已添加定时任务");
      await load();
    } catch (err) {
      onToast(err instanceof Error ? err.message : "添加失败");
    }
  };

  const deleteTimer = async (id: string) => {
    await api(`/api/super/timers/${encodeURIComponent(id)}/delete`, {});
    await load();
  };

  const genPair = async (kind: "browser" | "machine") => {
    const r = await api<{ code: string; kind: string; pair_url: string; addresses: string[] }>("/api/pair/new", { kind });
    setPairResult({ code: r.code, kind: r.kind, pair_url: r.pair_url, addr: r.addresses[0] ?? "" });
  };

  const revokeDevice = async (id: string) => {
    await api(`/api/devices/${encodeURIComponent(id)}/delete`, {});
    await load();
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
              onChange={setSettings}
              onAllowChange={setAllowText}
              onSave={saveSettings}
            />
          )}
          {tab === "hooks" && (
            <HooksTab
              key={hookKey + policies.length}
              hookKey={hookKey}
              policies={policies}
              triggers={triggers}
              onSavePolicies={savePolicies}
              onToast={onToast}
            />
          )}
          {tab === "memory" && memory && <MemoryTab memory={memory} onSaveFile={saveMemory} onStartDream={startDream} />}
          {tab === "timers" && <TimersTab timers={timers} onAdd={addTimer} onDelete={deleteTimer} />}
          {tab === "devices" && <DevicesTab devices={devices} pairResult={pairResult} onGenPair={genPair} onRevoke={revokeDevice} />}
        </div>
      </div>
    </div>
  );
}

import { useState } from "react";
import { FolderOpen } from "lucide-react";
import { DirPicker } from "../../components/DirPicker";
import type { Machine, PendingWeComUser, Settings } from "../../types/api";
import { ModelEffortFields } from "./ModelEffortFields";
import { WeComSection } from "./WeComSection";

interface Props {
  settings: Settings;
  allowText: string;
  wecomOnline: boolean;
  wecomError?: string;
  wecomPending?: PendingWeComUser[];
  hubAgents?: string[];
  machines?: Machine[];
  onChange: (next: Settings) => void;
  onAllowChange: (val: string) => void;
  onSave: () => Promise<void>;
  onApproveWeCom?: (userId: string, dismiss?: boolean) => Promise<void>;
}

export function SettingsTab({
  settings,
  allowText,
  wecomOnline,
  wecomError,
  wecomPending = [],
  hubAgents = [],
  machines = [],
  onChange,
  onAllowChange,
  onSave,
  onApproveWeCom,
}: Props) {
  const [dirOpen, setDirOpen] = useState(false);

  const workerMachineObj = machines.find((m) => m.id === (settings.worker_machine ?? "") || (!settings.worker_machine && m.local));

  return (
    <div className="space-y-5">
      <WeComSection
        settings={settings}
        allowText={allowText}
        wecomOnline={wecomOnline}
        wecomError={wecomError}
        wecomPending={wecomPending}
        onChange={onChange}
        onAllowChange={onAllowChange}
        onApproveWeCom={onApproveWeCom}
      />

      <section className="p-4 rounded-xl border border-hairline space-y-3">
        <div>
          <div className="font-semibold text-sm">Corgi 大脑（运行在 Hub）</div>
          <div className="text-[11.5px] text-muted mt-0.5">
            只负责理解意图、分派任务和简短回复，建议选用轻量快速模型并将思考强度设为 <span className="font-mono-code">low</span>。
          </div>
        </div>
        <ModelEffortFields
          kind={settings.router_kind}
          model={settings.router_model ?? ""}
          effort={settings.router_effort ?? ""}
          allowedKinds={hubAgents}
          recommendLowEffort
          onChange={({ kind, model, effort }) =>
            onChange({ ...settings, router_kind: kind, router_model: model, router_effort: effort })
          }
        />
      </section>

      <section className="p-4 rounded-xl border border-hairline space-y-3">
        <div>
          <div className="font-semibold text-sm">派活默认（Worker）</div>
          <div className="text-[11.5px] text-muted mt-0.5">
            Corgi 派 Agent 干活时的默认配置；你在聊天框或消息里指定电脑、Agent 或模型时会优先按你指定的运行。
          </div>
        </div>
        <ModelEffortFields
          kind={settings.worker_kind}
          model={settings.worker_model ?? ""}
          effort={settings.worker_effort ?? ""}
          machine={settings.worker_machine ?? ""}
          allowedKinds={workerMachineObj?.agents}
          onChange={({ kind, model, effort }) =>
            onChange({ ...settings, worker_kind: kind, worker_model: model, worker_effort: effort })
          }
        />
        <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
          <label className="space-y-1">
            <span className="text-muted">默认运行电脑</span>
            <select
              value={settings.worker_machine ?? ""}
              onChange={(e) => onChange({ ...settings, worker_machine: e.target.value })}
              className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline"
            >
              {machines.length === 0 && <option value="">Hub 本机</option>}
              {machines.map((m) => (
                <option key={m.id || "hub"} value={m.id}>
                  {m.name}
                  {m.local ? "（Hub）" : m.online ? "" : "（离线）"}
                </option>
              ))}
            </select>
          </label>
          <div className="sm:col-span-2 space-y-1">
            <span className="text-muted">默认工作目录</span>
            <div className="flex gap-2">
              <input
                value={settings.worker_cwd ?? ""}
                onChange={(e) => onChange({ ...settings, worker_cwd: e.target.value })}
                className="flex-1 px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
              />
              <button
                type="button"
                onClick={() => setDirOpen((v) => !v)}
                className="px-3 py-2 rounded-lg border border-strong text-secondary surface-hover cursor-pointer inline-flex items-center gap-1 shrink-0"
              >
                <FolderOpen className="w-3.5 h-3.5" />
                浏览
              </button>
            </div>
          </div>
        </div>
        {dirOpen && (
          <DirPicker
            value={settings.worker_cwd || "~"}
            machine={settings.worker_machine ?? ""}
            onChange={(p) => onChange({ ...settings, worker_cwd: p })}
          />
        )}
      </section>

      <section className="p-4 rounded-xl border border-hairline space-y-3">
        <div className="font-semibold text-sm">会话交接、通知与权限</div>
        <div className="grid grid-cols-2 sm:grid-cols-5 gap-3">
          <label className="space-y-1">
            <span className="text-muted">回复字数预算</span>
            <input
              type="number"
              min={40}
              max={1000}
              value={settings.reply_chars || 120}
              onChange={(e) => onChange({ ...settings, reply_chars: Number(e.target.value) || 120 })}
              className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
            />
          </label>
          <label className="space-y-1">
            <span className="text-muted">卡死判定 (分钟)</span>
            <input
              type="number"
              min={1}
              max={120}
              value={settings.stall_minutes || 12}
              onChange={(e) => onChange({ ...settings, stall_minutes: Number(e.target.value) || 12 })}
              className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
            />
          </label>
          <label className="space-y-1">
            <span className="text-muted">交接阈值 (%)</span>
            <input
              type="number"
              min={-1}
              max={95}
              value={settings.rotate_pct ?? 60}
              onChange={(e) => onChange({ ...settings, rotate_pct: Number(e.target.value) })}
              className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
            />
          </label>
          <label className="space-y-1">
            <span className="text-muted">夜间做梦时间</span>
            <input
              placeholder="04:00 或 off"
              value={settings.dream_at ?? "04:00"}
              onChange={(e) => onChange({ ...settings, dream_at: e.target.value })}
              className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
            />
          </label>
          <label className="space-y-1 col-span-2 sm:col-span-1">
            <span className="text-muted">企微推送节奏</span>
            <select
              value={settings.notify_level ?? "quiet"}
              onChange={(e) => onChange({ ...settings, notify_level: e.target.value as "quiet" | "normal" })}
              className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline"
            >
              <option value="quiet">安静（看网页不打扰）</option>
              <option value="normal">常规（20s 合批）</option>
            </select>
          </label>
        </div>

        <label className="flex items-center gap-2 cursor-pointer">
          <input
            type="checkbox"
            checked={Boolean(settings.ask_permissions)}
            onChange={(e) => onChange({ ...settings, ask_permissions: e.target.checked })}
          />
          <span>严格逐条确认模式（关闭默认免权限 bypass，需重启大脑与新派任务生效）</span>
        </label>
      </section>

      <button type="button" onClick={() => void onSave()} className="px-4 py-2 rounded-lg btn-accent font-medium cursor-pointer">
        保存设置
      </button>
    </div>
  );
}

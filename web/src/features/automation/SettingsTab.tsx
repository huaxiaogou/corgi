import type { Settings } from "../../types/api";

interface Props {
  settings: Settings;
  allowText: string;
  wecomOnline: boolean;
  wecomError?: string;
  onChange: (next: Settings) => void;
  onAllowChange: (val: string) => void;
  onSave: () => Promise<void>;
}

const AGENT_KINDS = ["claude", "codex", "agy", "codebuddy"] as const;

export function SettingsTab({ settings, allowText, wecomOnline, wecomError, onChange, onAllowChange, onSave }: Props) {
  return (
    <div className="space-y-5">
      <section className="p-4 rounded-xl border border-hairline space-y-3">
        <div className="flex items-center justify-between">
          <span className="font-semibold text-sm">企业微信智能机器人（WebSocket 长连接）</span>
          <span className="font-mono-code" style={{ color: wecomOnline ? "var(--ok)" : "var(--fg-3)" }}>
            {wecomOnline ? "● 已连接" : "○ 未连接"}
          </span>
        </div>
        <label className="flex items-center gap-2 cursor-pointer">
          <input
            type="checkbox"
            checked={Boolean(settings.wecom_enabled)}
            onChange={(e) => onChange({ ...settings, wecom_enabled: e.target.checked })}
          />
          <span>启用企微长连接（免公网 IP）</span>
        </label>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <input
            placeholder="BotID"
            value={settings.wecom_bot_id ?? ""}
            onChange={(e) => onChange({ ...settings, wecom_bot_id: e.target.value })}
            className="px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
          />
          <input
            type="password"
            placeholder="Secret"
            value={settings.wecom_secret ?? ""}
            onChange={(e) => onChange({ ...settings, wecom_secret: e.target.value })}
            className="px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
          />
        </div>
        <input
          placeholder="允许谁在企微指挥 Corgi（企微 userid，多人用逗号分隔，必填）"
          value={allowText}
          onChange={(e) => onAllowChange(e.target.value)}
          className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
        />
        <input
          placeholder="默认私聊推送给谁（可选，留空自动推给白名单第一位）"
          value={settings.wecom_default_to ?? ""}
          onChange={(e) => onChange({ ...settings, wecom_default_to: e.target.value })}
          className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
        />
        {settings.wecom_enabled && !wecomOnline && wecomError && (
          <div className="px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code break-all" style={{ color: "var(--attn)" }}>
            连接失败：{wecomError}
          </div>
        )}
      </section>

      <section className="p-4 rounded-xl border border-hairline space-y-3">
        <div className="font-semibold text-sm">Super-Agent 执行与常驻策略</div>
        <div className="grid grid-cols-2 gap-3">
          <label className="space-y-1">
            <span className="text-muted">大脑 Agent (Router)</span>
            <select
              value={settings.router_kind}
              onChange={(e) => onChange({ ...settings, router_kind: e.target.value })}
              className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline"
            >
              {AGENT_KINDS.map((k) => (
                <option key={k} value={k}>
                  {k}
                </option>
              ))}
            </select>
          </label>
          <label className="space-y-1">
            <span className="text-muted">默认 Worker</span>
            <select
              value={settings.worker_kind}
              onChange={(e) => onChange({ ...settings, worker_kind: e.target.value })}
              className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline"
            >
              {AGENT_KINDS.map((k) => (
                <option key={k} value={k}>
                  {k}
                </option>
              ))}
            </select>
          </label>
        </div>

        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
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
          <label className="space-y-1">
            <span className="text-muted">企微推送节奏</span>
            <select
              value={settings.notify_level ?? "quiet"}
              onChange={(e) => onChange({ ...settings, notify_level: e.target.value as "quiet" | "normal" })}
              className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline"
            >
              <option value="quiet">安静（看网页时不打扰）</option>
              <option value="normal">常规（20s 合批推送）</option>
            </select>
          </label>
        </div>

        <label className="flex items-center gap-2 cursor-pointer">
          <input
            type="checkbox"
            checked={Boolean(settings.ask_permissions)}
            onChange={(e) => onChange({ ...settings, ask_permissions: e.target.checked })}
          />
          <span>严格逐条确认模式（关闭默认免权限 bypass）</span>
        </label>
      </section>

      <button type="button" onClick={() => void onSave()} className="px-4 py-2 rounded-lg btn-accent font-medium cursor-pointer">
        保存设置
      </button>
    </div>
  );
}

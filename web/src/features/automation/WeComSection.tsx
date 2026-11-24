import type { PendingWeComUser, Settings } from "../../types/api";

interface Props {
  settings: Settings;
  allowText: string;
  wecomOnline: boolean;
  wecomError?: string;
  wecomPending?: PendingWeComUser[];
  onChange: (next: Settings) => void;
  onAllowChange: (val: string) => void;
  onApproveWeCom?: (userId: string, dismiss?: boolean) => Promise<void>;
}

export function WeComSection({
  settings,
  allowText,
  wecomOnline,
  wecomError,
  wecomPending = [],
  onChange,
  onAllowChange,
  onApproveWeCom,
}: Props) {
  return (
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
        placeholder="已授权的企微匿名 ID（如 T81900036A；留空时直接私聊机器人发一句即可一键认主）"
        value={allowText}
        onChange={(e) => onAllowChange(e.target.value)}
        className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
      />
      {wecomPending.length > 0 && onApproveWeCom && (
        <div className="rounded-lg border border-strong surface-rail divide-y divide-[var(--line)]">
          {wecomPending.map((p) => (
            <div key={p.user_id} className="px-3 py-2.5 flex items-center justify-between gap-3 flex-wrap">
              <div className="min-w-0 space-y-0.5">
                <div>
                  <span className="text-secondary">收到企微私聊待认主：</span>
                  <code className="font-mono-code font-medium text-primary ml-1">{p.user_id}</code>
                </div>
                {p.preview && <div className="text-muted truncate">“{p.preview}”</div>}
              </div>
              <div className="flex items-center gap-2 shrink-0">
                <button
                  type="button"
                  onClick={() => void onApproveWeCom(p.user_id, false)}
                  className="px-2.5 py-1 rounded-md btn-accent font-medium cursor-pointer"
                >
                  一键加入白名单
                </button>
                <button type="button" onClick={() => void onApproveWeCom(p.user_id, true)} className="text-muted hover:underline cursor-pointer">
                  忽略
                </button>
              </div>
            </div>
          ))}
        </div>
      )}
      {wecomOnline && allowText.trim() === "" && wecomPending.length === 0 && (
        <div className="text-muted">
          已连接。在企微里给机器人私聊发一句「你好」，这里会自动弹出你的账号并可一键认主（无需手查编码）。
        </div>
      )}
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
  );
}

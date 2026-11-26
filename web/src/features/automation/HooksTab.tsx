import { useState } from "react";
import type { PendingWeComUser, Policy, Settings, TriggerView } from "../../types/api";
import { WeComSection } from "./WeComSection";

interface Props {
  settings: Settings;
  wecomOnline: boolean;
  wecomError?: string;
  wecomPending?: PendingWeComUser[];
  hookKey: string;
  policies: Policy[];
  triggers: TriggerView[];
  onSaveSettingsPatch: (patch: Partial<Settings>, okMsg: string) => Promise<void>;
  onApproveWeCom?: (userId: string, dismiss?: boolean) => Promise<void>;
  onSavePolicies: (next: Policy[]) => Promise<void>;
  onToast: (msg: string) => void;
}

export function HooksTab({
  settings,
  wecomOnline,
  wecomError,
  wecomPending = [],
  hookKey,
  policies,
  triggers,
  onSaveSettingsPatch,
  onApproveWeCom,
  onSavePolicies,
  onToast,
}: Props) {
  const [list, setList] = useState<Policy[]>(policies);
  const [newSrc, setNewSrc] = useState("webhook:github");
  const [newAct, setNewAct] = useState<"respond" | "candidate" | "ignore">("candidate");
  const [newAllow, setNewAllow] = useState("");

  const origin = window.location.origin;
  const ghURL = `${origin}/hooks/github?key=${hookKey}`;
  const genericURL = `${origin}/hooks/<source>?key=${hookKey}`;

  const copy = (text: string) => {
    void navigator.clipboard.writeText(text).then(() => onToast("已复制 Webhook 地址"));
  };

  const addPolicy = () => {
    const src = newSrc.trim();
    if (!src) return;
    const allow = newAllow
      .split(/[,，\s]+/)
      .map((s) => s.trim())
      .filter(Boolean);
    const next = [...list.filter((p) => p.source !== src), { source: src, action: newAct, allow: allow.length ? allow : undefined }];
    setList(next);
    setNewSrc("");
    setNewAllow("");
  };

  const removePolicy = (src: string) => {
    setList((prev) => prev.filter((p) => p.source !== src));
  };

  return (
    <div className="space-y-5">
      <WeComSection
        settings={settings}
        wecomOnline={wecomOnline}
        wecomError={wecomError}
        wecomPending={wecomPending}
        onSavePatch={onSaveSettingsPatch}
        onApproveWeCom={onApproveWeCom}
      />

      <section className="p-4 rounded-xl border border-hairline space-y-3">
        <div className="font-semibold text-sm">外部 Webhook 接入（202 异步入箱）</div>
        <div className="space-y-2">
          <div className="flex items-center justify-between gap-2 p-2.5 rounded-lg surface-rail border border-hairline">
            <div className="min-w-0">
              <div className="text-muted mb-0.5">GitHub Webhook（application/json）</div>
              <div className="font-mono-code truncate">{ghURL}</div>
            </div>
            <button type="button" onClick={() => copy(ghURL)} className="px-2.5 py-1 rounded-md border border-strong shrink-0 cursor-pointer">
              复制
            </button>
          </div>
          <div className="flex items-center justify-between gap-2 p-2.5 rounded-lg surface-rail border border-hairline">
            <div className="min-w-0">
              <div className="text-muted mb-0.5">通用 JSON Webhook（支持 X-Corgi-Thread / X-Idempotency-Key）</div>
              <div className="font-mono-code truncate">{genericURL}</div>
            </div>
            <button type="button" onClick={() => copy(genericURL)} className="px-2.5 py-1 rounded-md border border-strong shrink-0 cursor-pointer">
              复制
            </button>
          </div>
        </div>
      </section>

      <section className="p-4 rounded-xl border border-hairline space-y-3">
        <div className="font-semibold text-sm">收件箱快路径过滤策略（respond / candidate / ignore）</div>
        <div className="flex flex-wrap gap-2">
          <input
            value={newSrc}
            onChange={(e) => setNewSrc(e.target.value)}
            placeholder="来源（如 webhook:github）"
            className="w-44 px-2.5 py-1.5 rounded-lg font-mono-code surface-rail border border-hairline outline-none"
          />
          <select
            value={newAct}
            onChange={(e) => setNewAct(e.target.value as "respond" | "candidate" | "ignore")}
            className="px-2.5 py-1.5 rounded-lg surface-rail border border-hairline"
          >
            <option value="respond">respond（直接处理）</option>
            <option value="candidate">candidate（仅规则命中时处理）</option>
            <option value="ignore">ignore（直接丢弃）</option>
          </select>
          <input
            value={newAllow}
            onChange={(e) => setNewAllow(e.target.value)}
            placeholder="允许发送者（可选，逗号分隔）"
            className="flex-1 min-w-[160px] px-2.5 py-1.5 rounded-lg font-mono-code surface-rail border border-hairline outline-none"
          />
          <button type="button" onClick={addPolicy} className="px-3 py-1.5 rounded-lg border border-strong cursor-pointer">
            添加
          </button>
        </div>

        <div className="rounded-lg border border-hairline divide-y divide-[var(--line)]">
          {list.length === 0 && <div className="p-3 text-muted">暂无自定义规则（未匹配来源默认 candidate）</div>}
          {list.map((p) => (
            <div key={p.source} className="px-3 py-2 flex items-center justify-between gap-2">
              <div className="flex items-center gap-2 min-w-0">
                <span className="font-mono-code font-medium">{p.source}</span>
                <span className="px-1.5 py-0.5 rounded surface-rail font-mono-code text-secondary">{p.action}</span>
                {p.allow && p.allow.length > 0 && <span className="text-muted truncate">allow: {p.allow.join(", ")}</span>}
              </div>
              <button type="button" onClick={() => removePolicy(p.source)} className="text-muted hover:underline cursor-pointer">
                删除
              </button>
            </div>
          ))}
        </div>

        <button type="button" onClick={() => void onSavePolicies(list)} className="px-4 py-1.5 rounded-lg btn-accent font-medium cursor-pointer">
          保存策略
        </button>
      </section>

      <section className="p-4 rounded-xl border border-hairline space-y-2">
        <div className="font-semibold text-sm">近期收件箱触发记录（{triggers.length}）</div>
        <div className="max-h-56 overflow-y-auto rounded-lg border border-hairline divide-y divide-[var(--line)]">
          {triggers.length === 0 && <div className="p-3 text-muted">暂无触发事件</div>}
          {triggers.slice(0, 30).map((t) => (
            <div key={t.id} className="px-3 py-2 space-y-0.5">
              <div className="flex items-center justify-between gap-2 text-[11px] font-mono-code text-muted">
                <span>
                  {t.id} · {t.source} · {t.thread}
                </span>
                <span>
                  {t.policy} / {t.status}
                </span>
              </div>
              <div className="truncate text-secondary">{t.text}</div>
            </div>
          ))}
        </div>
      </section>
    </div>
  );
}

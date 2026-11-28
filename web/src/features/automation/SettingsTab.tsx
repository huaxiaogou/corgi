import { useEffect, useMemo, useState } from "react";
import { normalizeKind } from "../../lib/dispatchDefaults";
import { TOKENHUB_BASE_URL, resolveRouterAPIEffort } from "../../lib/routerApiModels";
import type { Machine, ModelEffortSpec, RouterAPISecretStatus, Settings } from "../../types/api";
import { RouterAPISection } from "./RouterAPISection";
import { WorkerDispatchSection } from "./WorkerDispatchSection";

interface Props {
  settings: Settings;
  routerAPISecret?: RouterAPISecretStatus;
  machines?: Machine[];
  onSavePatch: (patch: Partial<Settings>, okMsg: string) => Promise<void>;
  onUpdateAPISecret?: (next: RouterAPISecretStatus) => void;
  onToast?: (msg: string) => void;
}

export function SettingsTab({
  settings,
  routerAPISecret,
  machines = [],
  onSavePatch,
  onUpdateAPISecret,
  onToast,
}: Props) {
  const [draft, setDraft] = useState<Settings>(settings);

  useEffect(() => {
    setDraft(settings);
  }, [settings]);

  const kindDefaults: Record<string, ModelEffortSpec> = useMemo(() => {
    const base: Record<string, ModelEffortSpec> = { ...(draft.kind_defaults ?? {}) };
    const wk = normalizeKind(draft.worker_kind);
    if (wk && !base[wk] && (draft.worker_model || draft.worker_effort)) {
      base[wk] = { model: draft.worker_model ?? "", effort: draft.worker_effort ?? "" };
    }
    return base;
  }, [draft.kind_defaults, draft.worker_kind, draft.worker_model, draft.worker_effort]);

  const save = () => {
    const apiProvider = draft.router_api_provider === "openai" ? "openai" : "tokenhub";
    const effectiveAPIBaseURL = apiProvider === "tokenhub" ? TOKENHUB_BASE_URL : draft.router_api_base_url ?? "";
    const effectiveAPIModel = (draft.router_api_model ?? "").trim() || (apiProvider === "tokenhub" ? "hy4" : "");
    const effectiveAPIEffort = resolveRouterAPIEffort(apiProvider, effectiveAPIModel, draft.router_api_effort);

    const cleanKindDefaults: Record<string, ModelEffortSpec> = {};
    for (const [k, spec] of Object.entries(kindDefaults)) {
      const m = (spec.model ?? "").trim();
      const ef = (spec.effort ?? "").trim();
      if (m || ef) cleanKindDefaults[k] = { model: m, effort: ef };
    }
    const cleanOverrides: Record<string, Record<string, ModelEffortSpec>> = {};
    for (const [mk, byKind] of Object.entries(draft.machine_kind_overrides ?? {})) {
      for (const [k, spec] of Object.entries(byKind ?? {})) {
        const m = (spec.model ?? "").trim();
        const ef = (spec.effort ?? "").trim();
        if (m || ef) {
          cleanOverrides[mk] = { ...(cleanOverrides[mk] ?? {}), [k]: { model: m, effort: ef } };
        }
      }
    }
    const wk = normalizeKind(draft.worker_kind);
    const activeDefault = cleanKindDefaults[wk];
    return onSavePatch(
      {
        router_api_provider: apiProvider,
        router_api_base_url: effectiveAPIBaseURL,
        router_api_model: effectiveAPIModel,
        router_api_effort: effectiveAPIEffort,
        worker_kind: draft.worker_kind,
        worker_model: activeDefault?.model ?? draft.worker_model ?? "",
        worker_effort: activeDefault?.effort ?? draft.worker_effort ?? "",
        kind_defaults: cleanKindDefaults,
        machine_kind_overrides: cleanOverrides,
        worker_machine: draft.worker_machine,
        worker_cwd: draft.worker_cwd,
        reply_chars: draft.reply_chars,
        stall_minutes: draft.stall_minutes,
        ask_permissions: draft.ask_permissions,
      },
      "模型与运行设置已保存"
    );
  };

  return (
    <div className="space-y-5">
      <RouterAPISection
        draft={draft}
        setDraft={setDraft}
        routerAPISecret={routerAPISecret}
        onUpdateAPISecret={onUpdateAPISecret}
        onToast={onToast}
      />

      <WorkerDispatchSection draft={draft} setDraft={setDraft} kindDefaults={kindDefaults} machines={machines} />

      <section className="p-4 rounded-xl border border-hairline space-y-3">
        <div className="font-semibold text-sm">对话与执行偏好</div>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <label className="space-y-1">
            <span className="text-muted">回复字数预算</span>
            <input
              type="number"
              min={40}
              max={1000}
              value={draft.reply_chars || 200}
              onChange={(e) => setDraft((prev) => ({ ...prev, reply_chars: Number(e.target.value) || 200 }))}
              className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
            />
          </label>
          <label className="space-y-1">
            <span className="text-muted">Worker 卡死判定 (分钟)</span>
            <input
              type="number"
              min={1}
              max={120}
              value={draft.stall_minutes || 12}
              onChange={(e) => setDraft((prev) => ({ ...prev, stall_minutes: Number(e.target.value) || 12 }))}
              className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
            />
          </label>
        </div>

        <label className="flex items-center gap-2 cursor-pointer">
          <input
            type="checkbox"
            checked={Boolean(draft.ask_permissions)}
            onChange={(e) => setDraft((prev) => ({ ...prev, ask_permissions: e.target.checked }))}
          />
          <span>严格逐条确认模式（关闭默认免权限 bypass，需重启大脑与新派任务生效）</span>
        </label>
      </section>

      <button type="button" onClick={() => void save()} className="px-4 py-2 rounded-lg btn-accent font-medium cursor-pointer">
        保存模型与运行设置
      </button>
    </div>
  );
}

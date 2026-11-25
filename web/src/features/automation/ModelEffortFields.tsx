import { useEffect, useState } from "react";
import { api } from "../../lib/api";
import type { ModelCatalog } from "../../types/api";

const FALLBACK_EFFORTS = ["low", "medium", "high", "xhigh", "max"];
const AGENT_KINDS = ["claude", "codex", "agy", "codebuddy"] as const;

interface Props {
  kind: string;
  model: string;
  effort: string;
  machine?: string;
  allowedKinds?: string[];
  recommendLowEffort?: boolean;
  onChange: (next: { kind: string; model: string; effort: string }) => void;
}

export function ModelEffortFields({
  kind,
  model,
  effort,
  machine = "",
  allowedKinds,
  recommendLowEffort = false,
  onChange,
}: Props) {
  const [catalog, setCatalog] = useState<ModelCatalog | null>(null);

  useEffect(() => {
    if (!kind) return;
    const qs = new URLSearchParams({ kind });
    if (machine) qs.set("machine", machine);
    void api<ModelCatalog>(`/api/models?${qs.toString()}`)
      .then(setCatalog)
      .catch(() => setCatalog(null));
  }, [kind, machine]);

  const selectedModel = catalog?.models.find((m) => m.id === model);
  const efforts =
    selectedModel?.efforts && selectedModel.efforts.length > 0
      ? selectedModel.efforts
      : catalog?.efforts && catalog.efforts.length > 0
        ? catalog.efforts
        : FALLBACK_EFFORTS;

  const isInstalled = (k: string) => !allowedKinds || allowedKinds.length === 0 || allowedKinds.includes(k);
  const models = catalog?.models ?? [];
  const hasSavedModel = !model || models.some((m) => m.id === model);

  return (
    <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
      <label className="space-y-1">
        <span className="text-muted">Agent</span>
        <select
          value={kind}
          onChange={(e) => onChange({ kind: e.target.value, model: "", effort: recommendLowEffort ? "low" : "" })}
          className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline"
        >
          {AGENT_KINDS.map((k) => (
            <option key={k} value={k}>
              {isInstalled(k) ? k : `${k}（未安装）`}
            </option>
          ))}
        </select>
      </label>

      <label className="space-y-1">
        <span className="text-muted">模型</span>
        <select
          value={model}
          onChange={(e) => onChange({ kind, model: e.target.value, effort })}
          className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline"
        >
          <option value="">{catalog?.default ? `默认 (${catalog.default})` : "默认模型"}</option>
          {!hasSavedModel && <option value={model}>{model}</option>}
          {models.map((m) => (
            <option key={m.id} value={m.id}>
              {m.label || m.id}
            </option>
          ))}
        </select>
      </label>

      <label className="space-y-1">
        <span className="text-muted">思考强度（推理程度）</span>
        <select
          value={effort}
          onChange={(e) => onChange({ kind, model, effort: e.target.value })}
          className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline"
        >
          <option value="">{catalog?.default_effort ? `默认 (${catalog.default_effort})` : "CLI 默认"}</option>
          {efforts.map((ef) => (
            <option key={ef} value={ef}>
              {ef === "low" && recommendLowEffort ? "low（推荐 · 秒回不卡顿）" : ef}
            </option>
          ))}
        </select>
      </label>
    </div>
  );
}

import { useEffect, useState } from "react";
import { api } from "../../lib/api";
import { getSupportedEfforts } from "../../lib/models";
import type { ModelCatalog } from "../../types/api";

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
    setCatalog(null);
    const qs = new URLSearchParams({ kind });
    if (machine) qs.set("machine", machine);
    void api<ModelCatalog>(`/api/models?${qs.toString()}`)
      .then(setCatalog)
      .catch(() => setCatalog(null));
  }, [kind, machine]);

  const efforts = getSupportedEfforts(catalog, model);
  const hideEffort = Boolean(catalog && efforts.length === 0);

  useEffect(() => {
    if (!catalog) return;
    if (effort && !efforts.includes(effort)) {
      const nextEffort = recommendLowEffort && efforts.includes("low") ? "low" : "";
      onChange({ kind, model, effort: nextEffort });
    }
  }, [catalog, kind, model, effort, efforts.join(","), recommendLowEffort]);

  const isInstalled = (k: string) => !allowedKinds || allowedKinds.length === 0 || allowedKinds.includes(k);
  const models = catalog?.models ?? [];
  const hasSavedModel = !model || models.some((m) => m.id === model);

  return (
    <div className={`grid grid-cols-1 ${hideEffort ? "sm:grid-cols-2" : "sm:grid-cols-3"} gap-3`}>
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
          onChange={(e) => {
            const nextModel = e.target.value;
            const nextEfforts = getSupportedEfforts(catalog, nextModel);
            const nextEffort = effort && nextEfforts.includes(effort)
              ? effort
              : recommendLowEffort && nextEfforts.includes("low")
                ? "low"
                : "";
            onChange({ kind, model: nextModel, effort: nextEffort });
          }}
          className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline"
        >
          <option value="">{catalog?.default ? `默认 (${catalog.default})` : "默认模型"}</option>
          {!hasSavedModel && <option value={model}>{model}</option>}
          {models.map((m) => (
            <option key={m.id} value={m.id}>
              {m.label || m.id}{m.note ? ` · ${m.note}` : ""}
            </option>
          ))}
        </select>
      </label>

      {!hideEffort && (
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
      )}
    </div>
  );
}

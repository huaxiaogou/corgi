import { useEffect, useMemo, useState, type Dispatch, type SetStateAction } from "react";
import { ChevronDown, ChevronRight, FolderOpen, Plus, Trash2 } from "lucide-react";
import { DirPicker } from "../../components/DirPicker";
import { api } from "../../lib/api";
import { normalizeKind, normalizeMachineKey } from "../../lib/dispatchDefaults";
import { getSupportedEfforts } from "../../lib/models";
import type { Machine, ModelCatalog, ModelEffortSpec, Settings } from "../../types/api";

const ALL_KINDS = [
  { id: "agy", label: "Antigravity" },
  { id: "codebuddy", label: "CodeBuddy" },
  { id: "codex", label: "Codex" },
  { id: "claude", label: "Claude Code" },
] as const;

function KindModelRow({
  kind,
  label,
  machine,
  spec,
  onChange,
}: {
  kind: string;
  label: string;
  machine: string;
  spec: ModelEffortSpec;
  onChange: (next: ModelEffortSpec) => void;
}) {
  const [catalog, setCatalog] = useState<ModelCatalog | null>(null);

  useEffect(() => {
    let cancelled = false;
    const qs = new URLSearchParams({ kind });
    if (machine) qs.set("machine", machine);
    void api<ModelCatalog>(`/api/models?${qs.toString()}`)
      .then((c) => {
        if (!cancelled) setCatalog(c);
      })
      .catch(() => {
        if (!cancelled) setCatalog(null);
      });
    return () => {
      cancelled = true;
    };
  }, [kind, machine]);

  const model = (spec.model ?? "").trim();
  const effort = (spec.effort ?? "").trim();
  const models = catalog?.models ?? [];
  const hasSavedModel = !model || models.some((m) => m.id === model);
  const efforts = getSupportedEfforts(catalog, model);
  const hideEffort = Boolean(catalog && efforts.length === 0);

  return (
    <div className="grid grid-cols-1 sm:grid-cols-[136px_1fr_120px_88px] gap-2 items-center py-2 border-b border-hairline last:border-b-0">
      <div className="min-w-0">
        <span className="font-medium text-primary">{label}</span>{" "}
        <span className="font-mono-code text-[11px] text-muted">{kind}</span>
      </div>
      <select
        value={model}
        onChange={(e) => {
          const nextModel = e.target.value;
          const nextEfforts = getSupportedEfforts(catalog, nextModel);
          const nextEffort = effort && nextEfforts.includes(effort) ? effort : "";
          onChange({ model: nextModel, effort: nextEffort });
        }}
        className="w-full px-2.5 py-1.5 rounded-lg surface-rail border border-hairline outline-none"
      >
        <option value="">{catalog?.default ? `跟随本机默认 (${catalog.default})` : "跟随本机 CLI 默认"}</option>
        {!hasSavedModel && <option value={model}>{model}</option>}
        {models.map((m) => (
          <option key={m.id} value={m.id}>
            {m.label || m.id}
            {m.note ? ` · ${m.note}` : ""}
          </option>
        ))}
      </select>
      {hideEffort ? (
        <span className="text-[11px] text-muted px-2">无独立档位</span>
      ) : (
        <select
          value={effort}
          onChange={(e) => onChange({ model, effort: e.target.value })}
          className="w-full px-2.5 py-1.5 rounded-lg surface-rail border border-hairline outline-none"
        >
          <option value="">{catalog?.default_effort ? `默认 (${catalog.default_effort})` : "CLI 默认"}</option>
          {efforts.map((ef) => (
            <option key={ef} value={ef}>
              {ef}
            </option>
          ))}
        </select>
      )}
      <div className="sm:text-right">
        <span
          className={`inline-block px-2 py-0.5 rounded-full text-[10.5px] border ${
            model || effort ? "border-hairline surface-bg text-primary font-medium" : "border-hairline surface-rail text-muted"
          }`}
        >
          {model || effort ? "已锁定" : "跟随本机"}
        </span>
      </div>
    </div>
  );
}

interface Props {
  draft: Settings;
  setDraft: Dispatch<SetStateAction<Settings>>;
  kindDefaults: Record<string, ModelEffortSpec>;
  machines: Machine[];
}

export function WorkerDispatchSection({ draft, setDraft, kindDefaults, machines }: Props) {
  const [dirOpen, setDirOpen] = useState(false);
  const [overridesOpen, setOverridesOpen] = useState(false);

  const workerMachineObj = machines.find((m) => m.id === (draft.worker_machine ?? "") || (!draft.worker_machine && m.local));

  const catalogMachineForKind = useMemo(() => {
    const out: Record<string, string> = {};
    for (const k of ALL_KINDS) {
      if (workerMachineObj?.agents?.includes(k.id)) {
        out[k.id] = workerMachineObj.local ? "" : workerMachineObj.id;
        continue;
      }
      const match = machines.find((m) => m.online && m.agents?.includes(k.id));
      out[k.id] = match ? (match.local ? "" : match.id) : "";
    }
    return out;
  }, [machines, workerMachineObj]);

  const updateKindDefault = (kind: string, spec: ModelEffortSpec) => {
    setDraft((prev) => {
      const nextMap: Record<string, ModelEffortSpec> = { ...(prev.kind_defaults ?? {}) };
      if ((spec.model ?? "").trim() || (spec.effort ?? "").trim()) {
        nextMap[kind] = { model: (spec.model ?? "").trim(), effort: (spec.effort ?? "").trim() };
      } else {
        delete nextMap[kind];
      }
      const next: Settings = { ...prev, kind_defaults: nextMap };
      if (normalizeKind(prev.worker_kind) === kind) {
        next.worker_model = (spec.model ?? "").trim();
        next.worker_effort = (spec.effort ?? "").trim();
      }
      return next;
    });
  };

  const overrideEntries = useMemo(() => {
    const rows: { machineKey: string; kind: string; spec: ModelEffortSpec }[] = [];
    for (const [mk, byKind] of Object.entries(draft.machine_kind_overrides ?? {})) {
      for (const [k, spec] of Object.entries(byKind ?? {})) {
        rows.push({ machineKey: mk, kind: k, spec });
      }
    }
    return rows;
  }, [draft.machine_kind_overrides]);

  const addMachineOverride = () => {
    setOverridesOpen(true);
    setDraft((prev) => {
      const next = { ...(prev.machine_kind_overrides ?? {}) };
      const mk = "hub";
      const existing = { ...(next[mk] ?? {}) };
      const freeKind = ALL_KINDS.map((k) => k.id).find((k) => !existing[k]) ?? "codebuddy";
      existing[freeKind] = existing[freeKind] ?? { model: "", effort: "" };
      next[mk] = existing;
      return { ...prev, machine_kind_overrides: next };
    });
  };

  const updateMachineOverride = (oldMk: string, oldK: string, nextMk: string, nextK: string, spec: ModelEffortSpec) => {
    setDraft((prev) => {
      const next: Record<string, Record<string, ModelEffortSpec>> = {};
      for (const [mk, byKind] of Object.entries(prev.machine_kind_overrides ?? {})) {
        next[mk] = { ...byKind };
      }
      if (next[oldMk]) {
        delete next[oldMk][oldK];
        if (Object.keys(next[oldMk]).length === 0) delete next[oldMk];
      }
      const targetMk = normalizeMachineKey(nextMk);
      next[targetMk] = { ...(next[targetMk] ?? {}), [nextK]: spec };
      return { ...prev, machine_kind_overrides: next };
    });
  };

  const removeMachineOverride = (machineKey: string, kind: string) => {
    setDraft((prev) => {
      const next: Record<string, Record<string, ModelEffortSpec>> = {};
      for (const [mk, byKind] of Object.entries(prev.machine_kind_overrides ?? {})) {
        next[mk] = { ...byKind };
      }
      if (next[machineKey]) {
        delete next[machineKey][kind];
        if (Object.keys(next[machineKey]).length === 0) delete next[machineKey];
      }
      return { ...prev, machine_kind_overrides: next };
    });
  };

  return (
    <section className="p-4 rounded-xl border border-hairline space-y-4">
      <div>
        <div className="font-semibold text-sm">派活全局默认 & 各工具默认模型</div>
        <div className="text-[11.5px] text-muted mt-0.5 leading-relaxed">
          这里是唯一的全局默认配置（输入框左下角会直接显示当前生效值；输入框里修改只影响单条消息）。跨工具派活时自动命中对应工具的模型，不再静默掉回本机旧配置。
        </div>
      </div>

      <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
        <label className="space-y-1">
          <span className="text-muted">默认运行电脑</span>
          <select
            value={draft.worker_machine ?? ""}
            onChange={(e) => {
              const nextMachine = e.target.value;
              const curCwd = (draft.worker_cwd ?? "").trim();
              const resetCwd = nextMachine !== (draft.worker_machine ?? "") && curCwd.startsWith("/") ? "~" : draft.worker_cwd;
              setDraft((prev) => ({ ...prev, worker_machine: nextMachine, worker_cwd: resetCwd }));
            }}
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
        <label className="space-y-1">
          <span className="text-muted">默认派活工具</span>
          <select
            value={normalizeKind(draft.worker_kind) || "claude"}
            onChange={(e) => {
              const nextKind = e.target.value;
              const spec = kindDefaults[nextKind];
              setDraft((prev) => ({
                ...prev,
                worker_kind: nextKind,
                worker_model: spec?.model ?? "",
                worker_effort: spec?.effort ?? "",
              }));
            }}
            className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline"
          >
            {ALL_KINDS.map((k) => {
              const installed = !workerMachineObj?.agents?.length || workerMachineObj.agents.includes(k.id);
              return (
                <option key={k.id} value={k.id}>
                  {k.label} ({k.id})
                  {installed ? "" : "（默认电脑未安装）"}
                </option>
              );
            })}
          </select>
        </label>
      </div>

      <div className="space-y-1">
        <div className="text-[11.5px] font-medium text-secondary">各工具默认模型（跨工具派活自动生效）</div>
        <div className="px-3 py-1 rounded-xl border border-hairline surface-bg">
          {ALL_KINDS.map((k) => (
            <KindModelRow
              key={k.id}
              kind={k.id}
              label={k.label}
              machine={catalogMachineForKind[k.id] ?? ""}
              spec={kindDefaults[k.id] ?? {}}
              onChange={(spec) => updateKindDefault(k.id, spec)}
            />
          ))}
        </div>
      </div>

      <div className="space-y-2 pt-1 border-t border-hairline">
        <div className="flex items-center justify-between gap-2">
          <button
            type="button"
            onClick={() => setOverridesOpen((v) => !v)}
            className="inline-flex items-center gap-1 text-[11.5px] text-secondary hover:text-primary cursor-pointer"
          >
            {overridesOpen ? <ChevronDown className="w-3.5 h-3.5" /> : <ChevronRight className="w-3.5 h-3.5" />}
            <span>按电脑单独覆盖（可选{overrideEntries.length > 0 ? `，已配 ${overrideEntries.length} 条` : ""}）</span>
          </button>
          <button
            type="button"
            onClick={addMachineOverride}
            className="inline-flex items-center gap-1 px-2 py-1 rounded-md border border-hairline surface-rail surface-hover text-[11px] text-secondary cursor-pointer"
          >
            <Plus className="w-3 h-3" />
            <span>新增覆盖</span>
          </button>
        </div>
        {overridesOpen && (
          <div className="space-y-2">
            {overrideEntries.length === 0 ? (
              <div className="text-[11px] text-muted px-1">暂无电脑级覆盖；所有电脑均使用上方各工具默认模型。</div>
            ) : (
              overrideEntries.map((row) => (
                <div
                  key={`${row.machineKey}:${row.kind}`}
                  className="grid grid-cols-1 sm:grid-cols-[150px_110px_1fr_92px_32px] gap-2 items-center p-2 rounded-lg border border-hairline surface-rail"
                >
                  <select
                    value={row.machineKey}
                    onChange={(e) => updateMachineOverride(row.machineKey, row.kind, e.target.value, row.kind, row.spec)}
                    className="px-2 py-1.5 rounded-md surface-bg border border-hairline"
                  >
                    <option value="hub">Hub 本机</option>
                    {machines
                      .filter((m) => !m.local)
                      .map((m) => (
                        <option key={m.id} value={m.id}>
                          {m.name}
                        </option>
                      ))}
                  </select>
                  <select
                    value={row.kind}
                    onChange={(e) => updateMachineOverride(row.machineKey, row.kind, row.machineKey, e.target.value, row.spec)}
                    className="px-2 py-1.5 rounded-md surface-bg border border-hairline"
                  >
                    {ALL_KINDS.map((k) => (
                      <option key={k.id} value={k.id}>
                        {k.id}
                      </option>
                    ))}
                  </select>
                  <input
                    value={row.spec.model ?? ""}
                    onChange={(e) =>
                      updateMachineOverride(row.machineKey, row.kind, row.machineKey, row.kind, { ...row.spec, model: e.target.value })
                    }
                    placeholder="模型 ID（例如 claude-opus-5.5）"
                    className="px-2.5 py-1.5 rounded-md surface-bg border border-hairline font-mono-code outline-none"
                  />
                  <select
                    value={row.spec.effort ?? ""}
                    onChange={(e) =>
                      updateMachineOverride(row.machineKey, row.kind, row.machineKey, row.kind, { ...row.spec, effort: e.target.value })
                    }
                    className="px-2 py-1.5 rounded-md surface-bg border border-hairline"
                  >
                    <option value="">默认</option>
                    {["low", "medium", "high", "xhigh", "max"].map((ef) => (
                      <option key={ef} value={ef}>
                        {ef}
                      </option>
                    ))}
                  </select>
                  <button
                    type="button"
                    onClick={() => removeMachineOverride(row.machineKey, row.kind)}
                    className="w-8 h-8 rounded-md flex items-center justify-center text-muted hover:text-primary surface-hover cursor-pointer"
                    title="删除覆盖"
                  >
                    <Trash2 className="w-3.5 h-3.5" />
                  </button>
                </div>
              ))
            )}
          </div>
        )}
      </div>

      <div className="space-y-1">
        <span className="text-muted">未命中项目时的兜底工作目录</span>
        <div className="flex gap-2">
          <input
            value={draft.worker_cwd ?? ""}
            onChange={(e) => setDraft((prev) => ({ ...prev, worker_cwd: e.target.value }))}
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
      {dirOpen && (
        <DirPicker
          value={draft.worker_cwd || "~"}
          machine={draft.worker_machine ?? ""}
          onChange={(p) => setDraft((prev) => ({ ...prev, worker_cwd: p }))}
        />
      )}
    </section>
  );
}

import { useEffect, useMemo, useState } from "react";
import { ChevronDown, ChevronRight, FolderOpen, Plus, Trash2 } from "lucide-react";
import { DirPicker } from "../../components/DirPicker";
import { api } from "../../lib/api";
import { normalizeKind, normalizeMachineKey } from "../../lib/dispatchDefaults";
import { getSupportedEfforts } from "../../lib/models";
import {
  TOKENHUB_BASE_URL,
  TOKENHUB_MODELS,
  getRouterAPIEffortOptions,
  resolveRouterAPIEffort,
} from "../../lib/routerApiModels";
import type { Machine, ModelCatalog, ModelEffortSpec, RouterAPISecretStatus, Settings } from "../../types/api";

const ALL_KINDS = [
  { id: "agy", label: "Antigravity" },
  { id: "codebuddy", label: "CodeBuddy" },
  { id: "codex", label: "Codex" },
  { id: "claude", label: "Claude Code" },
] as const;

interface Props {
  settings: Settings;
  routerAPISecret?: RouterAPISecretStatus;
  machines?: Machine[];
  onSavePatch: (patch: Partial<Settings>, okMsg: string) => Promise<void>;
  onUpdateAPISecret?: (next: RouterAPISecretStatus) => void;
  onToast?: (msg: string) => void;
}

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
            model || effort
              ? "border-hairline surface-bg text-primary font-medium"
              : "border-hairline surface-rail text-muted"
          }`}
        >
          {model || effort ? "已锁定" : "跟随本机"}
        </span>
      </div>
    </div>
  );
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
  const [dirOpen, setDirOpen] = useState(false);
  const [overridesOpen, setOverridesOpen] = useState(false);
  // API key stays strictly local to this input and is never merged into Settings state.
  const [apiKeyInput, setApiKeyInput] = useState("");
  const [savingKey, setSavingKey] = useState(false);
  const [testingAPI, setTestingAPI] = useState(false);
  const [testResult, setTestResult] = useState<{
    ok: boolean;
    message: string;
  } | null>(null);

  useEffect(() => {
    setDraft(settings);
  }, [settings]);

  const apiProvider = draft.router_api_provider === "openai" ? "openai" : "tokenhub";
  const effectiveAPIBaseURL = apiProvider === "tokenhub" ? TOKENHUB_BASE_URL : draft.router_api_base_url ?? "";
  const effectiveAPIModel = (draft.router_api_model ?? "").trim() || (apiProvider === "tokenhub" ? "hy4" : "");
  const apiEffortOptions = useMemo(
    () => getRouterAPIEffortOptions(apiProvider, effectiveAPIModel),
    [apiProvider, effectiveAPIModel]
  );
  const effectiveAPIEffort = resolveRouterAPIEffort(apiProvider, effectiveAPIModel, draft.router_api_effort);

  const draftAPIOrigin = useMemo(() => {
    try {
      const u = new URL(effectiveAPIBaseURL);
      return `${u.protocol}//${u.host}`.toLowerCase();
    } catch {
      return "";
    }
  }, [effectiveAPIBaseURL]);
  const draftOriginMatch =
    Boolean(routerAPISecret?.has_router_api_key) &&
    Boolean(draftAPIOrigin) &&
    routerAPISecret?.router_api_key_bound_origin === draftAPIOrigin;

  const saveAPIKey = async () => {
    const trimmed = apiKeyInput.trim();
    if (!trimmed) {
      onToast?.("请先输入 API Key");
      return;
    }
    setSavingKey(true);
    try {
      const r = await api<RouterAPISecretStatus>("/api/super/router/api-key", {
        provider: apiProvider,
        base_url: effectiveAPIBaseURL,
        api_key: trimmed,
      });
      setApiKeyInput("");
      setTestResult(null);
      // Update only secret status so unsaved settings draft (mode/URL/model) is never overwritten.
      onUpdateAPISecret?.({
        has_router_api_key: Boolean(r.has_router_api_key),
        router_api_key_masked: r.router_api_key_masked ?? "",
        router_api_key_bound_origin: r.router_api_key_bound_origin ?? "",
        router_api_key_origin_match: Boolean(r.router_api_key_origin_match),
      });
      onToast?.("API Key 已安全保存");
    } catch (err) {
      onToast?.(err instanceof Error ? err.message : "保存 API Key 失败");
    } finally {
      setSavingKey(false);
    }
  };

  const clearAPIKey = async () => {
    setSavingKey(true);
    try {
      await api("/api/super/router/api-key", { clear: true });
      setApiKeyInput("");
      setTestResult(null);
      onUpdateAPISecret?.({ has_router_api_key: false });
      onToast?.("已清除保存的 API Key");
    } catch (err) {
      onToast?.(err instanceof Error ? err.message : "清除 API Key 失败");
    } finally {
      setSavingKey(false);
    }
  };

  const testRouterAPI = async () => {
    setTestingAPI(true);
    setTestResult(null);
    try {
      const r = await api<{ ok: boolean; latency_ms?: number; request_id?: string; model?: string; error?: string }>(
        "/api/super/router/test-api",
        {
          provider: apiProvider,
          base_url: effectiveAPIBaseURL,
          api_key: apiKeyInput.trim(),
          model: effectiveAPIModel,
          effort: effectiveAPIEffort,
        }
      );
      const parts = [r.model || effectiveAPIModel];
      if (typeof r.latency_ms === "number") parts.push(`${r.latency_ms}ms`);
      if (r.request_id) parts.push(`req=${r.request_id}`);
      setTestResult({ ok: true, message: `连通成功 · ${parts.join(" · ")}` });
    } catch (err) {
      setTestResult({ ok: false, message: err instanceof Error ? err.message : "连通测试失败" });
    } finally {
      setTestingAPI(false);
    }
  };

  const workerMachineObj = machines.find(
    (m) => m.id === (draft.worker_machine ?? "") || (!draft.worker_machine && m.local)
  );

  // Pick a machine that has each kind installed so model catalog queries succeed accurately.
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

  const kindDefaults: Record<string, ModelEffortSpec> = useMemo(() => {
    const base: Record<string, ModelEffortSpec> = { ...(draft.kind_defaults ?? {}) };
    const wk = normalizeKind(draft.worker_kind);
    if (wk && !base[wk] && (draft.worker_model || draft.worker_effort)) {
      base[wk] = { model: draft.worker_model ?? "", effort: draft.worker_effort ?? "" };
    }
    return base;
  }, [draft.kind_defaults, draft.worker_kind, draft.worker_model, draft.worker_effort]);

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

  const updateMachineOverride = (
    oldMachineKey: string,
    oldKind: string,
    nextMachineKey: string,
    nextKind: string,
    spec: ModelEffortSpec
  ) => {
    setDraft((prev) => {
      const next: Record<string, Record<string, ModelEffortSpec>> = {};
      for (const [mk, byKind] of Object.entries(prev.machine_kind_overrides ?? {})) {
        next[mk] = { ...byKind };
      }
      if (next[oldMachineKey]) {
        delete next[oldMachineKey][oldKind];
        if (Object.keys(next[oldMachineKey]).length === 0) delete next[oldMachineKey];
      }
      const targetMk = normalizeMachineKey(nextMachineKey);
      next[targetMk] = { ...(next[targetMk] ?? {}), [nextKind]: spec };
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

  const save = () => {
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
      <section className="p-4 rounded-xl border border-hairline space-y-3">
        <div>
          <div className="font-semibold text-sm">Corgi 原生大脑（运行在 Hub 进程内）</div>
          <div className="text-[11.5px] text-muted mt-0.5">
            负责理解意图、按需查证、编排多机 Worker 和汇报结论，直连上游模型 API（低延迟、无上下文交接）。
          </div>
        </div>

        <div className="space-y-3 pt-1">
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
              <label className="space-y-1">
                <span className="text-muted">模型服务商</span>
                <select
                  value={apiProvider}
                  onChange={(e) => {
                    const nextProv = e.target.value === "openai" ? "openai" : "tokenhub";
                    setTestResult(null);
                    setDraft((prev) => {
                      const nextModel = nextProv === "tokenhub" ? prev.router_api_model || "hy4" : prev.router_api_model || "";
                      return {
                        ...prev,
                        router_api_provider: nextProv,
                        router_api_base_url:
                          nextProv === "tokenhub" ? TOKENHUB_BASE_URL : prev.router_api_base_url === TOKENHUB_BASE_URL ? "" : prev.router_api_base_url,
                        router_api_model: nextModel,
                        router_api_effort: resolveRouterAPIEffort(nextProv, nextModel, prev.router_api_effort),
                      };
                    });
                  }}
                  className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline"
                >
                  <option value="tokenhub">腾讯 TokenHub</option>
                  <option value="openai">自定义 OpenAI 兼容（HTTPS）</option>
                </select>
              </label>

              <label className="space-y-1">
                <span className="text-muted">Base URL</span>
                <input
                  type="url"
                  value={effectiveAPIBaseURL}
                  readOnly={apiProvider === "tokenhub"}
                  placeholder="https://api.example.com/v1"
                  onChange={(e) => {
                    setTestResult(null);
                    setDraft((prev) => ({ ...prev, router_api_base_url: e.target.value }));
                  }}
                  className={`w-full px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code text-[11.5px] ${
                    apiProvider === "tokenhub" ? "text-muted" : ""
                  }`}
                />
              </label>
            </div>

            <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
              <label className="space-y-1">
                <span className="text-muted">大脑模型</span>
                {apiProvider === "tokenhub" ? (
                  <div className="flex gap-2">
                    <select
                      value={TOKENHUB_MODELS.some((m) => m.id === effectiveAPIModel) ? effectiveAPIModel : "__custom__"}
                      onChange={(e) => {
                        const v = e.target.value;
                        if (v === "__custom__") return;
                        setTestResult(null);
                        setDraft((prev) => ({
                          ...prev,
                          router_api_model: v,
                          router_api_effort: resolveRouterAPIEffort("tokenhub", v, prev.router_api_effort),
                        }));
                      }}
                      className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline"
                    >
                      {TOKENHUB_MODELS.map((m) => (
                        <option key={m.id} value={m.id}>
                          {m.label}
                        </option>
                      ))}
                      {!TOKENHUB_MODELS.some((m) => m.id === effectiveAPIModel) && (
                        <option value="__custom__">{effectiveAPIModel}</option>
                      )}
                    </select>
                  </div>
                ) : (
                  <input
                    type="text"
                    value={draft.router_api_model ?? ""}
                    placeholder="如 hy4 / gpt-4o / deepseek-chat"
                    onChange={(e) => setDraft((prev) => ({ ...prev, router_api_model: e.target.value }))}
                    className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code"
                  />
                )}
              </label>

              <label className="space-y-1">
                <span className="text-muted">推理强度（reasoning_effort）</span>
                <select
                  value={effectiveAPIEffort}
                  onChange={(e) => {
                    setTestResult(null);
                    setDraft((prev) => ({ ...prev, router_api_effort: e.target.value }));
                  }}
                  className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline"
                >
                  {apiEffortOptions.map((opt) => (
                    <option key={opt.id || "default"} value={opt.id}>
                      {opt.label}
                    </option>
                  ))}
                  {!apiEffortOptions.some((o) => o.id === effectiveAPIEffort) && effectiveAPIEffort && (
                    <option value={effectiveAPIEffort}>{effectiveAPIEffort}</option>
                  )}
                </select>
              </label>
            </div>

            <div className="p-3 rounded-lg surface-rail border border-hairline space-y-2">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <span className="font-medium text-secondary">API Key（独立安全存储，不随普通设置回传）</span>
                <span className="text-[11px] font-mono-code text-muted">
                  {routerAPISecret?.has_router_api_key
                    ? !draftOriginMatch
                      ? `已存 Key 绑定于 ${routerAPISecret.router_api_key_bound_origin}（切换域名后需重填）`
                      : `已配置 · ${routerAPISecret.router_api_key_masked || "••••••••"}`
                    : "尚未配置"}
                </span>
              </div>
              <div className="flex flex-wrap items-center gap-2">
                <input
                  type="password"
                  autoComplete="off"
                  value={apiKeyInput}
                  onChange={(e) => setApiKeyInput(e.target.value)}
                  placeholder={routerAPISecret?.has_router_api_key ? "输入新 Key 以替换，留空保持当前 Key" : "输入 TokenHub / OpenAI API Key"}
                  className="flex-1 min-w-[200px] px-3 py-1.5 rounded-lg surface-bg border border-hairline font-mono-code text-[11.5px]"
                />
                <button
                  type="button"
                  disabled={savingKey || !apiKeyInput.trim()}
                  onClick={() => void saveAPIKey()}
                  className="px-3 py-1.5 rounded-lg border border-strong surface-bg text-primary surface-hover disabled:opacity-40 cursor-pointer"
                >
                  {savingKey ? "保存中…" : "保存 Key"}
                </button>
                <button
                  type="button"
                  disabled={testingAPI}
                  onClick={() => void testRouterAPI()}
                  className="px-3 py-1.5 rounded-lg border border-hairline surface-bg text-secondary surface-hover disabled:opacity-40 cursor-pointer"
                >
                  {testingAPI ? "测试中…" : "测试连通"}
                </button>
                {routerAPISecret?.has_router_api_key && (
                  <button
                    type="button"
                    disabled={savingKey}
                    onClick={() => void clearAPIKey()}
                    className="px-2.5 py-1.5 rounded-lg border border-hairline text-muted hover:text-primary cursor-pointer"
                  >
                    清除
                  </button>
                )}
              </div>
              {testResult && (
                <div
                  className="text-[11.5px] font-mono-code"
                  style={{ color: testResult.ok ? "var(--ok)" : "var(--err)" }}
                >
                  {testResult.ok ? "✓ " : "✕ "}
                  {testResult.message}
                </div>
              )}
            </div>
        </div>
      </section>

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
                const resetCwd =
                  nextMachine !== (draft.worker_machine ?? "") && curCwd.startsWith("/") ? "~" : draft.worker_cwd;
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
                    {k.label} ({k.id}){installed ? "" : "（默认电脑未安装）"}
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
              <span>
                按电脑单独覆盖（可选{overrideEntries.length > 0 ? `，已配 ${overrideEntries.length} 条` : ""}）
              </span>
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
                      onChange={(e) =>
                        updateMachineOverride(row.machineKey, row.kind, e.target.value, row.kind, row.spec)
                      }
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
                      onChange={(e) =>
                        updateMachineOverride(row.machineKey, row.kind, row.machineKey, e.target.value, row.spec)
                      }
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
                        updateMachineOverride(row.machineKey, row.kind, row.machineKey, row.kind, {
                          ...row.spec,
                          model: e.target.value,
                        })
                      }
                      placeholder="模型 ID（例如 claude-opus-5.5）"
                      className="px-2.5 py-1.5 rounded-md surface-bg border border-hairline font-mono-code outline-none"
                    />
                    <select
                      value={row.spec.effort ?? ""}
                      onChange={(e) =>
                        updateMachineOverride(row.machineKey, row.kind, row.machineKey, row.kind, {
                          ...row.spec,
                          effort: e.target.value,
                        })
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

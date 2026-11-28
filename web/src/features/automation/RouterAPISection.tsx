import { useMemo, useState, type Dispatch, type SetStateAction } from "react";
import { api } from "../../lib/api";
import {
  TOKENHUB_BASE_URL,
  TOKENHUB_MODELS,
  getRouterAPIEffortOptions,
  resolveRouterAPIEffort,
} from "../../lib/routerApiModels";
import type { RouterAPISecretStatus, Settings } from "../../types/api";

interface Props {
  draft: Settings;
  setDraft: Dispatch<SetStateAction<Settings>>;
  routerAPISecret?: RouterAPISecretStatus;
  onUpdateAPISecret?: (next: RouterAPISecretStatus) => void;
  onToast?: (msg: string) => void;
}

export function RouterAPISection({ draft, setDraft, routerAPISecret, onUpdateAPISecret, onToast }: Props) {
  // API key stays strictly local to this input and is never merged into Settings state.
  const [apiKeyInput, setApiKeyInput] = useState("");
  const [savingKey, setSavingKey] = useState(false);
  const [testingAPI, setTestingAPI] = useState(false);
  const [testResult, setTestResult] = useState<{ ok: boolean; message: string } | null>(null);

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

  return (
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

        <label className="block space-y-1">
          <span className="text-muted">工作上下文换代阈值（达到后自动后台压缩，保留最近轮次原文）</span>
          <select
            value={draft.router_context_budget_tokens ?? 80000}
            onChange={(e) =>
              setDraft((prev) => ({
                ...prev,
                router_context_budget_tokens: Number(e.target.value) || 80000,
              }))
            }
            className="w-full sm:w-1/2 px-3 py-2 rounded-lg surface-rail border border-hairline"
          >
            <option value={32000}>32K tokens（轻快省流）</option>
            <option value={80000}>80K tokens（默认均衡）</option>
            <option value={160000}>160K tokens（超长连续）</option>
            <option value={300000}>300K tokens（极限长会话）</option>
            {![32000, 80000, 160000, 300000].includes(draft.router_context_budget_tokens ?? 80000) && (
              <option value={draft.router_context_budget_tokens}>
                {Math.round((draft.router_context_budget_tokens ?? 80000) / 1000)}K tokens（自定义）
              </option>
            )}
          </select>
        </label>

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
            <div className="text-[11.5px] font-mono-code" style={{ color: testResult.ok ? "var(--ok)" : "var(--err)" }}>
              {testResult.ok ? "✓ " : "✕ "}
              {testResult.message}
            </div>
          )}
        </div>
      </div>
    </section>
  );
}

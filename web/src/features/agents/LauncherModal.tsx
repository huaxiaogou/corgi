import { useEffect, useState, type FormEvent } from "react";
import { RefreshCw, X } from "lucide-react";
import { DirPicker, rememberRecentDir } from "../../components/DirPicker";
import { api } from "../../lib/api";
import type { Machine, ModelCatalog } from "../../types/api";

interface Props {
  open: boolean;
  machines: Machine[];
  onClose: () => void;
  onLaunched: (paneId: string) => void;
  onToast: (msg: string) => void;
}

const KINDS = ["claude", "codex", "agy", "codebuddy", "shell"] as const;

export function LauncherModal({ open, machines, onClose, onLaunched, onToast }: Props) {
  const [machine, setMachine] = useState("");
  const [kind, setKind] = useState("claude");
  const [modelName, setModelName] = useState("");
  const [effort, setEffort] = useState("");
  const [cwd, setCwd] = useState("~");
  const [name, setName] = useState("");
  const [prompt, setPrompt] = useState("");
  const [catalog, setCatalog] = useState<ModelCatalog | null>(null);
  const [loadingModels, setLoadingModels] = useState(false);
  const [busy, setBusy] = useState(false);

  const curMachine = machines.find((m) => m.id === machine) ?? machines[0];
  const installed = curMachine?.agents ?? [];

  // Auto-select an installed agent on the chosen machine when opening or switching computers.
  useEffect(() => {
    if (!open || kind === "shell" || installed.length === 0) return;
    if (!installed.includes(kind)) {
      setKind(installed[0]);
      setModelName("");
      setEffort("");
    }
  }, [open, machine, installed.join(",")]);

  const fetchModels = (refresh = false) => {
    if (!open || kind === "shell") {
      setCatalog(null);
      return;
    }
    setLoadingModels(true);
    const qs = new URLSearchParams({ kind });
    if (machine) qs.set("machine", machine);
    if (refresh) qs.set("refresh", "1");
    void api<ModelCatalog>(`/api/models?${qs.toString()}`)
      .then(setCatalog)
      .catch(() => setCatalog(null))
      .finally(() => setLoadingModels(false));
  };

  useEffect(() => {
    fetchModels(false);
  }, [open, kind, machine]);

  if (!open) return null;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    try {
      const res = await api<{ pane_id: string }>("/api/agents", {
        kind,
        machine,
        model: modelName,
        effort,
        cwd,
        name,
        prompt,
      });
      rememberRecentDir(machine, cwd);
      onClose();
      if (res.pane_id) onLaunched(res.pane_id);
    } catch (err) {
      onToast(err instanceof Error ? err.message : "启动失败");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4" style={{ background: "rgba(0,0,0,0.35)" }}>
      <form onSubmit={(e) => void submit(e)} className="w-full max-w-[480px] max-h-[90vh] flex flex-col card-floating rounded-2xl overflow-hidden">
        <div className="px-5 py-3.5 flex items-center justify-between border-b border-hairline shrink-0">
          <h3 className="text-sm font-semibold">手动新建</h3>
          <button type="button" onClick={onClose} className="text-muted cursor-pointer">
            <X className="w-4 h-4" />
          </button>
        </div>

        <div className="p-5 space-y-4 text-xs overflow-y-auto">
          <p className="text-muted">日常任务直接告诉 Corgi 拆活就行；这里用于亲自开一个独立 Agent 或纯终端。</p>

          {machines.length > 1 && (
            <div className="space-y-1.5">
              <label className="text-secondary block">在哪台电脑上</label>
              <div className="flex flex-wrap gap-2">
                {machines.map((m) => (
                  <button
                    key={m.id || "local"}
                    type="button"
                    onClick={() => {
                      setMachine(m.id);
                      setCwd("~");
                    }}
                    className={`px-3 py-1.5 rounded-lg border cursor-pointer ${
                      machine === m.id ? "border-strong surface-rail font-medium text-primary" : "border-hairline text-secondary"
                    }`}
                  >
                    {m.name}
                    {m.local ? " (hub)" : ""}
                  </button>
                ))}
              </div>
            </div>
          )}

          <div className="flex flex-wrap gap-1.5">
            {KINDS.map((k) => {
              const ready = k === "shell" || installed.length === 0 || installed.includes(k);
              return (
                <button
                  key={k}
                  type="button"
                  onClick={() => {
                    setKind(k);
                    setModelName("");
                    setEffort("");
                  }}
                  className={`py-2 px-3 rounded-xl cursor-pointer font-medium border ${
                    kind === k ? "border-strong surface-rail text-primary" : "border-hairline text-secondary"
                  } ${ready ? "" : "opacity-50"}`}
                >
                  {k === "shell" ? "终端" : k}
                  {!ready && <span className="ml-1 text-[10px] text-muted">未装</span>}
                </button>
              );
            })}
          </div>

          {kind !== "shell" && (
            <div className="flex items-center gap-2">
              <select
                value={modelName}
                onChange={(e) => setModelName(e.target.value)}
                className="flex-1 min-w-0 px-3 py-2 rounded-lg surface-rail border border-hairline outline-none"
              >
                <option value="">{loadingModels ? "读取可用模型中…" : catalog?.default ? `默认（${catalog.default}）` : "默认模型"}</option>
                {(catalog?.models ?? []).map((m) => (
                  <option key={m.id} value={m.id}>
                    {m.label || m.id}
                    {m.note ? ` · ${m.note}` : ""}
                  </option>
                ))}
              </select>
              <select
                value={effort}
                disabled={Boolean(catalog && catalog.efforts.length === 0)}
                onChange={(e) => setEffort(e.target.value)}
                className="w-32 shrink-0 px-3 py-2 rounded-lg surface-rail border border-hairline outline-none disabled:opacity-50"
              >
                <option value="">{catalog && catalog.efforts.length === 0 ? "无思考强度" : "默认思考"}</option>
                {(catalog?.efforts ?? ["low", "medium", "high"]).map((ef) => (
                  <option key={ef} value={ef}>
                    {ef}
                  </option>
                ))}
              </select>
              <button
                type="button"
                onClick={() => fetchModels(true)}
                title="刷新该电脑上的可用模型"
                className="p-2 rounded-lg border border-hairline surface-rail text-secondary surface-hover cursor-pointer shrink-0"
              >
                <RefreshCw className={`w-3.5 h-3.5 ${loadingModels ? "animate-spin" : ""}`} />
              </button>
            </div>
          )}

          <DirPicker machine={machine} value={cwd} onChange={setCwd} />

          {kind !== "shell" && (
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-2.5">
              <input
                type="text"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="名字（可选，小写字母开头）"
                className="w-full px-3 py-2 rounded-lg font-mono-code surface-rail border border-hairline outline-none"
              />
              <input
                type="text"
                value={prompt}
                onChange={(e) => setPrompt(e.target.value)}
                placeholder="第一个任务（可选）"
                className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline outline-none"
              />
            </div>
          )}
        </div>

        <div className="px-5 py-3 flex justify-end gap-2 border-t border-hairline shrink-0">
          <button type="button" onClick={onClose} className="px-3.5 py-1.5 rounded-lg text-xs border border-strong text-secondary cursor-pointer">
            取消
          </button>
          <button type="submit" disabled={busy} className="px-4 py-1.5 rounded-lg text-xs font-medium btn-accent cursor-pointer disabled:opacity-50">
            {busy ? "启动中…" : "启动"}
          </button>
        </div>
      </form>
    </div>
  );
}

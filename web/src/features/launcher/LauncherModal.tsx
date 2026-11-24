import { useEffect, useState, type FormEvent } from "react";
import { X } from "lucide-react";
import { api } from "../../lib/api";
import type { Machine, ModelCatalog } from "../../types/api";

interface Props {
  open: boolean;
  machines: Machine[];
  onClose: () => void;
  onLaunched: (paneId: string) => void;
  onToast: (msg: string) => void;
}

export function LauncherModal({ open, machines, onClose, onLaunched, onToast }: Props) {
  const [kind, setKind] = useState("claude");
  const [machine, setMachine] = useState("");
  const [modelName, setModelName] = useState("");
  const [effort, setEffort] = useState("");
  const [cwd, setCwd] = useState("~");
  const [name, setName] = useState("");
  const [prompt, setPrompt] = useState("");
  const [catalog, setCatalog] = useState<ModelCatalog | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!open || kind === "shell") {
      setCatalog(null);
      return;
    }
    const qs = new URLSearchParams({ kind });
    if (machine) qs.set("machine", machine);
    void api<ModelCatalog>(`/api/models?${qs.toString()}`)
      .then(setCatalog)
      .catch(() => setCatalog(null));
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
      <form onSubmit={(e) => void submit(e)} className="w-full max-w-[440px] card-floating rounded-2xl overflow-hidden">
        <div className="px-5 py-3.5 flex items-center justify-between border-b border-hairline">
          <h3 className="text-sm font-semibold">手动新建</h3>
          <button type="button" onClick={onClose} className="text-muted cursor-pointer">
            <X className="w-4 h-4" />
          </button>
        </div>

        <div className="p-5 space-y-4 text-xs">
          <p className="text-muted">日常任务直接告诉 Corgi 拆活就行；这里用于亲自开一个独立 Agent 或纯终端。</p>

          <div className="grid grid-cols-3 sm:grid-cols-5 gap-2">
            {(["claude", "codex", "agy", "codebuddy", "shell"] as const).map((k) => (
              <button
                key={k}
                type="button"
                onClick={() => {
                  setKind(k);
                  setModelName("");
                  setEffort("");
                }}
                className={`py-2.5 px-1.5 rounded-xl cursor-pointer font-medium border truncate ${
                  kind === k ? "border-strong surface-rail text-primary" : "border-hairline text-secondary"
                }`}
              >
                {k === "shell" ? "终端" : k}
              </button>
            ))}
          </div>

          {machines.length > 1 && (
            <div className="space-y-1.5">
              <label className="text-secondary block">在哪台电脑上</label>
              <div className="flex flex-wrap gap-2">
                {machines.map((m) => (
                  <button
                    key={m.id || "local"}
                    type="button"
                    onClick={() => setMachine(m.id)}
                    className={`px-3 py-1.5 rounded-lg border cursor-pointer ${
                      machine === m.id ? "border-strong surface-rail font-medium" : "border-hairline text-secondary"
                    }`}
                  >
                    {m.name}
                    {m.local ? " (hub)" : ""}
                  </button>
                ))}
              </div>
            </div>
          )}

          {kind !== "shell" && (
            <div className="grid grid-cols-2 gap-2">
              <select
                value={modelName}
                onChange={(e) => setModelName(e.target.value)}
                className="px-3 py-2 rounded-lg surface-rail border border-hairline outline-none"
              >
                <option value="">默认模型</option>
                {(catalog?.models ?? []).map((m) => (
                  <option key={m.id} value={m.id}>
                    {m.label || m.id}
                  </option>
                ))}
              </select>
              <select
                value={effort}
                disabled={Boolean(catalog && catalog.efforts.length === 0)}
                onChange={(e) => setEffort(e.target.value)}
                className="px-3 py-2 rounded-lg surface-rail border border-hairline outline-none disabled:opacity-50"
              >
                <option value="">{catalog && catalog.efforts.length === 0 ? "无思考强度选项" : "默认思考强度"}</option>
                {(catalog?.efforts ?? ["low", "medium", "high"]).map((ef) => (
                  <option key={ef} value={ef}>
                    {ef}
                  </option>
                ))}
              </select>
            </div>
          )}

          <div className="space-y-1.5">
            <label className="text-secondary block">目录</label>
            <input
              type="text"
              value={cwd}
              onChange={(e) => setCwd(e.target.value)}
              className="w-full px-3 py-2 rounded-lg font-mono-code surface-rail border border-hairline outline-none"
            />
          </div>

          {kind !== "shell" && (
            <>
              <div className="space-y-1.5">
                <label className="text-secondary block">名字（可选）</label>
                <input
                  type="text"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="小写字母开头，可含数字、- 和 _"
                  className="w-full px-3 py-2 rounded-lg font-mono-code surface-rail border border-hairline outline-none"
                />
              </div>
              <div className="space-y-1.5">
                <label className="text-secondary block">第一个任务（可选）</label>
                <textarea
                  rows={2}
                  value={prompt}
                  onChange={(e) => setPrompt(e.target.value)}
                  placeholder="例如：跑一遍测试，修掉失败用例"
                  className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline outline-none"
                />
              </div>
            </>
          )}
        </div>

        <div className="px-5 py-3 flex justify-end gap-2 border-t border-hairline">
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

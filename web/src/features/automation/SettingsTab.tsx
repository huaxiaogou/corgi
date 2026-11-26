import { useEffect, useState } from "react";
import { FolderOpen } from "lucide-react";
import { DirPicker } from "../../components/DirPicker";
import type { Machine, Settings } from "../../types/api";
import { ModelEffortFields } from "./ModelEffortFields";

interface Props {
  settings: Settings;
  hubAgents?: string[];
  machines?: Machine[];
  onSavePatch: (patch: Partial<Settings>, okMsg: string) => Promise<void>;
}

export function SettingsTab({ settings, hubAgents = [], machines = [], onSavePatch }: Props) {
  const [draft, setDraft] = useState<Settings>(settings);
  const [dirOpen, setDirOpen] = useState(false);

  useEffect(() => {
    setDraft(settings);
  }, [settings]);

  const workerMachineObj = machines.find(
    (m) => m.id === (draft.worker_machine ?? "") || (!draft.worker_machine && m.local)
  );

  const save = () =>
    onSavePatch(
      {
        router_kind: draft.router_kind,
        router_model: draft.router_model,
        router_effort: draft.router_effort,
        worker_kind: draft.worker_kind,
        worker_model: draft.worker_model,
        worker_effort: draft.worker_effort,
        worker_machine: draft.worker_machine,
        worker_cwd: draft.worker_cwd,
        reply_chars: draft.reply_chars,
        stall_minutes: draft.stall_minutes,
        ask_permissions: draft.ask_permissions,
      },
      "模型与运行设置已保存"
    );

  return (
    <div className="space-y-5">
      <section className="p-4 rounded-xl border border-hairline space-y-3">
        <div>
          <div className="font-semibold text-sm">Corgi 大脑（运行在 Hub）</div>
          <div className="text-[11.5px] text-muted mt-0.5">
            只负责理解意图、分派任务和简短回复，建议选用轻量快速模型并将思考强度设为 <span className="font-mono-code">low</span>。
          </div>
        </div>
        <ModelEffortFields
          kind={draft.router_kind}
          model={draft.router_model ?? ""}
          effort={draft.router_effort ?? ""}
          allowedKinds={hubAgents}
          recommendLowEffort
          onChange={({ kind, model, effort }) =>
            setDraft((prev) => ({ ...prev, router_kind: kind, router_model: model, router_effort: effort }))
          }
        />
      </section>

      <section className="p-4 rounded-xl border border-hairline space-y-3">
        <div>
          <div className="font-semibold text-sm">派活默认（Worker）</div>
          <div className="text-[11.5px] text-muted mt-0.5">
            Corgi 派 Agent 干活时的默认配置；你在聊天框或消息里指定电脑、Agent 或模型时会优先按你指定的运行。
          </div>
        </div>
        <ModelEffortFields
          kind={draft.worker_kind}
          model={draft.worker_model ?? ""}
          effort={draft.worker_effort ?? ""}
          machine={draft.worker_machine ?? ""}
          allowedKinds={workerMachineObj?.agents}
          onChange={({ kind, model, effort }) =>
            setDraft((prev) => ({ ...prev, worker_kind: kind, worker_model: model, worker_effort: effort }))
          }
        />
        <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
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
          <div className="sm:col-span-2 space-y-1">
            <span className="text-muted">默认工作目录</span>
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

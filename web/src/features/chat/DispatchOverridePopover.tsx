import type { RefObject } from "react";
import { X } from "lucide-react";
import { resolveEffectiveDispatch } from "../../lib/dispatchDefaults";
import { getSupportedEfforts } from "../../lib/models";
import type { Machine, ModelCatalog, WorkerDispatchDefaults } from "../../types/api";

interface Props {
  popoverRef: RefObject<HTMLDivElement | null>;
  machines: Machine[];
  dispatchDefaults?: WorkerDispatchDefaults;
  machine: string;
  kind: string;
  model: string;
  effort: string;
  availableKinds: string[];
  catalog: ModelCatalog | null;
  baseModelDisplay: string;
  effectiveBaseModel: string;
  effectiveBaseEffort: string;
  efforts: string[];
  hideEffort: boolean;
  isOverride: boolean;
  onChangeMachine: (v: string) => void;
  onChangeKind: (v: string) => void;
  onChangeModel: (v: string) => void;
  onChangeEffort: (v: string) => void;
  onClearOverride: () => void;
  onClose: () => void;
  onOpenSettings?: () => void;
}

export function DispatchOverridePopover({
  popoverRef,
  machines,
  dispatchDefaults,
  machine,
  kind,
  model,
  effort,
  availableKinds,
  catalog,
  baseModelDisplay,
  effectiveBaseModel,
  effectiveBaseEffort,
  efforts,
  hideEffort,
  isOverride,
  onChangeMachine,
  onChangeKind,
  onChangeModel,
  onChangeEffort,
  onClearOverride,
  onClose,
  onOpenSettings,
}: Props) {
  return (
    <div
      ref={popoverRef}
      className="absolute left-0 bottom-[calc(100%+10px)] w-[min(430px,calc(100vw-40px))] rounded-2xl card-floating border border-strong p-4 z-40 space-y-3 text-xs"
    >
      <div className="flex items-start justify-between gap-2">
        <div>
          <div className="font-semibold text-[13px] text-primary">仅本次派活临时指定</div>
          <div className="text-[11px] text-muted mt-0.5">只影响接下来发送的这一条消息，发送后自动还原为全局默认。</div>
        </div>
        <button
          type="button"
          onClick={onClose}
          className="w-6 h-6 rounded-md flex items-center justify-center text-muted hover:text-primary surface-hover cursor-pointer"
          title="关闭"
        >
          <X className="w-3.5 h-3.5" />
        </button>
      </div>

      <div className="grid grid-cols-1 sm:grid-cols-2 gap-2.5">
        <label className="space-y-1">
          <span className="text-[11px] text-muted">电脑</span>
          <select
            value={machine}
            onChange={(e) => onChangeMachine(e.target.value)}
            className="w-full px-2.5 py-1.5 rounded-lg surface-rail border border-hairline outline-none"
          >
            <option value="">沿用默认（{resolveEffectiveDispatch(dispatchDefaults, machines).machineLabel}）</option>
            {machines.map((m) => (
              <option key={m.id || "hub"} value={m.local ? "hub" : m.id}>
                {m.name}
                {m.local ? "（Hub）" : ""}
              </option>
            ))}
          </select>
        </label>
        <label className="space-y-1">
          <span className="text-[11px] text-muted">工具</span>
          <select
            value={kind}
            onChange={(e) => onChangeKind(e.target.value)}
            className="w-full px-2.5 py-1.5 rounded-lg surface-rail border border-hairline outline-none"
          >
            <option value="">沿用默认（{resolveEffectiveDispatch(dispatchDefaults, machines, { machine }).kind}）</option>
            {availableKinds.map((k) => (
              <option key={k} value={k}>
                {k}
              </option>
            ))}
          </select>
        </label>
      </div>

      <div className={`grid grid-cols-1 ${hideEffort ? "" : "sm:grid-cols-2"} gap-2.5`}>
        <label className="space-y-1">
          <span className="text-[11px] text-muted">模型</span>
          <select
            value={model}
            onChange={(e) => {
              const nextModel = e.target.value;
              const nextEfforts = getSupportedEfforts(catalog, nextModel || effectiveBaseModel);
              onChangeModel(nextModel);
              if (effort && !nextEfforts.includes(effort)) onChangeEffort("");
            }}
            className="w-full px-2.5 py-1.5 rounded-lg surface-rail border border-hairline outline-none"
          >
            <option value="">沿用默认（当前：{baseModelDisplay}）</option>
            {(catalog?.models ?? []).map((m) => (
              <option key={m.id} value={m.id}>
                {m.label || m.id}
              </option>
            ))}
          </select>
        </label>
        {!hideEffort && (
          <label className="space-y-1">
            <span className="text-[11px] text-muted">思考强度</span>
            <select
              value={effort}
              onChange={(e) => onChangeEffort(e.target.value)}
              className="w-full px-2.5 py-1.5 rounded-lg surface-rail border border-hairline outline-none"
            >
              <option value="">沿用默认{effectiveBaseEffort ? `（${effectiveBaseEffort}）` : ""}</option>
              {efforts.map((ef) => (
                <option key={ef} value={ef}>
                  {ef}
                </option>
              ))}
            </select>
          </label>
        )}
      </div>

      <div className="pt-2.5 border-t border-hairline flex items-center justify-between gap-2">
        {onOpenSettings ? (
          <button
            type="button"
            onClick={() => {
              onClose();
              onOpenSettings();
            }}
            className="text-[11.5px] text-secondary hover:text-primary underline underline-offset-2 cursor-pointer"
          >
            去改全局默认设置 →
          </button>
        ) : (
          <span />
        )}
        <div className="flex items-center gap-1.5">
          {isOverride && (
            <button
              type="button"
              onClick={onClearOverride}
              className="px-2.5 py-1 rounded-lg border border-hairline surface-rail surface-hover text-secondary cursor-pointer"
            >
              恢复默认
            </button>
          )}
          <button
            type="button"
            onClick={onClose}
            className="px-2.5 py-1 rounded-lg btn-accent font-medium cursor-pointer"
          >
            应用到本条
          </button>
        </div>
      </div>
    </div>
  );
}

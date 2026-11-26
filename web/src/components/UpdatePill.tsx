import { ArrowUp, Loader2, RotateCcw } from "lucide-react";
import type { FleetUpdateController } from "../hooks/useFleetUpdate";

interface Props {
  fleetUpdate: FleetUpdateController;
  compact?: boolean;
}

export function UpdatePill({ fleetUpdate, compact = false }: Props) {
  const { outdatedOnline, latest, busy, phase, index, total, currentName, failedName, error, startFleetUpdate } =
    fleetUpdate;

  if (!busy && phase !== "failed" && outdatedOnline.length === 0) {
    return null;
  }

  let label = "";
  let tip = "";
  let failed = false;

  if (phase === "updating") {
    const step = total > 1 ? ` ${index}/${total}` : "";
    label = `更新中${step}…`;
    tip = `正在更新 ${currentName || "设备"}${step ? `（${index}/${total}）` : ""}…`;
  } else if (phase === "reconnecting") {
    label = "重连中…";
    tip = `${currentName || "节点"} 已更新，正在等待带着新版本重新上线…`;
  } else if (phase === "restarting") {
    label = "重启中…";
    tip = "Hub 新版本已就绪，正在热重启并等待页面自动刷新…";
  } else if (phase === "failed") {
    failed = true;
    label = compact ? "重试" : "重试更新";
    tip = `更新 ${failedName} 失败：${error || "未知错误"}（点击仅重试未完成机器）`;
  } else {
    const names = outdatedOnline.map((m) => m.name).join("、");
    label = compact ? latest : `更新到 ${latest}`;
    tip = `一键更新到 ${latest}（先升在线节点再升 Hub：${names}）`;
  }

  const baseCls = compact
    ? "inline-flex items-center gap-1 px-2.5 py-1 rounded-full text-[11.5px] font-medium shrink-0 cursor-pointer transition-opacity disabled:opacity-75"
    : "w-full px-3 py-1.5 rounded-xl flex items-center justify-center gap-1.5 text-[12px] font-medium cursor-pointer transition-opacity disabled:opacity-75";

  const toneCls = failed
    ? "border border-amber-500/40 bg-amber-500/12 text-amber-600 dark:text-amber-300"
    : "btn-accent";

  return (
    <button
      type="button"
      disabled={busy}
      title={tip}
      aria-label={tip}
      onClick={() => void startFleetUpdate()}
      className={`${baseCls} ${toneCls}`}
    >
      {busy ? (
        <Loader2 className="w-3.5 h-3.5 animate-spin shrink-0" />
      ) : failed ? (
        <RotateCcw className="w-3.5 h-3.5 shrink-0" />
      ) : (
        <ArrowUp className="w-3.5 h-3.5 shrink-0" />
      )}
      <span className="truncate">{label}</span>
    </button>
  );
}

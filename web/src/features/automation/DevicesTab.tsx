import { useEffect, useState } from "react";
import { ArrowUpCircle, RefreshCw } from "lucide-react";
import { StatusDot } from "../../components/CorgiMark";
import { api } from "../../lib/api";
import type { DeviceView, Machine, VersionStatus } from "../../types/api";

interface PairResult {
  code: string;
  kind: string;
  pair_url: string;
  addr: string;
}

interface Props {
  devices: DeviceView[];
  machines: Machine[];
  pairResult: PairResult | null;
  onGenPair: (kind: "browser" | "machine") => Promise<void>;
  onRevoke: (id: string) => Promise<void>;
  onToast: (msg: string) => void;
}

const INSTALL_URL = "https://github.com/huaxiaogou/corgi/releases/latest/download/install.sh";

export function DevicesTab({ devices, machines, pairResult, onGenPair, onRevoke, onToast }: Props) {
  const [verInfo, setVerInfo] = useState<VersionStatus | null>(null);
  const [updatingId, setUpdatingId] = useState<string | null>(null);

  const checkVersions = (soon = false) => {
    void api<VersionStatus>(`/api/version${soon ? "?soon=1" : ""}`)
      .then(setVerInfo)
      .catch(() => {});
  };

  useEffect(() => {
    checkVersions(true);
  }, []);

  const triggerUpdate = async (machineId: string, name: string) => {
    setUpdatingId(machineId);
    try {
      const qs = machineId ? `?machine=${encodeURIComponent(machineId)}` : "";
      const res = await api<{ version?: string; up_to_date?: boolean }>(`/api/update${qs}`, {});
      if (res.up_to_date) {
        onToast(`${name} 已是最新版本`);
      } else {
        onToast(`${name} 已升级至 ${res.version ?? "最新版"}，正在原地热重启…`);
        window.setTimeout(() => checkVersions(true), 3500);
      }
    } catch (err) {
      onToast(err instanceof Error ? err.message : "升级失败");
    } finally {
      setUpdatingId(null);
    }
  };

  const outdatedIds = new Set((verInfo?.outdated ?? []).map((o) => o.id));
  const joinCmd =
    pairResult && pairResult.kind === "machine"
      ? `curl -fsSL ${INSTALL_URL} | sh -s -- node --hub ${pairResult.addr} --code ${pairResult.code}`
      : "";

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex gap-2">
          <button type="button" onClick={() => void onGenPair("browser")} className="px-3.5 py-2 rounded-lg btn-accent font-medium cursor-pointer">
            连接手机 / 浏览器
          </button>
          <button type="button" onClick={() => void onGenPair("machine")} className="px-3.5 py-2 rounded-lg border border-strong text-primary cursor-pointer">
            连接一台新电脑 (Node)
          </button>
        </div>
        <button
          type="button"
          onClick={() => checkVersions(true)}
          className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg border border-hairline text-secondary surface-hover cursor-pointer"
        >
          <RefreshCw className="w-3.5 h-3.5" />
          <span>检查更新{verInfo?.latest ? `（最新 ${verInfo.latest}）` : ""}</span>
        </button>
      </div>

      {pairResult && (
        <div className="p-4 rounded-xl border border-hairline surface-rail space-y-2.5">
          <div className="font-medium">
            一次性配对码（10 分钟内有效）：<span className="font-mono-code ml-1">{pairResult.code}</span>
          </div>
          {pairResult.kind === "machine" ? (
            <div className="space-y-2">
              <div className="text-muted">在目标电脑执行（自动安装 + 配对 + 后台常驻）：</div>
              <pre className="p-2.5 rounded-lg surface-bg border border-hairline font-mono-code overflow-x-auto">{joinCmd}</pre>
            </div>
          ) : (
            <img src={`/api/qr?text=${encodeURIComponent(pairResult.pair_url)}`} alt="配对二维码" className="w-40 h-40 rounded-lg border border-hairline" />
          )}
        </div>
      )}

      <div className="space-y-2">
        <div className="font-medium text-secondary">算力主机与版本状态</div>
        <div className="rounded-xl border border-hairline divide-y divide-[var(--line)]">
          {machines.map((m) => {
            const needUpdate = m.online && outdatedIds.has(m.id);
            return (
              <div key={m.id || "hub"} className="px-4 py-3 flex items-center justify-between gap-3">
                <div className="min-w-0 space-y-0.5">
                  <div className="flex items-center gap-2">
                    <StatusDot status={m.online ? "online" : "idle"} />
                    <span className="font-medium">{m.name}</span>
                    {m.local && <span className="px-1.5 py-0.5 rounded surface-rail border border-hairline text-[10.5px] text-muted">Hub</span>}
                    {m.version && <span className="font-mono-code text-muted">{m.version}</span>}
                  </div>
                  <div className="text-[11px] font-mono-code text-muted truncate">
                    {m.os} · 已装 Agent: {(m.agents ?? []).join(", ") || "无"}
                  </div>
                </div>
                {needUpdate && verInfo?.latest && (
                  <button
                    type="button"
                    disabled={updatingId === m.id}
                    onClick={() => void triggerUpdate(m.id, m.name)}
                    className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg btn-accent cursor-pointer shrink-0 disabled:opacity-50"
                  >
                    <ArrowUpCircle className="w-3.5 h-3.5" />
                    <span>{updatingId === m.id ? "升级中…" : `一键升级 ${verInfo.latest}`}</span>
                  </button>
                )}
              </div>
            );
          })}
        </div>
      </div>

      <div className="space-y-2">
        <div className="font-medium text-secondary">已配对凭证</div>
        <div className="rounded-xl border border-hairline divide-y divide-[var(--line)]">
          {devices.length === 0 && <div className="p-4 text-muted">暂无已配对设备</div>}
          {devices.map((d) => (
            <div key={d.id} className="px-4 py-3 flex items-center justify-between">
              <div>
                <span className="font-medium">{d.name}</span>
                <span className="ml-2 font-mono-code text-muted">
                  {d.kind} {d.os ? `· ${d.os}` : ""}
                </span>
              </div>
              <button type="button" onClick={() => void onRevoke(d.id)} className="text-muted hover:underline cursor-pointer">
                移除
              </button>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

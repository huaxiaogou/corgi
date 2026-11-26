import { useEffect, useState } from "react";
import { ArrowUpCircle, Copy, Loader2, RefreshCw } from "lucide-react";
import { StatusDot } from "../../components/CorgiMark";
import type { FleetUpdateController } from "../../hooks/useFleetUpdate";
import { isOldVersion } from "../../lib/fleetUpdate";
import type { DeviceView, Machine, Settings } from "../../types/api";

interface PairResult {
  code: string;
  kind: string;
  pair_url: string;
  addr: string;
}

interface Props {
  devices: DeviceView[];
  machines: Machine[];
  settings: Settings | null;
  effectivePublicURL?: string;
  flagPublicURL?: string;
  fleetUpdate: FleetUpdateController;
  pairResult: PairResult | null;
  onGenPair: (kind: "browser" | "machine") => Promise<void>;
  onRevoke: (id: string) => Promise<void>;
  onSaveSettingsPatch: (patch: Partial<Settings>, okMsg: string) => Promise<void>;
  onToast: (msg: string) => void;
}

const INSTALL_URL = "https://github.com/huaxiaogou/corgi/releases/latest/download/install.sh";

export function DevicesTab({
  devices,
  machines,
  settings,
  effectivePublicURL = "",
  flagPublicURL = "",
  fleetUpdate,
  pairResult,
  onGenPair,
  onRevoke,
  onSaveSettingsPatch,
  onToast,
}: Props) {
  const [savingAuto, setSavingAuto] = useState(false);
  const [publicURLDraft, setPublicURLDraft] = useState(settings?.public_url ?? "");
  const [savingURL, setSavingURL] = useState(false);

  useEffect(() => {
    setPublicURLDraft(settings?.public_url ?? "");
  }, [settings?.public_url]);
  const {
    verInfo,
    outdatedOnline,
    latest,
    busy,
    phase,
    currentId,
    errorFor,
    checkVersions,
    startFleetUpdate,
    startSingleUpdate,
  } = fleetUpdate;

  useEffect(() => {
    void checkVersions(true);
  }, [checkVersions]);

  const copyCmd = async (cmd: string) => {
    try {
      await navigator.clipboard.writeText(cmd);
      onToast("已复制升级命令");
    } catch {
      onToast("复制失败，请手动选中复制");
    }
  };

  const toggleAutoUpdate = async (checked: boolean) => {
    setSavingAuto(true);
    try {
      await onSaveSettingsPatch(
        { auto_update: checked },
        checked ? "已开启凌晨空闲自动更新" : "已关闭凌晨空闲自动更新"
      );
    } finally {
      setSavingAuto(false);
    }
  };

  const savePublicURL = async () => {
    setSavingURL(true);
    try {
      const cleaned = publicURLDraft.trim();
      await onSaveSettingsPatch(
        { public_url: cleaned },
        cleaned ? "对外访问地址已保存" : "已清空自定义地址（恢复沿用服务器默认地址）"
      );
    } finally {
      setSavingURL(false);
    }
  };

  const dreamAt = (settings?.dream_at ?? "04:00").trim() || "04:00";
  const dreamOff = dreamAt.toLowerCase() === "off";

  const joinCmd =
    pairResult && pairResult.kind === "machine"
      ? `curl -fsSL ${INSTALL_URL} | sh -s -- node --hub ${pairResult.addr} --code ${pairResult.code}`
      : "";

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap gap-2">
          <button type="button" onClick={() => void onGenPair("browser")} className="px-3.5 py-2 rounded-lg btn-accent font-medium cursor-pointer">
            连接手机 / 浏览器
          </button>
          <button type="button" onClick={() => void onGenPair("machine")} className="px-3.5 py-2 rounded-lg border border-strong text-primary cursor-pointer">
            连接一台新电脑 (Node)
          </button>
        </div>
        <div className="flex items-center gap-2">
          {(outdatedOnline.length > 0 || busy || phase === "failed") && (
            <button
              type="button"
              disabled={busy}
              onClick={() => void startFleetUpdate()}
              className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg btn-accent font-medium cursor-pointer disabled:opacity-60"
            >
              {busy ? <Loader2 className="w-3.5 h-3.5 animate-spin" /> : <ArrowUpCircle className="w-3.5 h-3.5" />}
              <span>
                {busy
                  ? phase === "restarting"
                    ? "Hub 重启中…"
                    : phase === "reconnecting"
                      ? "等待节点重连…"
                      : "全端升级中…"
                  : phase === "failed"
                    ? "重试全端升级"
                    : `一键升级全端 ${latest}`}
              </span>
            </button>
          )}
          <button
            type="button"
            onClick={() => void checkVersions(true)}
            className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg border border-hairline text-secondary surface-hover cursor-pointer"
          >
            <RefreshCw className="w-3.5 h-3.5" />
            <span>检查更新{verInfo?.latest ? `（最新 ${verInfo.latest}）` : ""}</span>
          </button>
        </div>
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
            const id = m.local ? "" : m.id;
            const online = m.local || m.online;
            const curVer = m.local ? m.version || verInfo?.version || "" : m.version || "";
            const needUpdate = online && Boolean(latest) && isOldVersion(curVer, latest);
            const isCurrent = busy && currentId === id;
            const errMsg = errorFor(id);
            const manualCmd = `curl -fsSL ${INSTALL_URL} | sh -s -- ${m.local ? "hub" : "node"}`;

            return (
              <div key={id || "hub"} className="px-4 py-3 space-y-2">
                <div className="flex items-center justify-between gap-3">
                  <div className="min-w-0 space-y-0.5">
                    <div className="flex items-center gap-2 flex-wrap">
                      <StatusDot status={online ? "online" : "idle"} />
                      <span className="font-medium">{m.name}</span>
                      {m.local && <span className="px-1.5 py-0.5 rounded surface-rail border border-hairline text-[10.5px] text-muted">Hub</span>}
                      {curVer && (
                        <span className={`font-mono-code ${needUpdate ? "text-amber-600 dark:text-amber-300" : "text-muted"}`}>
                          {needUpdate ? `${curVer} → ${latest}` : curVer}
                        </span>
                      )}
                    </div>
                    <div className="text-[11px] font-mono-code text-muted truncate">
                      {m.os} · 已装 Agent: {(m.agents ?? []).join(", ") || "无"}
                    </div>
                  </div>
                  {(needUpdate || isCurrent) && (
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() => void startSingleUpdate(id, m.name)}
                      className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg btn-accent cursor-pointer shrink-0 disabled:opacity-60"
                    >
                      {isCurrent ? <Loader2 className="w-3.5 h-3.5 animate-spin" /> : <ArrowUpCircle className="w-3.5 h-3.5" />}
                      <span>
                        {isCurrent
                          ? phase === "restarting"
                            ? "重启中…"
                            : phase === "reconnecting"
                              ? "重连中…"
                              : "升级中…"
                          : errMsg
                            ? `重试升级 ${latest}`
                            : `升级到 ${latest}`}
                      </span>
                    </button>
                  )}
                </div>

                {isCurrent && (
                  <div className="text-[11.5px] text-secondary">
                    {phase === "restarting"
                      ? "新版本已就绪，Hub 正在热重启并等待页面自动刷新（运行中的 Agent 不受影响）…"
                      : phase === "reconnecting"
                        ? "节点已完成安装，正在等待带着新版本重新上线…"
                        : `正在下载并校验 ${latest}（节点无法直连 GitHub 时将自动由 Hub 中转分发）…`}
                  </div>
                )}

                {errMsg && !isCurrent && (
                  <div className="px-2.5 py-1.5 rounded-lg border border-amber-500/30 bg-amber-500/10 text-[11.5px] text-amber-700 dark:text-amber-300">
                    升级失败：{errMsg}
                  </div>
                )}

                {needUpdate && (
                  <div className="flex items-center justify-between gap-2 px-2.5 py-1.5 rounded-lg surface-rail border border-hairline">
                    <span className="font-mono-code text-[11px] text-muted truncate" title={manualCmd}>
                      手动升级：{manualCmd}
                    </span>
                    <button
                      type="button"
                      onClick={() => void copyCmd(manualCmd)}
                      title="复制手动升级命令"
                      className="p-1 rounded text-secondary surface-hover cursor-pointer shrink-0"
                    >
                      <Copy className="w-3.5 h-3.5" />
                    </button>
                  </div>
                )}
              </div>
            );
          })}
        </div>
      </div>

      {settings && (
        <section className="p-4 rounded-xl border border-hairline space-y-2.5">
          <div className="font-semibold text-sm">对外访问地址</div>
          <div className="text-[11.5px] text-muted leading-relaxed">
            填写平时访问 Corgi 的网址，用于生成企微里的在线预览链接；留空则沿用服务器启动时的默认地址。
          </div>
          <div className="flex items-center gap-2">
            <input
              type="text"
              value={publicURLDraft}
              onChange={(e) => setPublicURLDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  void savePublicURL();
                }
              }}
              placeholder={flagPublicURL || "例如 http://9.134.247.217:7788 或反代域名"}
              className="flex-1 px-3 py-1.5 rounded-lg surface-rail border border-strong text-xs font-mono-code outline-none"
            />
            <button
              type="button"
              disabled={savingURL}
              onClick={() => void savePublicURL()}
              className="px-3.5 py-1.5 rounded-lg btn-accent font-medium cursor-pointer shrink-0 disabled:opacity-50"
            >
              保存
            </button>
          </div>
          {effectivePublicURL ? (
            <div className="text-[11px] text-muted font-mono-code truncate" title={effectivePublicURL}>
              当前生效：{effectivePublicURL}
            </div>
          ) : (
            <div className="text-[11px] text-muted">
              当前未配置对外地址，企微交付将仅发送原始文件附件。
            </div>
          )}
        </section>
      )}

      {settings && (
        <section className="p-4 rounded-xl border border-hairline space-y-2">
          <div className="font-semibold text-sm">自动更新策略</div>
          <label className="flex items-center gap-2 cursor-pointer">
            <input
              type="checkbox"
              disabled={savingAuto}
              checked={Boolean(settings.auto_update)}
              onChange={(e) => void toggleAutoUpdate(e.target.checked)}
            />
            <span className="font-medium">凌晨空闲自动更新（默认关闭，切换即生效）</span>
          </label>
          <div className="text-[11.5px] text-muted leading-relaxed">
            {dreamOff
              ? "开启后仅在凌晨 04:00–06:00 完全空闲时自动先升在线 Node、再热重启 Hub；运行中的 Agent 不受影响。"
              : `开启后仅在当天计划做梦（${dreamAt}）完成或静默跳过后的 3 小时夜间完全空闲窗口内自动先升在线 Node、再热重启 Hub；绝不打断做梦、会话交接与进行中的任务。`}
          </div>
        </section>
      )}

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

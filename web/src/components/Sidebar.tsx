import { useEffect, useState } from "react";
import { Hash, Moon, Pin, Plus, Settings as SettingsIcon, Sun, X } from "lucide-react";
import { isInternalCorgiPane } from "../lib/agentIdentity";
import { api } from "../lib/api";
import { isDecisionAction } from "../lib/chatFilter";
import type { AgentView, Machine, SuperView } from "../types/api";
import { AgentGlyph, CorgiMark, StatusDot } from "./CorgiMark";
import { HistorySection } from "./HistorySection";
import { UpdatePill } from "./UpdatePill";
import type { FleetUpdateController } from "../hooks/useFleetUpdate";

function formatIdleAge(iso?: string, nowMs = Date.now()): string {
  if (!iso) return "";
  const ms = nowMs - new Date(iso).getTime();
  if (!Number.isFinite(ms) || ms < 60_000) return "";
  const mins = Math.floor(ms / 60_000);
  if (mins < 60) return `${mins}m`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs}h`;
  return `${Math.floor(hrs / 24)}d`;
}

export type AppRoute = { view: "corgi"; thread: string } | { view: "agent"; paneId: string } | { view: "auto" };

interface Props {
  route: AppRoute;
  superView: SuperView | null;
  agents: AgentView[];
  shells: AgentView[];
  machines: Machine[];
  connected: boolean;
  dark: boolean;
  mobile?: boolean;
  fleetUpdate: FleetUpdateController;
  onSelect: (r: AppRoute) => void;
  onOpenLauncher: () => void;
  onToggleTheme: () => void;
  onCloseMobile?: () => void;
  onToast: (msg: string) => void;
}

export function Sidebar({
  route,
  superView,
  agents,
  shells,
  machines,
  connected,
  dark,
  mobile = false,
  fleetUpdate,
  onSelect,
  onOpenLauncher,
  onToggleTheme,
  onCloseMobile,
  onToast,
}: Props) {
  const [nowMs, setNowMs] = useState(() => Date.now());
  const [keepOverride, setKeepOverride] = useState<Record<string, boolean>>({});

  useEffect(() => {
    const t = window.setInterval(() => setNowMs(Date.now()), 30_000);
    return () => window.clearInterval(t);
  }, []);

  const visibleAgents = agents.filter((a) => !isInternalCorgiPane(a));
  const visibleShells = shells.filter((s) => !isInternalCorgiPane(s));
  const blocked = visibleAgents.filter((a) => a.status === "blocked");
  const running = visibleAgents.filter(
    (a) => a.status === "working" || a.status === "starting" || a.status === "waiting"
  );
  const idleAgents = visibleAgents.filter(
    (a) => a.status !== "blocked" && a.status !== "working" && a.status !== "starting" && a.status !== "waiting"
  );
  const activeWorkers = (superView?.workers ?? []).filter((w) => !w.ended);
  const runningWorkers = activeWorkers.filter(
    (w) => w.status === "working" || w.status === "starting" || w.status === "waiting"
  );
  const pendingDecisions = (superView?.actions ?? []).filter(isDecisionAction);
  const mainPending = pendingDecisions.filter((a) => a.thread === "chat:main").length;
  const wecomGroups = (superView?.threads ?? []).filter((t) => t.id.startsWith("wecom:"));

  const isKept = (a: AgentView) => (a.name in keepOverride ? keepOverride[a.name] : Boolean(a.keep));

  const toggleKeep = async (a: AgentView, e: React.MouseEvent) => {
    e.stopPropagation();
    const next = !isKept(a);
    setKeepOverride((m) => ({ ...m, [a.name]: next }));
    try {
      await api(`/api/super/workers/${encodeURIComponent(a.name)}/keep`, { keep: next });
      onToast(next ? `已将 ${a.name} 设为常驻保留（豁免过期归档）` : `已取消 ${a.name} 的常驻保留`);
    } catch (err) {
      setKeepOverride((m) => {
        const copy = { ...m };
        delete copy[a.name];
        return copy;
      });
      onToast(err instanceof Error ? err.message : "设置失败");
    }
  };

  const corgiSubtitle =
    mainPending > 0
      ? `${mainPending} 项待你拍板`
      : runningWorkers.length > 0
        ? `${runningWorkers.length} 个任务在跑`
        : superView?.router.status === "working"
          ? `Corgi 在想…${activeWorkers.length > 0 ? ` · ${activeWorkers.length} 个会话就绪` : ""}`
          : activeWorkers.length > 0
            ? `${activeWorkers.length} 个受管会话就绪`
            : "随时待命";

  const pick = (next: AppRoute) => {
    onSelect(next);
    onCloseMobile?.();
  };

  return (
    <aside
      className={`${mobile ? "w-[272px] h-full" : "hidden md:flex w-[252px] shrink-0"} flex flex-col surface-rail border-r border-hairline select-none`}
    >
      <div className="h-14 px-4 flex items-center justify-between">
        <div className="flex items-center gap-2.5">
          <div className="w-7 h-7 rounded-lg flex items-center justify-center surface-bg border border-hairline">
            <CorgiMark className="w-[18px] h-[18px]" />
          </div>
          <span className="font-semibold text-[15px] tracking-tight">Corgi</span>
        </div>
        <div className="flex items-center gap-1">
          <button
            type="button"
            onClick={onOpenLauncher}
            title="新建独立会话"
            className="w-7 h-7 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
          >
            <Plus className="w-4 h-4" strokeWidth={1.7} />
          </button>
          {mobile && (
            <button
              type="button"
              onClick={onCloseMobile}
              className="w-7 h-7 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
            >
              <X className="w-4 h-4" />
            </button>
          )}
        </div>
      </div>

      <div className="px-2.5 space-y-0.5">
        <button
          type="button"
          onClick={() => pick({ view: "corgi", thread: "chat:main" })}
          className={`w-full px-2.5 py-2 rounded-xl flex items-center justify-between text-left cursor-pointer ${
            route.view === "corgi" && route.thread === "chat:main" ? "surface-active" : "surface-hover"
          }`}
        >
          <div className="flex items-center gap-2.5 min-w-0">
            <CorgiMark className="w-4 h-4 shrink-0 text-primary" />
            <div className="min-w-0">
              <div className="text-[13.5px] font-medium truncate">和 Corgi 对话</div>
              <div className="text-[11px] text-muted truncate">{corgiSubtitle}</div>
            </div>
          </div>
          <StatusDot status={mainPending > 0 ? "blocked" : superView?.router.status ?? "idle"} />
        </button>

        <div className="pt-1">
          <UpdatePill fleetUpdate={fleetUpdate} />
        </div>
      </div>

      <div className="flex-1 overflow-y-auto px-2.5 pt-4 pb-3 space-y-4">
        {wecomGroups.length > 0 && (
          <div className="space-y-1">
            <div className="px-2.5 text-[11px] font-medium text-muted">企微群聊 {wecomGroups.length}</div>
            {wecomGroups.map((th) => {
              const groupPending = pendingDecisions.filter((a) => a.thread === th.id).length;
              const label = th.title || th.id.replace(/^wecom:/, "");
              return (
                <button
                  key={th.id}
                  type="button"
                  onClick={() => pick({ view: "corgi", thread: th.id })}
                  className={`w-full px-2.5 py-1.5 rounded-xl flex items-center justify-between gap-2 text-left cursor-pointer ${
                    route.view === "corgi" && route.thread === th.id ? "surface-active" : "surface-hover"
                  }`}
                >
                  <div className="flex items-center gap-2 min-w-0">
                    <Hash className="w-3.5 h-3.5 text-muted shrink-0" />
                    <span className="text-[13px] truncate text-primary">{label}</span>
                  </div>
                  {groupPending > 0 && <StatusDot status="blocked" />}
                </button>
              );
            })}
          </div>
        )}

        {blocked.length > 0 && (
          <div className="space-y-1">
            <div className="px-2.5 text-[11px] font-medium text-secondary">需要你确认 · {blocked.length}</div>
            {blocked.map((a) => (
              <button
                key={a.pane_id}
                type="button"
                onClick={() => pick({ view: "agent", paneId: a.pane_id })}
                className={`w-full px-2.5 py-2 rounded-xl flex items-center justify-between gap-2 text-left cursor-pointer ${
                  route.view === "agent" && route.paneId === a.pane_id ? "sidebar-direct-focus" : "surface-hover"
                }`}
              >
                <div className="flex items-center gap-2 min-w-0">
                  <AgentGlyph kind={a.kind} className="w-3.5 h-3.5 text-secondary shrink-0" />
                  <div className="min-w-0">
                    <div className="text-[13px] font-medium truncate">{a.topic || a.name}</div>
                    <div className="text-[11px] font-mono-code text-muted truncate">
                      {a.kind}
                      {a.machine ? ` · ${a.machine}` : ""}
                      {a.worktree_branch ? ` · ${a.worktree_branch}` : ""}
                    </div>
                  </div>
                </div>
                <StatusDot status="blocked" />
              </button>
            ))}
          </div>
        )}

        <div className="space-y-1">
          <div className="px-2.5 text-[11px] font-medium text-muted">运行中 {running.length}</div>
          {running.map((a) => (
            <button
              key={a.pane_id}
              type="button"
              onClick={() => pick({ view: "agent", paneId: a.pane_id })}
              className={`w-full px-2.5 py-2 rounded-xl flex items-center justify-between gap-2 text-left cursor-pointer ${
                route.view === "agent" && route.paneId === a.pane_id ? "sidebar-direct-focus" : "surface-hover"
              }`}
            >
              <div className="flex items-center gap-2 min-w-0">
                <AgentGlyph kind={a.kind} className="w-3.5 h-3.5 text-muted shrink-0" />
                <div className="min-w-0">
                  <div className="text-[13px] text-primary truncate">{a.topic || a.name}</div>
                  <div className="text-[11px] font-mono-code text-muted truncate">
                    {a.kind}
                    {a.machine ? ` · ${a.machine}` : ""}
                    {a.worktree_branch ? ` · ${a.worktree_branch}` : ""}
                  </div>
                </div>
              </div>
              <StatusDot status={a.status} />
            </button>
          ))}
        </div>

        {idleAgents.length > 0 && (
          <div className="space-y-1">
            <div className="px-2.5 flex items-center justify-between text-[11px] font-medium text-muted">
              <span>空闲待命 {idleAgents.length}</span>
            </div>
            {idleAgents.map((a) => {
              const kept = isKept(a);
              const age = formatIdleAge(a.idle_since, nowMs);
              return (
                <div
                  key={a.pane_id}
                  role="button"
                  tabIndex={0}
                  onClick={() => pick({ view: "agent", paneId: a.pane_id })}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") pick({ view: "agent", paneId: a.pane_id });
                  }}
                  className={`group w-full px-2.5 py-2 rounded-xl flex items-center justify-between gap-2 text-left cursor-pointer ${
                    route.view === "agent" && route.paneId === a.pane_id ? "sidebar-direct-focus" : "surface-hover"
                  }`}
                >
                  <div className="flex items-center gap-2 min-w-0">
                    <AgentGlyph kind={a.kind} className="w-3.5 h-3.5 text-muted shrink-0" />
                    <div className="min-w-0">
                      <div className="text-[13px] text-primary truncate">{a.topic || a.name}</div>
                      <div className="text-[11px] font-mono-code text-muted truncate">
                        {a.kind}
                        {a.machine ? ` · ${a.machine}` : ""}
                        {age ? ` · 空闲 ${age}` : ""}
                        {a.created_by === "you" ? " · 手动" : ""}
                        {kept ? " · 常驻" : ""}
                        {a.worktree_branch ? ` · ${a.worktree_branch}` : ""}
                      </div>
                    </div>
                  </div>
                  <div className="flex items-center gap-1 shrink-0">
                    <button
                      type="button"
                      onClick={(e) => void toggleKeep(a, e)}
                      title={kept ? "取消常驻保留" : "设为常驻保留（豁免过期归档）"}
                      className={`p-1 rounded-md cursor-pointer transition-opacity ${
                        kept ? "text-primary opacity-100" : "text-muted opacity-0 group-hover:opacity-100 focus:opacity-100"
                      }`}
                    >
                      <Pin className="w-3 h-3" strokeWidth={kept ? 2.2 : 1.7} fill={kept ? "currentColor" : "none"} />
                    </button>
                    <StatusDot status={a.status} />
                  </div>
                </div>
              );
            })}
          </div>
        )}

        {visibleShells.length > 0 && (
          <div className="space-y-1">
            <div className="px-2.5 text-[11px] font-medium text-muted">终端 {visibleShells.length}</div>
            {visibleShells.map((s) => (
              <button
                key={s.pane_id}
                type="button"
                onClick={() => pick({ view: "agent", paneId: s.pane_id })}
                className={`w-full px-2.5 py-1.5 rounded-xl flex items-center gap-2 text-left cursor-pointer ${
                  route.view === "agent" && route.paneId === s.pane_id ? "sidebar-direct-focus" : "surface-hover"
                }`}
              >
                <AgentGlyph kind="shell" className="w-3.5 h-3.5 text-muted shrink-0" />
                <span className="text-[12.5px] text-secondary truncate">{s.name || s.cwd}</span>
              </button>
            ))}
          </div>
        )}

        <HistorySection onSelectPane={(paneId) => pick({ view: "agent", paneId })} onToast={onToast} />
      </div>

      <footer className="h-11 px-3 flex items-center justify-between text-[11.5px] border-t border-hairline text-muted shrink-0 gap-2">
        <div className="flex items-center gap-2 min-w-0">
          <StatusDot status={connected ? "online" : "idle"} />
          <span className="truncate">
            {machines.filter((m) => m.online).length}/{Math.max(machines.length, 1)} 台电脑
            {superView?.wecom_online ? " · 企微在线" : ""}
          </span>
        </div>
        <div className="flex items-center gap-1 shrink-0">
          <button
            type="button"
            onClick={onToggleTheme}
            className="w-7 h-7 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
            title="切换深浅色"
          >
            {dark ? <Sun className="w-3.5 h-3.5" /> : <Moon className="w-3.5 h-3.5" />}
          </button>
          <button
            type="button"
            onClick={() => pick({ view: "auto" })}
            className={`w-7 h-7 rounded-lg flex items-center justify-center cursor-pointer transition-colors ${
              route.view === "auto" ? "btn-accent" : "text-secondary surface-hover"
            }`}
            title="自动化与设置（模型 · 项目 · 企微 · 记忆 · 设备）"
            aria-label="自动化与设置"
          >
            <SettingsIcon className="w-3.5 h-3.5" strokeWidth={1.8} />
          </button>
        </div>
      </footer>
    </aside>
  );
}

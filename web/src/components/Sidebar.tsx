import { useEffect, useState } from "react";
import { ChevronDown, ChevronRight, Hash, History, Moon, Plus, RotateCcw, SlidersHorizontal, Sun, X } from "lucide-react";
import { api } from "../lib/api";
import type { AgentView, EndedSession, Machine, SuperView } from "../types/api";
import { AgentGlyph, CorgiMark, StatusDot } from "./CorgiMark";

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
  onSelect,
  onOpenLauncher,
  onToggleTheme,
  onCloseMobile,
  onToast,
}: Props) {
  const [historyOpen, setHistoryOpen] = useState(false);
  const [history, setHistory] = useState<EndedSession[]>([]);
  const [resumingId, setResumingId] = useState("");

  useEffect(() => {
    if (!historyOpen) return;
    void api<{ sessions: EndedSession[] }>("/api/history")
      .then((r) => setHistory(r.sessions ?? []))
      .catch(() => setHistory([]));
  }, [historyOpen]);

  const blocked = agents.filter((a) => a.status === "blocked");
  const active = agents.filter((a) => a.status !== "blocked");
  const pendingActions = (superView?.actions ?? []).filter((a) => a.status === "pending");
  const mainPending = pendingActions.filter((a) => a.thread === "chat:main").length;
  const wecomGroups = (superView?.threads ?? []).filter((t) => t.id.startsWith("wecom:"));

  const pick = (next: AppRoute) => {
    onSelect(next);
    onCloseMobile?.();
  };

  const resumeSession = async (sess: EndedSession) => {
    setResumingId(sess.id);
    try {
      const res = await api<{ pane_id: string }>(`/api/history/${encodeURIComponent(sess.id)}/resume`, {});
      onToast("已续接历史会话");
      if (res.pane_id) pick({ view: "agent", paneId: res.pane_id });
    } catch (err) {
      onToast(err instanceof Error ? err.message : "续接失败");
    } finally {
      setResumingId("");
    }
  };

  return (
    <aside className={`${mobile ? "w-[288px] h-full shadow-2xl" : "w-[268px] hidden md:flex"} shrink-0 flex flex-col surface-rail border-r border-hairline`}>
      <div className="h-14 px-4 flex items-center justify-between shrink-0">
        <div className="flex items-center gap-2.5">
          <div className="w-7 h-7 rounded-lg border border-hairline flex items-center justify-center">
            <CorgiMark className="w-4 h-4" />
          </div>
          <span className="font-semibold text-[15px] tracking-tight">Corgi</span>
        </div>
        <div className="flex items-center gap-1">
          <button
            type="button"
            onClick={() => {
              onCloseMobile?.();
              onOpenLauncher();
            }}
            className="w-7 h-7 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
            title="手动新建独立 Agent 或终端"
          >
            <Plus className="w-4 h-4" strokeWidth={1.7} />
          </button>
          {mobile && onCloseMobile && (
            <button
              type="button"
              onClick={onCloseMobile}
              className="w-7 h-7 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
              title="关闭菜单"
            >
              <X className="w-4 h-4" />
            </button>
          )}
        </div>
      </div>

      <div className="px-2.5 space-y-1 shrink-0">
        <button
          type="button"
          onClick={() => pick({ view: "corgi", thread: "chat:main" })}
          className={`w-full flex items-center justify-between px-3 py-2.5 rounded-xl text-left cursor-pointer border ${
            route.view === "corgi" && route.thread === "chat:main" ? "surface-bg border-hairline" : "border-transparent text-secondary surface-hover"
          }`}
        >
          <div className="flex items-center gap-2.5 min-w-0">
            <CorgiMark className="w-4 h-4 shrink-0" />
            <div className="min-w-0">
              <div className="text-[13.5px] font-medium leading-tight">和 Corgi 对话</div>
              <div className="text-[11px] text-muted truncate mt-0.5">
                {mainPending > 0 ? `${mainPending} 项待拍板` : `${active.length} 个任务在跑`}
              </div>
            </div>
          </div>
          <StatusDot status={mainPending > 0 ? "blocked" : superView?.router.status ?? "asleep"} />
        </button>

        <button
          type="button"
          onClick={() => pick({ view: "auto" })}
          className={`w-full flex items-center gap-2.5 px-3 py-2 rounded-xl text-left cursor-pointer border ${
            route.view === "auto" ? "surface-bg border-hairline" : "border-transparent text-secondary surface-hover"
          }`}
        >
          <SlidersHorizontal className="w-4 h-4 shrink-0" strokeWidth={1.6} />
          <div className="min-w-0">
            <div className="text-[13.5px] font-medium leading-tight">自动化与设置</div>
            <div className="text-[11px] text-muted mt-0.5">企微 · 接入 · 记忆 · 多机</div>
          </div>
        </button>
      </div>

      <div className="flex-1 overflow-y-auto px-2.5 pt-4 pb-2 space-y-4">
        {wecomGroups.length > 0 && (
          <div className="space-y-1">
            <div className="px-2 text-[11px] text-muted">企微群频道 {wecomGroups.length}</div>
            {wecomGroups.map((th) => {
              const gPending = pendingActions.filter((a) => a.thread === th.id).length;
              const label = th.title || th.id.replace(/^wecom:/, "");
              const isCur = route.view === "corgi" && route.thread === th.id;
              return (
                <button
                  key={th.id}
                  type="button"
                  onClick={() => pick({ view: "corgi", thread: th.id })}
                  className={`w-full px-3 py-2 rounded-xl text-left flex items-center justify-between gap-2 cursor-pointer border ${
                    isCur ? "surface-bg border-hairline text-primary" : "border-transparent text-secondary surface-hover"
                  }`}
                >
                  <div className="flex items-center gap-2 min-w-0">
                    <Hash className="w-3.5 h-3.5 text-muted shrink-0" />
                    <span className="text-[13px] font-medium truncate">{label}</span>
                  </div>
                  {gPending > 0 && <StatusDot status="blocked" />}
                </button>
              );
            })}
          </div>
        )}

        {blocked.length > 0 && (
          <div className="space-y-1">
            <div className="px-2 text-[11px] text-muted">需要你</div>
            {blocked.map((a) => (
              <button
                key={a.pane_id}
                type="button"
                onClick={() => pick({ view: "agent", paneId: a.pane_id })}
                className="w-full px-3 py-2 rounded-xl text-left flex items-center justify-between gap-2 cursor-pointer"
                style={{ background: "var(--attn-soft)" }}
              >
                <div className="flex items-start gap-2.5 min-w-0">
                  <AgentGlyph kind={a.kind} className="w-4 h-4 mt-0.5 text-secondary shrink-0" />
                  <div className="min-w-0">
                    <div className="text-[13px] font-medium truncate">{a.topic || a.name}</div>
                    <div className="text-[11px] font-mono-code text-muted truncate">
                      {a.kind}
                      {a.machine ? ` · ${a.machine}` : ""}
                    </div>
                  </div>
                </div>
                <StatusDot status="blocked" />
              </button>
            ))}
          </div>
        )}

        <div className="space-y-1">
          <div className="px-2 text-[11px] text-muted">进行中 {active.length}</div>
          {active.map((a) => (
            <button
              key={a.pane_id}
              type="button"
              onClick={() => pick({ view: "agent", paneId: a.pane_id })}
              className="w-full px-3 py-2 rounded-xl text-left flex items-center justify-between gap-2 cursor-pointer surface-hover"
            >
              <div className="flex items-start gap-2.5 min-w-0">
                <AgentGlyph kind={a.kind} className="w-4 h-4 mt-0.5 text-secondary shrink-0" />
                <div className="min-w-0">
                  <div className="text-[13px] font-medium truncate">{a.topic || a.name}</div>
                  <div className="text-[11px] font-mono-code text-muted truncate">
                    {a.kind}
                    {a.machine ? ` · ${a.machine}` : ""}
                  </div>
                </div>
              </div>
              <StatusDot status={a.status} />
            </button>
          ))}
        </div>

        {shells.length > 0 && (
          <div className="space-y-1">
            <div className="px-2 text-[11px] text-muted">终端 {shells.length}</div>
            {shells.map((sh) => (
              <button
                key={sh.pane_id}
                type="button"
                onClick={() => pick({ view: "agent", paneId: sh.pane_id })}
                className="w-full px-3 py-1.5 rounded-xl text-left flex items-center gap-2 text-xs text-secondary surface-hover cursor-pointer"
              >
                <AgentGlyph kind="shell" isAgent={false} className="w-3.5 h-3.5 shrink-0" />
                <span className="truncate">{sh.name || sh.cwd}</span>
              </button>
            ))}
          </div>
        )}

        <div className="space-y-1 pt-1">
          <button
            type="button"
            onClick={() => setHistoryOpen((v) => !v)}
            className="w-full px-2 py-1 flex items-center justify-between text-[11px] text-muted hover:text-secondary cursor-pointer"
          >
            <span className="inline-flex items-center gap-1.5">
              <History className="w-3 h-3" />
              历史会话（可续接）
            </span>
            {historyOpen ? <ChevronDown className="w-3 h-3" /> : <ChevronRight className="w-3 h-3" />}
          </button>
          {historyOpen && (
            <div className="space-y-1">
              {history.length === 0 && <div className="px-2.5 py-1.5 text-[11px] text-muted">暂无归档会话</div>}
              {history.slice(0, 12).map((sess) => (
                <div key={sess.id} className="px-2.5 py-1.5 rounded-lg border border-hairline surface-bg flex items-center justify-between gap-2 text-xs">
                  <div className="min-w-0">
                    <div className="truncate font-medium text-primary">{sess.title || sess.name || sess.kind}</div>
                    <div className="text-[10.5px] font-mono-code text-muted truncate">
                      {sess.kind}
                      {sess.machine ? ` · ${sess.machine}` : ""}
                    </div>
                  </div>
                  <button
                    type="button"
                    disabled={resumingId === sess.id}
                    onClick={() => void resumeSession(sess)}
                    className="px-2 py-1 rounded-md border border-strong text-[11px] text-secondary surface-hover cursor-pointer shrink-0 inline-flex items-center gap-1 disabled:opacity-50"
                    title="用原会话 ID 续接"
                  >
                    <RotateCcw className="w-3 h-3" />
                    续接
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>

      <footer className="h-11 px-3.5 flex items-center justify-between text-[11.5px] border-t border-hairline text-muted shrink-0">
        <div className="flex items-center gap-2">
          <StatusDot status={connected ? "online" : "idle"} />
          <span>
            {machines.filter((m) => m.online).length}/{Math.max(machines.length, 1)} 台电脑
            {superView?.wecom_online ? " · 企微在线" : ""}
          </span>
        </div>
        <button type="button" onClick={onToggleTheme} className="text-secondary hover:opacity-80 cursor-pointer" title="切换深浅色">
          {dark ? <Sun className="w-3.5 h-3.5" /> : <Moon className="w-3.5 h-3.5" />}
        </button>
      </footer>
    </aside>
  );
}

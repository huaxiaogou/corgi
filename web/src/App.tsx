import { useEffect, useState } from "react";
import { ArrowLeft, Menu, Plus, RefreshCw } from "lucide-react";
import { AppShellSkeleton } from "./components/AppShellSkeleton";
import { AgentGlyph, CorgiMark, StatusDot } from "./components/CorgiMark";
import { Sidebar, type AppRoute } from "./components/Sidebar";
import { UpdatePill } from "./components/UpdatePill";
import { AgentEndedView } from "./features/agents/AgentEndedView";
import { AgentPaneView } from "./features/agents/AgentPaneView";
import { LoginView } from "./features/auth/LoginView";
import { AutomationView } from "./features/automation/AutomationView";
import { CorgiChatView } from "./features/chat/CorgiChatView";
import { FileDrawer } from "./features/files/FileDrawer";
import { LauncherModal } from "./features/launcher/LauncherModal";
import { useCorgiStream } from "./hooks/useCorgiStream";
import { useFleetUpdate } from "./hooks/useFleetUpdate";
import {
  formatDocumentTitle,
  formatMobileAgentBar,
  isInternalCorgiPane,
} from "./lib/agentIdentity";
import { api, onWebBuildChange } from "./lib/api";
import { matchLiveWorkerForRef } from "./lib/dispatchDefaults";
import type { EntryWorkerRef, FileDrawerTarget } from "./types/api";

export function App() {
  const [route, setRoute] = useState<AppRoute>({ view: "corgi", thread: "chat:main" });
  const [navSeq, setNavSeq] = useState(0);
  const [drawerOpen, setDrawerOpen] = useState(false);

  const selectRoute = (next: AppRoute) => {
    setRoute(next);
    setNavSeq((n) => n + 1);
    if (next.view === "auto") {
      setFileTarget(null);
    }
  };
  const [launchOpen, setLaunchOpen] = useState(false);
  const [fileTarget, setFileTarget] = useState<FileDrawerTarget | null>(null);
  const [toastMsg, setToastMsg] = useState("");
  const [webOutdated, setWebOutdated] = useState(false);
  const [dark, setDark] = useState(() => document.documentElement.dataset.theme === "dark");

  useEffect(() => onWebBuildChange(() => setWebOutdated(true)), []);

  const activeThread = route.view === "corgi" ? route.thread : "chat:main";
  const { authed, connected, superView, paneState, superReady, stateReady, refreshAuth } = useCorgiStream(activeThread);

  const toast = (msg: string) => {
    setToastMsg(msg);
    window.setTimeout(() => setToastMsg((cur) => (cur === msg ? "" : cur)), 2400);
  };

  const fleetUpdate = useFleetUpdate(authed === true, paneState?.machines ?? [], toast);

  const allAgents = paneState?.agents ?? [];
  const shells = paneState?.shells ?? [];
  const selectedPane =
    route.view === "agent" ? [...allAgents, ...shells].find((p) => p.pane_id === route.paneId) ?? null : null;

  useEffect(() => {
    if (authed !== true) {
      document.title = "Corgi";
      return;
    }
    document.title = formatDocumentTitle(route, selectedPane);
  }, [authed, route, selectedPane]);

  const toggleTheme = () => {
    const next = !dark;
    setDark(next);
    const val = next ? "dark" : "light";
    document.documentElement.dataset.theme = val;
    localStorage.setItem("corgi.theme", val);
  };

  if (authed === null) {
    return <AppShellSkeleton />;
  }
  if (!authed) {
    return <LoginView onSuccess={refreshAuth} />;
  }

  const agents = allAgents.filter((a) => !isInternalCorgiPane(a));
  const machines = paneState?.machines ?? [];
  const pendingCount = (superView?.actions ?? []).filter((a) => a.status === "pending").length;
  const backToCorgi = () => selectRoute({ view: "corgi", thread: "chat:main" });
  const mobileBar = route.view === "agent" ? formatMobileAgentBar(selectedPane) : null;

  const jumpToWorkerRef = async (ref: EntryWorkerRef): Promise<boolean> => {
    const live = matchLiveWorkerForRef(ref, superView?.workers);
    if (live?.pane_id) {
      selectRoute({ view: "agent", paneId: live.pane_id });
      return true;
    }
    try {
      const res = await api<{ ok: boolean; pane_id: string; resumed?: boolean; name?: string }>("/api/super/workers/resume", {
        name: ref.name,
        machine: ref.machine ?? "",
        session: ref.session ?? "",
        kind: ref.kind ?? "",
        work_item_id: ref.work_item_id ?? "",
      });
      if (res.pane_id) {
        toast(res.resumed ? `已原地恢复 ${res.name || ref.name}` : `已切入 ${res.name || ref.name}`);
        selectRoute({ view: "agent", paneId: res.pane_id });
        return true;
      }
      return false;
    } catch (err) {
      toast(err instanceof Error ? err.message : "打开该 Worker 会话失败");
      return false;
    }
  };

  return (
    <div className="h-full flex flex-col overflow-hidden surface-bg text-primary">
      {webOutdated && (
        <div className="px-4 py-1.5 border-b border-hairline surface-rail flex items-center justify-center gap-3 text-xs shrink-0">
          <span className="text-secondary">Corgi 控制台已更新到新版本</span>
          <button
            type="button"
            onClick={() => window.location.reload()}
            className="inline-flex items-center gap-1 px-2.5 py-0.5 rounded-md btn-accent cursor-pointer"
          >
            <RefreshCw className="w-3 h-3" />
            <span>立即刷新</span>
          </button>
        </div>
      )}

      <div className="flex-1 flex flex-col md:flex-row min-h-0 overflow-hidden">
        <div className="md:hidden h-12 px-3 flex items-center justify-between gap-2 border-b border-hairline surface-rail shrink-0">
          {route.view === "agent" ? (
            <>
              <button
                type="button"
                onClick={backToCorgi}
                className="inline-flex items-center gap-1.5 px-2 py-1 rounded-lg border border-hairline surface-bg text-xs font-medium text-primary surface-hover cursor-pointer shrink-0"
                title="返回 Corgi 主对话"
              >
                <ArrowLeft className="w-3.5 h-3.5 text-secondary" />
                <CorgiMark className="w-3.5 h-3.5" />
                <span>Corgi</span>
                {pendingCount > 0 && <StatusDot status="blocked" />}
              </button>
              <div className="flex-1 min-w-0 flex items-center justify-center gap-1.5">
                {selectedPane && (
                  <AgentGlyph
                    kind={selectedPane.kind}
                    isAgent={selectedPane.is_agent}
                    className="w-3.5 h-3.5 text-secondary shrink-0"
                  />
                )}
                <span className="text-[13px] font-semibold truncate">
                  {paneState === null ? "加载中…" : mobileBar?.title ?? "会话已结束"}
                </span>
                {mobileBar?.badge && (
                  <span className="direct-pill text-[10px] font-medium px-1.5 py-0.5 rounded-full shrink-0">
                    {mobileBar.badge}
                  </span>
                )}
              </div>
              <button
                type="button"
                onClick={() => setDrawerOpen(true)}
                className="w-8 h-8 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer shrink-0"
                title="打开侧栏"
              >
                <Menu className="w-4 h-4" />
              </button>
            </>
          ) : (
            <>
              <button type="button" onClick={() => setDrawerOpen(true)} className="w-8 h-8 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer" title="打开侧栏">
                <Menu className="w-4 h-4" />
              </button>
              <div className="flex items-center gap-2 text-sm font-semibold">
                <CorgiMark className="w-4 h-4" />
                <span>Corgi</span>
                {pendingCount > 0 && <StatusDot status="blocked" />}
              </div>
              <div className="flex items-center gap-1.5">
                <UpdatePill fleetUpdate={fleetUpdate} compact />
                <button
                  type="button"
                  disabled={!stateReady}
                  onClick={() => setLaunchOpen(true)}
                  className="w-8 h-8 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed"
                  title={stateReady ? "手动新建" : "正在同步机群状态…"}
                >
                  <Plus className="w-4 h-4" />
                </button>
              </div>
            </>
          )}
        </div>

        <Sidebar
          route={route}
          superView={superView}
          agents={agents}
          shells={shells}
          machines={machines}
          connected={connected}
          stateReady={stateReady}
          dark={dark}
          fleetUpdate={fleetUpdate}
          onSelect={selectRoute}
          onOpenLauncher={() => setLaunchOpen(true)}
          onToggleTheme={toggleTheme}
          onToast={toast}
        />

        {drawerOpen && (
          <div className="fixed inset-0 z-40 md:hidden flex" style={{ background: "rgba(0,0,0,0.35)" }} onClick={() => setDrawerOpen(false)}>
            <div onClick={(e) => e.stopPropagation()} className="h-full">
              <Sidebar
                mobile
                route={route}
                superView={superView}
                agents={agents}
                shells={shells}
                machines={machines}
                connected={connected}
                stateReady={stateReady}
                dark={dark}
                fleetUpdate={fleetUpdate}
                onSelect={selectRoute}
                onOpenLauncher={() => setLaunchOpen(true)}
                onToggleTheme={toggleTheme}
                onCloseMobile={() => setDrawerOpen(false)}
                onToast={toast}
              />
            </div>
          </div>
        )}

        <main className="flex-1 flex min-w-0 min-h-0 overflow-hidden">
          {route.view === "corgi" && (
            <CorgiChatView
              key={route.thread}
              data={superView}
              superReady={superReady}
              machines={machines}
              thread={route.thread}
              focusSeq={navSeq}
              onSelectWorker={(paneId) => selectRoute({ view: "agent", paneId })}
              onOpenFile={(p, m) => setFileTarget({ path: p, machine: m })}
              onOpenArtifact={(a) => setFileTarget(a)}
              onOpenSettings={() => selectRoute({ view: "auto" })}
              onToast={toast}
            />
          )}
          {route.view === "agent" && selectedPane && (
            <AgentPaneView
              key={selectedPane.pane_id}
              pane={selectedPane}
              stateReady={stateReady}
              focusSeq={navSeq}
              queuedPrompts={paneState?.queued_prompts?.[selectedPane.pane_id] ?? []}
              queuePaused={Boolean(paneState?.paused_queues?.[selectedPane.pane_id])}
              onBackToCorgi={backToCorgi}
              onClosed={backToCorgi}
              onOpenFile={(p, m) => setFileTarget({ path: p, machine: m ?? selectedPane.machine })}
              onToast={toast}
            />
          )}
          {route.view === "agent" && !selectedPane && (
            <AgentEndedView loading={paneState === null} onBackToCorgi={backToCorgi} />
          )}
          {route.view === "auto" && <AutomationView fleetUpdate={fleetUpdate} onToast={toast} />}
          <FileDrawer target={fileTarget} onClose={() => setFileTarget(null)} onJumpWorker={(ref) => jumpToWorkerRef(ref)} />
        </main>
      </div>

      <LauncherModal
        open={launchOpen}
        machines={machines}
        onClose={() => setLaunchOpen(false)}
        onLaunched={(paneId) => selectRoute({ view: "agent", paneId })}
        onToast={toast}
      />

      {toastMsg && (
        <div className="fixed top-16 left-1/2 -translate-x-1/2 pointer-events-none px-3.5 py-1.5 rounded-xl text-xs font-medium btn-accent shadow-lg z-50">
          {toastMsg}
        </div>
      )}
    </div>
  );
}

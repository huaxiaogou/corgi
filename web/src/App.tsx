import { useEffect, useState } from "react";
import { Menu, Plus, RefreshCw } from "lucide-react";
import { CorgiMark, StatusDot } from "./components/CorgiMark";
import { Sidebar, type AppRoute } from "./components/Sidebar";
import { UpdatePill } from "./components/UpdatePill";
import { AgentPaneView } from "./features/agents/AgentPaneView";
import { LoginView } from "./features/auth/LoginView";
import { AutomationView } from "./features/automation/AutomationView";
import { CorgiChatView } from "./features/chat/CorgiChatView";
import { FileDrawer } from "./features/files/FileDrawer";
import { LauncherModal } from "./features/launcher/LauncherModal";
import { useCorgiStream } from "./hooks/useCorgiStream";
import { useFleetUpdate } from "./hooks/useFleetUpdate";
import { onWebBuildChange } from "./lib/api";

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
  const [fileTarget, setFileTarget] = useState<{ path: string; machine?: string } | null>(null);
  const [toastMsg, setToastMsg] = useState("");
  const [webOutdated, setWebOutdated] = useState(false);
  const [dark, setDark] = useState(() => document.documentElement.dataset.theme === "dark");

  useEffect(() => onWebBuildChange(() => setWebOutdated(true)), []);

  const activeThread = route.view === "corgi" ? route.thread : "chat:main";
  const { authed, connected, superView, paneState, refreshAuth } = useCorgiStream(activeThread);

  const toast = (msg: string) => {
    setToastMsg(msg);
    window.setTimeout(() => setToastMsg((cur) => (cur === msg ? "" : cur)), 2400);
  };

  const fleetUpdate = useFleetUpdate(authed === true, paneState?.machines ?? [], toast);

  const toggleTheme = () => {
    const next = !dark;
    setDark(next);
    const val = next ? "dark" : "light";
    document.documentElement.dataset.theme = val;
    localStorage.setItem("corgi.theme", val);
  };

  if (authed === null) {
    return <div className="h-full flex items-center justify-center text-xs text-muted">连接中…</div>;
  }
  if (!authed) {
    return <LoginView onSuccess={refreshAuth} />;
  }

  const allAgents = paneState?.agents ?? [];
  const agents = allAgents.filter(
    (a) =>
      a.name !== "corgi" &&
      a.name !== "corgi-dream" &&
      a.workspace_label !== "corgi · router" &&
      a.workspace_label !== "corgi · dream" &&
      !a.workspace_label?.startsWith("corgi · dream · ")
  );
  const shells = paneState?.shells ?? [];
  const machines = paneState?.machines ?? [];
  const pendingCount = (superView?.actions ?? []).filter((a) => a.status === "pending").length;
  const selectedPane =
    route.view === "agent" ? [...allAgents, ...shells].find((p) => p.pane_id === route.paneId) ?? null : null;

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
        <div className="md:hidden h-12 px-3 flex items-center justify-between border-b border-hairline surface-rail shrink-0">
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
            <button type="button" onClick={() => setLaunchOpen(true)} className="w-8 h-8 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer" title="手动新建">
              <Plus className="w-4 h-4" />
            </button>
          </div>
        </div>

        <Sidebar
          route={route}
          superView={superView}
          agents={agents}
          shells={shells}
          machines={machines}
          connected={connected}
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
              machines={machines}
              thread={route.thread}
              focusSeq={navSeq}
              onSelectWorker={(paneId) => selectRoute({ view: "agent", paneId })}
              onOpenFile={(p, m) => setFileTarget({ path: p, machine: m })}
              onToast={toast}
            />
          )}
          {route.view === "agent" && selectedPane && (
            <AgentPaneView
              key={selectedPane.pane_id}
              pane={selectedPane}
              focusSeq={navSeq}
              queuedPrompts={paneState?.queued_prompts?.[selectedPane.pane_id] ?? []}
              queuePaused={Boolean(paneState?.paused_queues?.[selectedPane.pane_id])}
              onClosed={() => selectRoute({ view: "corgi", thread: "chat:main" })}
              onOpenFile={(p, m) => setFileTarget({ path: p, machine: m ?? selectedPane.machine })}
              onToast={toast}
            />
          )}
          {route.view === "auto" && <AutomationView fleetUpdate={fleetUpdate} onToast={toast} />}
          <FileDrawer target={fileTarget} onClose={() => setFileTarget(null)} />
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
        <div className="fixed bottom-5 left-1/2 -translate-x-1/2 px-3.5 py-2 rounded-xl text-xs font-medium btn-accent shadow-lg z-50">
          {toastMsg}
        </div>
      )}
    </div>
  );
}

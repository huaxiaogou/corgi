import { useState } from "react";
import { Menu, Plus } from "lucide-react";
import { CorgiMark, StatusDot } from "./components/CorgiMark";
import { Sidebar, type AppRoute } from "./components/Sidebar";
import { AgentPaneView } from "./features/agents/AgentPaneView";
import { LoginView } from "./features/auth/LoginView";
import { AutomationView } from "./features/automation/AutomationView";
import { CorgiChatView } from "./features/chat/CorgiChatView";
import { FileDrawer } from "./features/files/FileDrawer";
import { LauncherModal } from "./features/launcher/LauncherModal";
import { useCorgiStream } from "./hooks/useCorgiStream";

export function App() {
  const [route, setRoute] = useState<AppRoute>({ view: "corgi", thread: "chat:main" });
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [launchOpen, setLaunchOpen] = useState(false);
  const [fileTarget, setFileTarget] = useState<{ path: string; machine?: string } | null>(null);
  const [toastMsg, setToastMsg] = useState("");
  const [dark, setDark] = useState(() => document.documentElement.dataset.theme === "dark");

  const activeThread = route.view === "corgi" ? route.thread : "chat:main";
  const { authed, connected, superView, paneState, refreshAuth } = useCorgiStream(activeThread);

  const toast = (msg: string) => {
    setToastMsg(msg);
    window.setTimeout(() => setToastMsg((cur) => (cur === msg ? "" : cur)), 2400);
  };

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

  const agents = (paneState?.agents ?? []).filter((a) => a.name !== "corgi");
  const shells = paneState?.shells ?? [];
  const machines = paneState?.machines ?? [];
  const pendingCount = (superView?.actions ?? []).filter((a) => a.status === "pending").length;
  const selectedPane =
    route.view === "agent" ? [...agents, ...shells].find((p) => p.pane_id === route.paneId) ?? null : null;

  return (
    <div className="h-full flex flex-col md:flex-row overflow-hidden surface-bg text-primary">
      <div className="md:hidden h-12 px-3 flex items-center justify-between border-b border-hairline surface-rail shrink-0">
        <button
          type="button"
          onClick={() => setDrawerOpen(true)}
          className="w-8 h-8 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
          title="打开侧栏"
        >
          <Menu className="w-4 h-4" />
        </button>
        <div className="flex items-center gap-2 text-sm font-semibold">
          <CorgiMark className="w-4 h-4" />
          <span>Corgi</span>
          {pendingCount > 0 && <StatusDot status="blocked" />}
        </div>
        <button
          type="button"
          onClick={() => setLaunchOpen(true)}
          className="w-8 h-8 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
          title="手动新建"
        >
          <Plus className="w-4 h-4" />
        </button>
      </div>

      <Sidebar
        route={route}
        superView={superView}
        agents={agents}
        shells={shells}
        machines={machines}
        connected={connected}
        dark={dark}
        onSelect={setRoute}
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
              onSelect={setRoute}
              onOpenLauncher={() => setLaunchOpen(true)}
              onToggleTheme={toggleTheme}
              onCloseMobile={() => setDrawerOpen(false)}
              onToast={toast}
            />
          </div>
        </div>
      )}

      <main className="flex-1 flex min-w-0 min-h-0">
        {route.view === "corgi" && (
          <CorgiChatView
            data={superView}
            machines={machines}
            thread={route.thread}
            onSelectWorker={(paneId) => setRoute({ view: "agent", paneId })}
            onOpenFile={(p, m) => setFileTarget({ path: p, machine: m })}
            onToast={toast}
          />
        )}
        {route.view === "agent" && selectedPane && (
          <AgentPaneView
            pane={selectedPane}
            onClosed={() => setRoute({ view: "corgi", thread: "chat:main" })}
            onOpenFile={(p) => setFileTarget({ path: p, machine: selectedPane.machine })}
            onToast={toast}
          />
        )}
        {route.view === "auto" && <AutomationView onToast={toast} />}
        <FileDrawer target={fileTarget} onClose={() => setFileTarget(null)} />
      </main>

      <LauncherModal
        open={launchOpen}
        machines={machines}
        onClose={() => setLaunchOpen(false)}
        onLaunched={(paneId) => setRoute({ view: "agent", paneId })}
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

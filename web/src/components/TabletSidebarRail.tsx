import { Menu, Moon, Plus, Settings as SettingsIcon, Sun } from "lucide-react";
import type { AgentView } from "../types/api";
import { CorgiMark, StatusDot } from "./CorgiMark";
import type { AppRoute } from "./Sidebar";

interface Props {
  route: AppRoute;
  pendingCount: number;
  agents: AgentView[];
  stateReady: boolean;
  dark: boolean;
  onExpand: (triggerEl: HTMLButtonElement) => void;
  onSelect: (r: AppRoute) => void;
  onOpenLauncher: () => void;
  onToggleTheme: () => void;
}

export function TabletSidebarRail({
  route,
  pendingCount,
  agents,
  stateReady,
  dark,
  onExpand,
  onSelect,
  onOpenLauncher,
  onToggleTheme,
}: Props) {
  const blockedCount = agents.filter((a) => a.status === "blocked").length;
  const activeCount = agents.filter(
    (a) => a.status === "working" || a.status === "starting" || a.status === "waiting"
  ).length;

  return (
    <aside
      aria-label="平板快捷侧栏"
      className="hidden md:flex lg:hidden w-[54px] shrink-0 flex-col items-center py-3 justify-between surface-rail border-r border-hairline select-none"
    >
      <div className="flex flex-col items-center gap-2">
        <button
          type="button"
          onClick={(e) => onExpand(e.currentTarget)}
          title="展开侧栏"
          aria-label="展开侧栏"
          className="w-9 h-9 rounded-xl flex items-center justify-center text-secondary surface-hover cursor-pointer"
        >
          <Menu className="w-4 h-4" />
        </button>

        <button
          type="button"
          onClick={() => onSelect({ view: "corgi", thread: "chat:main" })}
          title="和 Corgi 对话"
          aria-label="和 Corgi 对话"
          className={`relative w-9 h-9 rounded-xl flex items-center justify-center cursor-pointer ${
            route.view === "corgi" ? "surface-active" : "surface-hover"
          }`}
        >
          <CorgiMark className="w-4 h-4 text-primary" />
          {pendingCount > 0 && (
            <span
              className="absolute top-1.5 right-1.5 w-1.5 h-1.5 rounded-full"
              style={{ background: "var(--attn)" }}
            />
          )}
        </button>

        <button
          type="button"
          disabled={!stateReady}
          onClick={onOpenLauncher}
          title={stateReady ? "新建独立会话" : "正在同步机群状态…"}
          aria-label="新建独立会话"
          className="w-9 h-9 rounded-xl flex items-center justify-center text-secondary surface-hover cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed"
        >
          <Plus className="w-4 h-4" strokeWidth={1.7} />
        </button>

        {(blockedCount > 0 || activeCount > 0) && (
          <button
            type="button"
            onClick={(e) => onExpand(e.currentTarget)}
            title={blockedCount > 0 ? `${blockedCount} 个会话待确认` : `${activeCount} 个会话运行中`}
            className="mt-1 px-1.5 py-1 rounded-lg border border-hairline surface-bg surface-hover flex flex-col items-center gap-1 cursor-pointer"
          >
            <StatusDot status={blockedCount > 0 ? "blocked" : "working"} />
            <span className="font-mono-code text-[10px] text-secondary leading-none">
              {blockedCount > 0 ? blockedCount : activeCount}
            </span>
          </button>
        )}
      </div>

      <div className="flex flex-col items-center gap-1.5">
        <button
          type="button"
          onClick={onToggleTheme}
          title="切换深浅色"
          aria-label="切换深浅色"
          className="w-9 h-9 rounded-xl flex items-center justify-center text-secondary surface-hover cursor-pointer"
        >
          {dark ? <Sun className="w-4 h-4" /> : <Moon className="w-4 h-4" />}
        </button>
        <button
          type="button"
          onClick={() => onSelect({ view: "auto" })}
          title="自动化与设置"
          aria-label="自动化与设置"
          className={`w-9 h-9 rounded-xl flex items-center justify-center cursor-pointer ${
            route.view === "auto" ? "btn-accent" : "text-secondary surface-hover"
          }`}
        >
          <SettingsIcon className="w-4 h-4" strokeWidth={1.8} />
        </button>
      </div>
    </aside>
  );
}

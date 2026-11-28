import { ArrowLeft, GitBranch, Trash2 } from "lucide-react";
import { AgentGlyph } from "../../components/CorgiMark";
import { formatPaneHandle } from "../../lib/agentIdentity";
import type { AgentView } from "../../types/api";

interface Props {
  pane: AgentView;
  mode: "chat" | "screen" | "log";
  badge: string;
  contextPct: number;
  stateReady: boolean;
  onBackToCorgi: () => void;
  onSwitchMode: (mode: "chat" | "screen" | "log") => void;
  onCloseWorkspace: () => void;
}

export function AgentPaneHeader({
  pane,
  mode,
  badge,
  contextPct,
  stateReady,
  onBackToCorgi,
  onSwitchMode,
  onCloseWorkspace,
}: Props) {
  const handleInfo = formatPaneHandle(pane);
  const modeTabs = pane.is_agent ? (["chat", "screen", "log"] as const) : (["screen", "log"] as const);

  return (
    <header className="min-h-14 py-2 px-4 md:px-5 flex items-center justify-between border-b border-hairline shrink-0 gap-2">
      <div className="flex items-start gap-2.5 min-w-0">
        <div className="w-7 h-7 rounded-lg border border-hairline surface-rail flex items-center justify-center shrink-0 mt-0.5 text-secondary">
          <AgentGlyph kind={pane.kind} isAgent={pane.is_agent} className="w-4 h-4" />
        </div>
        <div className="min-w-0">
          <div className="flex items-center gap-1.5 flex-wrap">
            <span className="font-semibold text-[15px] truncate">{handleInfo.title}</span>
            {handleInfo.showHandle && (
              <span className="text-[11.5px] font-mono-code text-muted shrink-0">{handleInfo.handle}</span>
            )}
            <span className="direct-pill text-[10.5px] font-medium px-1.5 py-0.5 rounded-full shrink-0">{badge}</span>
            <span className="text-[11px] font-mono-code px-1.5 py-0.5 rounded border border-strong text-secondary shrink-0">
              {pane.is_agent ? pane.kind : "shell"}
            </span>
            {pane.worktree_branch && (
              <span
                title={pane.repo_root ? `独立 Worktree（源仓库：${pane.repo_root}）` : "独立 Git Worktree"}
                className="inline-flex items-center gap-1 text-[11px] font-mono-code px-1.5 py-0.5 rounded border border-hairline surface-rail text-secondary shrink-0"
              >
                <GitBranch className="w-3 h-3" />
                <span>{pane.worktree_branch}</span>
              </span>
            )}
            {contextPct > 0 && <span className="text-[11px] font-mono-code text-muted">上下文 {contextPct}%</span>}
          </div>
          <div className="text-[11.5px] font-mono-code text-muted truncate mt-0.5">
            {pane.machine ? `${pane.machine} · ` : ""}
            {pane.cwd}
          </div>
          {pane.constraint && (
            <div title={`本会话红线：${pane.constraint}`} className="text-[11px] text-attn truncate mt-0.5">
              红线：{pane.constraint}
            </div>
          )}
        </div>
      </div>
      <div className="flex items-center gap-2 shrink-0">
        <button
          type="button"
          onClick={onBackToCorgi}
          className="hidden md:inline-flex items-center gap-1 px-2.5 py-1 rounded-lg text-xs border border-strong surface-bg text-secondary surface-hover cursor-pointer shrink-0"
          title="返回 Corgi 主对话"
        >
          <ArrowLeft className="w-3.5 h-3.5" />
          <span>回 Corgi</span>
        </button>
        <div className="inline-flex p-0.5 rounded-lg text-xs surface-rail border border-hairline">
          {modeTabs.map((m) => (
            <button
              key={m}
              type="button"
              onClick={() => onSwitchMode(m)}
              className={`px-2.5 py-1 rounded-md cursor-pointer ${mode === m ? "surface-bg font-medium text-primary" : "text-secondary"}`}
            >
              {m === "chat" ? "对话" : m === "screen" ? "屏幕" : "日志"}
            </button>
          ))}
        </div>
        <button
          type="button"
          disabled={!stateReady}
          onClick={onCloseWorkspace}
          className="w-8 h-8 rounded-lg flex items-center justify-center text-muted surface-hover cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed"
          title={stateReady ? "关闭工作区" : "正在同步实时状态…"}
        >
          <Trash2 className="w-4 h-4" strokeWidth={1.7} />
        </button>
      </div>
    </header>
  );
}

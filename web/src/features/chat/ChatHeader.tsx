import { useState } from "react";
import { List, MoreHorizontal, Pause, Play, RotateCw, Search, Terminal } from "lucide-react";
import { StatusDot } from "../../components/CorgiMark";
import type { DerivedWorkStatus, NotebookMode } from "../../lib/workItems";
import type { FollowView, RouterView } from "../../types/api";

interface Props {
  title: string;
  isWeComGroup: boolean;
  router?: RouterView;
  paused?: boolean;
  follow?: FollowView;
  notebookCount?: number;
  notebookDot?: DerivedWorkStatus;
  notebookMode?: NotebookMode;
  onToggleNotebook?: () => void;
  onToggleSearch: () => void;
  onRestartRouter: () => void;
  onTogglePause: () => void;
  onOpenRouterPane: (paneId: string) => void;
}

export function ChatHeader({
  title,
  isWeComGroup,
  router,
  paused,
  follow,
  notebookCount = 0,
  notebookDot = "idle",
  notebookMode = "mini",
  onToggleNotebook,
  onToggleSearch,
  onRestartRouter,
  onTogglePause,
  onOpenRouterPane,
}: Props) {
  const [menuOpen, setMenuOpen] = useState(false);

  const statusLabel = paused
    ? "已暂停自动投递"
    : router?.dreaming
      ? "正在整理记忆（做梦）…"
      : router?.handover
        ? "正在交接记忆并切换会话…"
        : router?.status === "working"
          ? router.replied
            ? "已回复，在整理记忆…"
            : "在想…"
          : router?.alive
            ? "空闲"
            : "打盹中";

  return (
    <header className="h-14 px-5 flex items-center justify-between border-b border-hairline shrink-0">
      <div className="min-w-0">
        <div className="flex items-center gap-2">
          <span className="font-semibold text-[15px] truncate">{title}</span>
          {router?.kind && (
            <span className="text-[11px] font-mono-code px-1.5 py-0.5 rounded border border-strong text-secondary shrink-0">
              {router.mode === "api" || router.kind === "api" ? "API" : router.kind}
            </span>
          )}
          {isWeComGroup && (
            <span className="text-[11px] px-1.5 py-0.5 rounded surface-rail border border-hairline text-muted shrink-0">
              双向同步
            </span>
          )}
        </div>
        <div className="flex items-center gap-2 text-[11.5px] text-muted mt-0.5 flex-wrap">
          <StatusDot status={paused ? "idle" : router?.status ?? "asleep"} />
          <span>{statusLabel}</span>
          {router?.model && (
            <span>
              · {router.model}
              {router.mode === "api" && router.effort ? ` (${router.effort})` : ""}
            </span>
          )}
          {router && router.mode !== "api" && router.context ? (
            <span>
              · 上下文 {router.context}%{router.rotate_at > 0 ? `/${router.rotate_at}%` : ""}
            </span>
          ) : null}
          {follow && (
            <span className="truncate max-w-[260px]" title={follow.next_text}>
              · 下次唤醒 {new Date(follow.next).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
            </span>
          )}
        </div>
      </div>

      <div className="flex items-center gap-1.5 relative">
        {onToggleNotebook && (
          <>
            {/* Compact pill on mobile/narrow viewports */}
            <button
              type="button"
              onClick={onToggleNotebook}
              className={`lg:hidden inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full border text-[12px] font-medium cursor-pointer transition-colors ${
                notebookMode === "expanded"
                  ? "border-strong surface-active text-primary"
                  : "border-strong surface-bg text-primary surface-hover"
              }`}
              title="打开小本本"
            >
              <span>小本本</span>
              {notebookCount > 0 && <span className="font-mono-code text-[11px] text-secondary">{notebookCount}</span>}
              {notebookCount > 0 && (
                <span
                  className="w-1.5 h-1.5 rounded-full"
                  style={{
                    background:
                      notebookDot === "failed"
                        ? "var(--err)"
                        : notebookDot === "blocked"
                          ? "var(--attn)"
                          : notebookDot === "working"
                            ? "var(--ok)"
                            : notebookDot === "review"
                              ? "var(--accent)"
                              : "var(--fg-3)",
                  }}
                />
              )}
            </button>
            {/* Quiet icon toggle on wide desktop */}
            <button
              type="button"
              onClick={onToggleNotebook}
              className={`hidden lg:flex relative w-8 h-8 rounded-lg items-center justify-center cursor-pointer transition-colors ${
                notebookMode === "expanded"
                  ? "text-primary surface-active"
                  : "text-secondary surface-hover"
              }`}
              title="切换小本本（页边 / 展开 / 隐藏）"
            >
              <List className="w-4 h-4" strokeWidth={1.7} />
              {notebookMode === "hidden" && (notebookDot === "failed" || notebookDot === "blocked") && (
                <span
                  className="absolute top-1.5 right-1.5 w-1.5 h-1.5 rounded-full"
                  style={{ background: notebookDot === "failed" ? "var(--err)" : "var(--attn)" }}
                />
              )}
            </button>
          </>
        )}
        <button
          type="button"
          onClick={onToggleSearch}
          className="w-8 h-8 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
          title="搜索对话"
        >
          <Search className="w-4 h-4" strokeWidth={1.7} />
        </button>
        <button
          type="button"
          onClick={() => setMenuOpen((v) => !v)}
          className="w-8 h-8 rounded-lg flex items-center justify-center text-secondary surface-hover cursor-pointer"
          title="大脑控制菜单"
        >
          <MoreHorizontal className="w-4 h-4" strokeWidth={1.7} />
        </button>

        {menuOpen && (
          <>
            <div className="fixed inset-0 z-20" onClick={() => setMenuOpen(false)} />
            <div className="absolute right-0 top-9 z-30 w-44 py-1 rounded-xl card-floating text-xs">
              {router?.pane_id && (
                <button
                  type="button"
                  onClick={() => {
                    setMenuOpen(false);
                    onOpenRouterPane(router.pane_id!);
                  }}
                  className="w-full px-3 py-2 flex items-center gap-2 text-left text-primary surface-hover cursor-pointer"
                >
                  <Terminal className="w-3.5 h-3.5 text-muted" />
                  <span>查看大脑终端</span>
                </button>
              )}
              <button
                type="button"
                onClick={() => {
                  setMenuOpen(false);
                  onTogglePause();
                }}
                className="w-full px-3 py-2 flex items-center gap-2 text-left text-primary surface-hover cursor-pointer"
              >
                {paused ? <Play className="w-3.5 h-3.5 text-muted" /> : <Pause className="w-3.5 h-3.5 text-muted" />}
                <span>{paused ? "恢复自动投递" : "暂停自动投递"}</span>
              </button>
              <button
                type="button"
                onClick={() => {
                  setMenuOpen(false);
                  onRestartRouter();
                }}
                className="w-full px-3 py-2 flex items-center gap-2 text-left text-primary surface-hover cursor-pointer"
              >
                <RotateCw className="w-3.5 h-3.5 text-muted" />
                <span>{router?.mode === "api" ? "重置 API 大脑状态" : "重启大脑会话"}</span>
              </button>
            </div>
          </>
        )}
      </div>
    </header>
  );
}

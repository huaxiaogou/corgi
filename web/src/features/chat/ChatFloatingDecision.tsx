import { ArrowDown } from "lucide-react";
import { DecisionCard, type DecisionResolvePayload } from "../../components/DecisionCard";
import { api } from "../../lib/api";
import type { UnreadSummary } from "../../lib/unreadTracker";
import type { Action } from "../../types/api";

interface FloatingDecisionProps {
  action: Action | null;
  queuePos: number;
  queueTotal: number;
  totalPending: number;
  superReady: boolean;
  workerNames: string[];
  onSwitchQueue: (delta: number) => void;
  onExpandInbox: () => void;
  onOpenWorker: (name: string) => void;
  onOpenFile: (path: string) => void;
  onToast: (msg: string) => void;
}

export function ChatFloatingDecision({
  action,
  queuePos,
  queueTotal,
  totalPending,
  superReady,
  workerNames,
  onSwitchQueue,
  onExpandInbox,
  onOpenWorker,
  onOpenFile,
  onToast,
}: FloatingDecisionProps) {
  if (!action) return null;
  return (
    <div className="pl-0 sm:pl-10 space-y-2">
      <DecisionCard
        action={action}
        disabled={!superReady}
        workerNames={workerNames}
        queuePos={queuePos}
        queueTotal={queueTotal}
        onSwitchQueue={onSwitchQueue}
        onResolve={async (id, payload: DecisionResolvePayload) => {
          const body: Record<string, unknown> = { by: "web" };
          if (payload.choices && payload.choices.length > 0) {
            body.choices = payload.choices;
          } else {
            body.choice = payload.choice ?? "";
          }
          if (payload.custom) body.custom = true;
          await api(`/api/super/actions/${encodeURIComponent(id)}/resolve`, body);
          onToast(payload.custom ? "已提交自定义回答并同步" : "已拍板并同步");
        }}
        onSnooze={async (id, minutes) => {
          await api(`/api/super/actions/${encodeURIComponent(id)}/snooze`, {
            by: "web",
            ...(minutes ? { minutes } : {}),
          });
          onToast(minutes ? `已暂缓 ${minutes} 分钟后提醒` : "已暂缓至明天晨报再次提醒");
        }}
        onDismiss={async (id) => {
          await api(`/api/super/actions/${encodeURIComponent(id)}/dismiss`, { by: "web" });
          onToast("已忽略待办");
        }}
        onOpenWorker={onOpenWorker}
        onOpenFile={onOpenFile}
      />
      {queueTotal > 1 && (
        <div className="flex items-center justify-between px-4 py-2 rounded-xl border border-hairline surface-rail text-xs">
          <span className="text-secondary">
            还有 <strong>{queueTotal - 1}</strong> 项待确认已折叠在顶部收件箱
          </span>
          <button
            type="button"
            onClick={onExpandInbox}
            className="font-medium text-primary hover:underline cursor-pointer"
          >
            展开全部 {totalPending} 项 →
          </button>
        </div>
      )}
    </div>
  );
}

interface JumpPillProps {
  unreadSummary: UnreadSummary;
  onJumpToFirstUnread: () => void;
  onJumpToBottom: () => void;
}

export function ChatUnreadJumpPill({ unreadSummary, onJumpToFirstUnread, onJumpToBottom }: JumpPillProps) {
  if (unreadSummary.jumpStage === "unread") {
    return (
      <button
        type="button"
        onClick={onJumpToFirstUnread}
        title={
          unreadSummary.windowTruncated
            ? "统计范围为当前已加载窗口，点击跳到第一条未读消息"
            : "跳到第一条未读消息"
        }
        className="absolute bottom-3 left-1/2 -translate-x-1/2 inline-flex items-center gap-1.5 px-3 py-1 rounded-full text-xs card-floating border-strong text-primary surface-hover cursor-pointer"
      >
        <span className="w-1.5 h-1.5 rounded-full shrink-0" style={{ background: "var(--attn)" }} />
        <ArrowDown className="w-3.5 h-3.5 text-secondary" />
        <span>
          {unreadSummary.unreadCount}
          {unreadSummary.windowTruncated ? "+" : ""} 条未读
        </span>
      </button>
    );
  }
  if (unreadSummary.jumpStage === "bottom") {
    return (
      <button
        type="button"
        onClick={onJumpToBottom}
        aria-label="回到最新"
        title="回到最新"
        className="absolute bottom-3 left-1/2 -translate-x-1/2 w-8 h-8 inline-flex items-center justify-center rounded-full card-floating border-strong text-secondary hover:text-primary surface-hover cursor-pointer"
      >
        <ArrowDown className="w-4 h-4" />
      </button>
    );
  }
  return null;
}

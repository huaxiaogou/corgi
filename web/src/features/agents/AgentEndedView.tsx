import { ArrowLeft, MessageSquareOff } from "lucide-react";

interface Props {
  loading?: boolean;
  onBackToCorgi: () => void;
}

export function AgentEndedView({ loading = false, onBackToCorgi }: Props) {
  if (loading) {
    return (
      <div className="flex-1 flex items-center justify-center p-6 text-xs text-muted">
        正在加载会话状态…
      </div>
    );
  }

  return (
    <div className="flex-1 flex items-center justify-center p-6">
      <div className="card-floating rounded-2xl p-6 max-w-sm w-full text-center space-y-3.5">
        <div className="w-10 h-10 rounded-xl border border-hairline surface-rail flex items-center justify-center mx-auto text-muted">
          <MessageSquareOff className="w-5 h-5" strokeWidth={1.6} />
        </div>
        <div className="space-y-1">
          <h2 className="text-[15px] font-semibold text-primary">该会话已结束</h2>
          <p className="text-xs text-secondary leading-relaxed">
            对应终端已关闭或退出；如需继续上下文，可在侧栏底部「历史会话」中恢复。
          </p>
        </div>
        <div className="pt-1">
          <button
            type="button"
            onClick={onBackToCorgi}
            className="inline-flex items-center gap-1.5 px-4 py-2 rounded-xl text-xs font-medium btn-accent cursor-pointer"
          >
            <ArrowLeft className="w-3.5 h-3.5" />
            <span>回 Corgi</span>
          </button>
        </div>
      </div>
    </div>
  );
}

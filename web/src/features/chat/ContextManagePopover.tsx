import { useCallback, useEffect, useState } from "react";
import { Layers, Minimize2, RefreshCw, Trash2, X } from "lucide-react";
import { api } from "../../lib/api";
import type { RouterContextInspect, RouterContextPruneResult, RouterContextView } from "../../types/api";

interface Props {
  thread: string;
  context?: RouterContextView;
  disabled?: boolean;
  onToast?: (msg: string) => void;
}

function fmtTok(n: number): string {
  return n >= 1000 ? `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}K` : `${n}`;
}

export function ContextManagePopover({ thread, context, disabled = false, onToast }: Props) {
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const [busyKey, setBusyKey] = useState("");
  const [detail, setDetail] = useState<RouterContextInspect | null>(null);

  const summary = detail?.summary ?? context;
  const budget = summary?.budget_tokens || 80000;
  const used = Math.max(summary?.last_prompt_tokens ?? 0, summary?.est_tokens ?? 0);
  const pct = Math.min(100, Math.round((used / Math.max(budget, 1)) * 100));
  const cachePct =
    summary && (summary.last_prompt_tokens ?? 0) > 0 && (summary.last_cached_tokens ?? 0) > 0
      ? Math.round(((summary.last_cached_tokens ?? 0) * 100) / (summary.last_prompt_tokens ?? 1))
      : 0;

  const loadDetail = useCallback(async () => {
    setLoading(true);
    try {
      const qs = new URLSearchParams({ thread });
      setDetail(await api<RouterContextInspect>(`/api/super/context?${qs.toString()}`));
    } catch (err) {
      onToast?.(err instanceof Error ? err.message : "读取上下文详情失败");
    } finally {
      setLoading(false);
    }
  }, [thread, onToast]);

  useEffect(() => {
    if (open) void loadDetail();
  }, [open, loadDetail]);
  useEffect(() => {
    setDetail(null);
  }, [thread]);

  const runPrune = async (key: string, body: Record<string, unknown>) => {
    if (busyKey) return;
    setBusyKey(key);
    try {
      const res = await api<RouterContextPruneResult>("/api/super/context/prune", { thread, ...body });
      onToast?.(res.message);
      await loadDetail();
    } catch (err) {
      onToast?.(err instanceof Error ? err.message : "上下文操作失败");
    } finally {
      setBusyKey("");
    }
  };

  const workGroups = (detail?.groups ?? []).filter((g) => g.work_item_id);
  const recentTurns = [...(detail?.turns ?? [])].reverse().slice(0, 12);

  return (
    <div className="relative">
      <button
        type="button"
        disabled={disabled}
        onClick={() => setOpen((v) => !v)}
        className={`inline-flex items-center gap-1.5 px-2 py-1 rounded-lg border text-[11px] font-mono-code cursor-pointer transition-colors disabled:opacity-40 ${
          open ? "border-strong surface-active text-primary" : "border-hairline surface-bg text-secondary surface-hover"
        }`}
        title="查看或选择性抛弃大脑工作上下文"
      >
        <Layers className="w-3.5 h-3.5 text-muted shrink-0" strokeWidth={1.7} />
        <span className="hidden sm:inline">{summary?.compacting ? "压缩中…" : `${fmtTok(used)}/${fmtTok(budget)}`}</span>
        {cachePct > 0 && !summary?.compacting && <span className="hidden md:inline text-muted">· {cachePct}%</span>}
        {(pct >= 80 || summary?.compacting) && <span className="w-1.5 h-1.5 rounded-full" style={{ background: "var(--attn)" }} />}
      </button>

      {open && (
        <>
          <div className="fixed inset-0 z-20" onClick={() => setOpen(false)} />
          <div className="absolute right-0 top-9 z-30 w-[330px] sm:w-[380px] max-h-[72vh] flex flex-col rounded-xl card-floating text-xs overflow-hidden">
            <div className="px-3.5 py-2.5 border-b border-hairline flex items-center justify-between gap-2">
              <div className="min-w-0">
                <div className="font-semibold text-primary">大脑工作上下文</div>
                <div className="text-[11px] font-mono-code text-muted mt-0.5">
                  Epoch {summary?.epoch ?? 1} · {summary?.active_turns ?? 0} 轮
                  {cachePct > 0 ? ` · KV 缓存 ${cachePct}%` : ""}
                  {(summary?.compaction_count ?? 0) > 0 ? ` · 已压缩 ${summary?.compaction_count} 次` : ""}
                </div>
              </div>
              <button
                type="button"
                onClick={() => void loadDetail()}
                disabled={loading}
                className="w-7 h-7 rounded-lg flex items-center justify-center text-muted surface-hover cursor-pointer"
                title="刷新"
              >
                <RefreshCw className={`w-3.5 h-3.5 ${loading ? "animate-spin" : ""}`} />
              </button>
            </div>

            <div className="px-3.5 py-2.5 border-b border-hairline space-y-1.5">
              <div className="flex items-center justify-between text-[11px] font-mono-code">
                <span className="text-secondary">{fmtTok(used)} / {fmtTok(budget)} tokens（{pct}%）</span>
                <span className="text-muted">{summary?.compacting ? "后台压缩中…" : "聊天记录不受影响"}</span>
              </div>
              <div className="h-1.5 rounded-full surface-rail overflow-hidden">
                <div className="h-full rounded-full transition-all" style={{ width: `${pct}%`, background: pct >= 85 ? "var(--attn)" : "var(--fg)" }} />
              </div>
            </div>

            <div className="flex-1 overflow-y-auto divide-y divide-[var(--line)]">
              {workGroups.length > 0 && (
                <div className="p-3 space-y-1.5">
                  <div className="text-[11px] font-medium text-muted">按事项抛弃上下文</div>
                  {workGroups.map((g) => (
                    <div key={g.work_item_id} className="flex items-center justify-between gap-2 px-2.5 py-1.5 rounded-lg surface-rail border border-hairline">
                      <div className="min-w-0">
                        <div className="text-primary truncate">
                          {g.title}
                          {g.closed && <span className="ml-1 text-[10.5px] text-muted">已关闭</span>}
                        </div>
                        <div className="text-[10.5px] font-mono-code text-muted">{g.turn_count} 轮 · ~{fmtTok(g.est_tokens)}</div>
                      </div>
                      <button
                        type="button"
                        disabled={Boolean(busyKey)}
                        onClick={() => void runPrune(`wi:${g.work_item_id}`, { mode: "work_item", work_item_id: g.work_item_id })}
                        className="px-2 py-1 rounded-md border border-strong surface-bg text-[11px] text-primary surface-hover shrink-0 cursor-pointer disabled:opacity-40"
                      >
                        {busyKey === `wi:${g.work_item_id}` ? "…" : "抛弃"}
                      </button>
                    </div>
                  ))}
                </div>
              )}

              <div className="p-3 space-y-1.5">
                <div className="text-[11px] font-medium text-muted">活跃轮次（{detail?.turns.length ?? summary?.active_turns ?? 0}）</div>
                {recentTurns.length === 0 ? (
                  <div className="text-[11.5px] text-muted py-3 text-center">
                    {loading ? "加载中…" : "当前工作上下文为空（新对话将自动建立缓存前缀）"}
                  </div>
                ) : (
                  recentTurns.map((t) => (
                    <div key={t.id} className="flex items-start justify-between gap-2 px-2.5 py-1.5 rounded-lg border border-hairline surface-bg">
                      <div className="min-w-0 space-y-0.5">
                        <div className="flex items-center gap-1.5 text-[10.5px] font-mono-code text-muted">
                          <span>{t.kind === "summary" ? "前情摘要" : `#${t.seq}`}</span>
                          <span>· ~{fmtTok(t.est_tokens)}</span>
                          {t.work_item_title && <span className="truncate max-w-[130px]">· {t.work_item_title}</span>}
                        </div>
                        <div className="text-[11.5px] text-primary truncate">{t.user_preview}</div>
                      </div>
                      <button
                        type="button"
                        disabled={Boolean(busyKey)}
                        onClick={() => void runPrune(`t:${t.id}`, { mode: "turns", turn_ids: [t.id] })}
                        className="w-6 h-6 rounded flex items-center justify-center text-muted hover:text-primary surface-hover shrink-0 cursor-pointer disabled:opacity-40"
                        title="抛弃这一轮上下文"
                      >
                        <X className="w-3.5 h-3.5" />
                      </button>
                    </div>
                  ))
                )}
              </div>
            </div>

            <div className="px-3 py-2.5 border-t border-hairline flex items-center justify-between gap-2 surface-rail">
              <button
                type="button"
                disabled={Boolean(busyKey) || Boolean(summary?.compacting) || (summary?.active_turns ?? 0) <= 1}
                onClick={() => void runPrune("compact", { mode: "compact" })}
                className="flex-1 inline-flex items-center justify-center gap-1.5 px-2.5 py-1.5 rounded-lg border border-strong surface-bg text-primary surface-hover cursor-pointer disabled:opacity-40"
              >
                <Minimize2 className="w-3.5 h-3.5 text-muted" />
                <span>{busyKey === "compact" || summary?.compacting ? "压缩中…" : "立即压缩"}</span>
              </button>
              <button
                type="button"
                disabled={Boolean(busyKey)}
                onClick={() => void runPrune("clear", { mode: "clear" })}
                className="inline-flex items-center justify-center gap-1.5 px-2.5 py-1.5 rounded-lg border border-hairline surface-bg text-secondary hover:text-primary surface-hover cursor-pointer disabled:opacity-40"
                title="清空大脑活跃上下文并换代，不删除历史聊天记录"
              >
                <Trash2 className="w-3.5 h-3.5 text-muted" />
                <span>清空重开</span>
              </button>
            </div>
          </div>
        </>
      )}
    </div>
  );
}

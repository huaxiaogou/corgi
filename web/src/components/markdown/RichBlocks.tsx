import type { ReactNode } from "react";
import { AlertOctagon, AlertTriangle, Check, Info, Lightbulb, Sparkles } from "lucide-react";
import type { CalloutKind, MetricItem, StepItem } from "./parseBlocks";

const CALLOUT_META: Record<CalloutKind, { label: string; colorVar: string }> = {
  note: { label: "注意", colorVar: "var(--co-note)" },
  tip: { label: "提示", colorVar: "var(--co-tip)" },
  important: { label: "重要", colorVar: "var(--co-important)" },
  warning: { label: "警告", colorVar: "var(--co-warning)" },
  caution: { label: "警示", colorVar: "var(--co-caution)" },
};

function CalloutIcon({ kind }: { kind: CalloutKind }) {
  const cls = "w-3.5 h-3.5 shrink-0";
  if (kind === "tip") return <Lightbulb className={cls} strokeWidth={1.8} />;
  if (kind === "important") return <Sparkles className={cls} strokeWidth={1.8} />;
  if (kind === "warning") return <AlertTriangle className={cls} strokeWidth={1.8} />;
  if (kind === "caution") return <AlertOctagon className={cls} strokeWidth={1.8} />;
  return <Info className={cls} strokeWidth={1.8} />;
}

export function CalloutBlock({
  kind,
  title,
  children,
}: {
  kind: CalloutKind;
  title?: string;
  children?: ReactNode;
}) {
  const meta = CALLOUT_META[kind];
  return (
    <div
      data-callout={kind}
      className="my-2.5 pl-3.5 py-1 space-y-1"
      style={{ borderLeft: `2px solid ${meta.colorVar}` }}
    >
      <div className="flex items-center gap-1.5 text-xs font-semibold" style={{ color: meta.colorVar }}>
        <CalloutIcon kind={kind} />
        <span>{title || meta.label}</span>
      </div>
      {children && <div className="text-[13.5px] text-secondary space-y-1">{children}</div>}
    </div>
  );
}

export function MetricsStrip({ items }: { items: MetricItem[] }) {
  return (
    <div
      data-metrics-strip="true"
      className="my-2.5 py-2 border-y border-hairline grid grid-cols-2 sm:grid-cols-3 gap-x-4 gap-y-2.5"
    >
      {items.map((it, idx) => (
        <div key={idx} className="min-w-0">
          <div className="text-[11px] text-muted truncate">{it.label}</div>
          <div className="flex items-baseline gap-1.5 flex-wrap">
            <span className="text-[16px] font-semibold text-primary tabular-nums font-mono-code">
              {it.value}
            </span>
            {it.delta && (
              <span
                className="text-[11px] font-mono-code tabular-nums"
                style={{
                  color: it.delta.startsWith("+")
                    ? "var(--diff-add-fg)"
                    : it.delta.startsWith("-")
                      ? "var(--fg-2)"
                      : "var(--fg-3)",
                }}
              >
                {it.delta}
              </span>
            )}
          </div>
        </div>
      ))}
    </div>
  );
}

export function StepsTimeline({
  items,
  renderText,
}: {
  items: StepItem[];
  renderText: (text: string) => ReactNode;
}) {
  return (
    <ol data-steps-timeline="true" className="my-2.5 space-y-2 pl-1">
      {items.map((it, idx) => {
        const isLast = idx === items.length - 1;
        return (
          <li key={idx} data-step-status={it.status} className="relative pl-6 text-[13.5px] leading-relaxed">
            {!isLast && (
              <span
                aria-hidden="true"
                className="absolute left-[7px] top-4 bottom-[-10px] w-px"
                style={{ background: "var(--line-2)" }}
              />
            )}
            <span
              aria-hidden="true"
              className="absolute left-0 top-[5px] w-4 h-4 rounded-full flex items-center justify-center border surface-bg"
              style={{
                borderColor:
                  it.status === "done"
                    ? "var(--ok)"
                    : it.status === "warn"
                      ? "var(--attn)"
                      : it.status === "active"
                        ? "var(--fg)"
                        : "var(--line-2)",
                color:
                  it.status === "done"
                    ? "var(--ok)"
                    : it.status === "warn"
                      ? "var(--attn)"
                      : "var(--fg)",
              }}
            >
              {it.status === "done" ? (
                <Check className="w-2.5 h-2.5" strokeWidth={2.5} />
              ) : it.status === "active" ? (
                <span className="w-1.5 h-1.5 rounded-full" style={{ background: "var(--fg)" }} />
              ) : it.status === "warn" ? (
                <span className="text-[9px] font-bold leading-none">!</span>
              ) : (
                <span className="w-1 h-1 rounded-full" style={{ background: "var(--fg-3)" }} />
              )}
            </span>
            <span className={it.status === "done" ? "text-secondary" : "text-primary"}>
              {renderText(it.text)}
            </span>
          </li>
        );
      })}
    </ol>
  );
}

export function TaskListItem({ checked, children }: { checked: boolean; children: ReactNode }) {
  return (
    <li className="list-none -ml-5 flex items-start gap-2 leading-[1.65]">
      <span
        aria-hidden="true"
        className="mt-[5px] w-3.5 h-3.5 rounded-[4px] border flex items-center justify-center shrink-0"
        style={{
          borderColor: checked ? "var(--fg)" : "var(--line-2)",
          background: checked ? "var(--fg)" : "transparent",
          color: "var(--bg)",
        }}
      >
        {checked && <Check className="w-2.5 h-2.5" strokeWidth={2.8} />}
      </span>
      <span className={checked ? "text-muted line-through" : "text-primary"}>{children}</span>
    </li>
  );
}

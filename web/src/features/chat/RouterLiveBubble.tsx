import { useEffect, useState } from "react";
import { StatusDot } from "../../components/CorgiMark";
import { MarkdownText } from "../../components/MarkdownText";
import type { RouterView } from "../../types/api";

interface Props {
  router?: RouterView;
  thread: string;
  visible: boolean;
}

export function RouterLiveBubble({ router, thread, visible }: Props) {
  const working = router?.status === "working";
  const live = router?.live;
  const threadMatches = !live?.thread || live.thread === thread;
  const activeLive = working && threadMatches ? live : undefined;
  const progress = activeLive?.progress ?? [];
  const draftText = activeLive?.draft_text ?? "";
  const subject = activeLive?.subject?.trim() ?? "";
  const thinkingSince = activeLive?.thinking_since;

  const [elapsedSec, setElapsedSec] = useState(0);

  useEffect(() => {
    if (!working) {
      setElapsedSec(0);
      return;
    }
    const baseMs = thinkingSince ? Date.parse(thinkingSince) : Date.now();
    const update = () => {
      const diff = Math.max(0, Math.floor((Date.now() - (Number.isFinite(baseMs) ? baseMs : Date.now())) / 1000));
      setElapsedSec(diff);
    };
    update();
    const id = window.setInterval(update, 1000);
    return () => window.clearInterval(id);
  }, [working, thinkingSince]);

  if (!visible) return null;

  if (!working) {
    return <div aria-hidden="true" className="mt-3 h-6 pointer-events-none select-none" />;
  }

  const label = draftText
    ? subject
      ? `Corgi 正在整理${subject}回复…`
      : "Corgi 正在回复…"
    : subject
      ? elapsedSec >= 2
        ? `Corgi 正在处理${subject} · ${elapsedSec}s`
        : `Corgi 正在处理${subject}…`
      : elapsedSec >= 2
        ? `Corgi 思考中 · ${elapsedSec}s`
        : "Corgi 在想…";

  return (
    <div aria-live="polite" className="mt-3 space-y-2">
      <div className="min-h-6 flex items-center gap-2.5 text-xs text-muted">
        <StatusDot status="working" />
        <span>{label}</span>
      </div>

      {progress.length > 0 && (
        <div className="pl-2.5 border-l border-strong space-y-0.5 text-[11.5px] font-mono-code text-muted">
          {progress.map((line, i) => (
            <div key={`${i}-${line}`} className="truncate">
              {line}
            </div>
          ))}
        </div>
      )}

      {draftText && (
        <div className="rounded-2xl border border-hairline surface-bg px-4 py-3 text-[14px] leading-relaxed text-primary">
          <MarkdownText text={draftText} />
          <span
            aria-hidden="true"
            className="inline-block w-1.5 h-3.5 ml-1 align-middle rounded-xs animate-pulse"
            style={{ background: "var(--fg-3)" }}
          />
        </div>
      )}
    </div>
  );
}

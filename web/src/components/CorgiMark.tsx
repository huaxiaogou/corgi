import { Terminal } from "lucide-react";

export function CorgiMark({ className = "w-4 h-4" }: { className?: string }) {
  return (
    <svg
      className={className}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.65"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M5.5 10.5 4.2 4.2 10 7.6a6.2 6.2 0 0 1 4 0l5.8-3.4-1.3 6.3a6.8 6.8 0 0 1-13 0Z" />
      <path d="M9.4 11.4v.4M14.6 11.4v.4" />
      <path d="M10.8 14.3c.7.6 1.7.6 2.4 0" />
    </svg>
  );
}

export function AgentGlyph({ kind, isAgent = true, className = "w-4 h-4" }: { kind: string; isAgent?: boolean; className?: string }) {
  if (!isAgent) return <Terminal className={className} strokeWidth={1.6} />;
  if (kind === "claude") {
    return (
      <svg className={className} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.65">
        <path d="M12 4v16M4 12h16M6.3 6.3l11.4 11.4M17.7 6.3 6.3 17.7" />
      </svg>
    );
  }
  if (kind === "codex") {
    return (
      <svg className={className} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.65">
        <polygon points="12 3 20 7.5 20 16.5 12 21 4 16.5 4 7.5 12 3" />
      </svg>
    );
  }
  if (kind === "codebuddy") {
    return (
      <svg className={className} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.65" strokeLinecap="round" strokeLinejoin="round">
        <path d="M8.5 7 3.5 12l5 5" />
        <path d="M15.5 7 20.5 12l-5 5" />
        <path d="M13.2 5.5 10.8 18.5" />
      </svg>
    );
  }
  return (
    <svg className={className} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.65">
      <ellipse cx="12" cy="12" rx="8" ry="3.8" transform="rotate(-30 12 12)" />
      <path d="M12 16V8M9.5 10.5 12 8l2.5 2.5" />
    </svg>
  );
}

export function StatusDot({ status }: { status: string }) {
  if (status === "working" || status === "starting") {
    return (
      <svg className="w-3.5 h-3.5 animate-spin shrink-0 text-secondary" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
        <path d="M21 12a9 9 0 1 1-6.2-8.6" />
      </svg>
    );
  }
  const color =
    status === "blocked"
      ? "var(--attn)"
      : status === "done"
        ? "var(--fg)"
        : status === "online"
          ? "var(--ok)"
          : "var(--fg-3)";
  return <span className="w-2 h-2 rounded-full shrink-0 inline-block" style={{ background: color }} />;
}

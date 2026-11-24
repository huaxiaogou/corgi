import { Terminal } from "lucide-react";

export function CorgiMark({ className = "w-4 h-4" }: { className?: string }) {
  return (
    <svg
      className={className}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      {/* Iconic Corgi fur mask (ears + eye patches framing the white blaze & cheeks) */}
      <path
        d="M10.7 8.2 C9.5 8.2 8.9 7.8 8.3 7.0 C6.3 4.3 4.0 2.7 2.8 3.6 C1.7 4.5 2.4 8.2 4.3 11.4 C3.4 12.6 3.0 13.9 3.3 15.1 C5.3 14.9 7.2 14.4 8.6 13.5 C10.0 12.6 10.7 10.8 10.7 8.2 Z"
        fill="currentColor"
        fillOpacity="0.16"
        stroke="none"
      />
      <path
        d="M13.3 8.2 C14.5 8.2 15.1 7.8 15.7 7.0 C17.7 4.3 20.0 2.7 21.2 3.6 C22.3 4.5 21.6 8.2 19.7 11.4 C20.6 12.6 21.0 13.9 20.7 15.1 C18.7 14.9 16.8 14.4 15.4 13.5 C14.0 12.6 13.3 10.8 13.3 8.2 Z"
        fill="currentColor"
        fillOpacity="0.16"
        stroke="none"
      />
      {/* Wide rounded Corgi ears & fluffy cheek silhouette */}
      <path d="M8.8 8.3 C10.8 7.7 13.2 7.7 15.2 8.3 C17.4 4.8 19.8 2.8 21.1 3.6 C22.4 4.4 21.7 8.2 19.7 11.5 C21.0 13.0 21.3 14.8 20.4 16.4 C19.0 18.8 15.7 20.8 12.0 20.8 C8.3 20.8 5.0 18.8 3.6 16.4 C2.7 14.8 3.0 13.0 4.3 11.5 C2.3 8.2 1.6 4.4 2.9 3.6 C4.2 2.8 6.6 4.8 8.8 8.3 Z" />
      <circle cx="8.6" cy="12.2" r="1.0" fill="currentColor" stroke="none" />
      <circle cx="15.4" cy="12.2" r="1.0" fill="currentColor" stroke="none" />
      <path
        d="M10.4 14.7 C10.4 14.2 10.9 14.0 12.0 14.0 C13.1 14.0 13.6 14.2 13.6 14.7 C13.6 15.3 12.6 16.2 12.0 16.2 C11.4 16.2 10.4 15.3 10.4 14.7 Z"
        fill="currentColor"
        stroke="none"
      />
      <path d="M9.5 17.3 C10.2 18.3 11.3 18.2 12.0 17.1 C12.7 18.2 13.8 18.3 14.5 17.3" strokeWidth="1.4" />
    </svg>
  );
}

export function CorgiButtMark({ className = "w-7 h-7" }: { className?: string }) {
  return (
    <svg
      className={className}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      {/* Peeking Corgi ears */}
      <path d="M8.8 7.5 C7.3 4.3 5.3 2.7 4.2 3.4 C3.1 4.1 3.8 7.3 5.5 10.0" />
      <path d="M15.2 7.5 C16.7 4.3 18.7 2.7 19.8 3.4 C20.9 4.1 20.2 7.3 18.5 10.0" />
      {/* Upper coat saddle */}
      <path
        d="M5.4 10.1 C7.1 7.7 9.4 6.9 12.0 6.9 C14.6 6.9 16.9 7.7 18.6 10.1 C19.5 11.4 19.8 12.8 19.6 13.9 C17.5 12.4 15.0 12.0 12.0 13.0 C9.0 12.0 6.5 12.4 4.4 13.9 C4.2 12.8 4.5 11.4 5.4 10.1 Z"
        fill="currentColor"
        fillOpacity="0.16"
        stroke="none"
      />
      {/* Round peach butt contour */}
      <path d="M5.4 10.1 C7.1 7.7 9.4 6.9 12.0 6.9 C14.6 6.9 16.9 7.7 18.6 10.1 C20.3 12.4 20.2 15.8 18.2 17.7 C16.7 19.0 14.3 19.1 12.0 18.0 C9.7 19.1 7.3 19.0 5.8 17.7 C3.8 15.8 3.7 12.4 5.4 10.1 Z" />
      {/* Fluffy bobtail puff & cleft */}
      <path d="M10.7 10.9 C10.6 9.6 13.4 9.6 13.3 10.9 C13.3 11.7 12.7 12.2 12.0 12.2 C11.3 12.2 10.7 11.7 10.7 10.9 Z" strokeWidth="1.4" />
      <path d="M12.0 18.0 L12.0 14.2" strokeWidth="1.4" />
      {/* Stubby back paws */}
      <path d="M7.0 18.6 C6.8 20.1 7.7 20.9 8.9 20.7 C9.8 20.5 10.2 19.8 10.1 18.9" strokeWidth="1.5" />
      <path d="M17.0 18.6 C17.2 20.1 16.3 20.9 15.1 20.7 C14.2 20.5 13.8 19.8 13.9 18.9" strokeWidth="1.5" />
      <circle cx="8.5" cy="19.6" r="0.5" fill="currentColor" stroke="none" />
      <circle cx="15.5" cy="19.6" r="0.5" fill="currentColor" stroke="none" />
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

import { useEffect, useMemo, useRef } from "react";
import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";

interface Props {
  frame: string;
  onKey?: (key: string) => void;
  onText?: (text: string) => void;
  onToast?: (msg: string) => void;
}

const KEY_MAP: Record<string, string> = {
  "\x1b[A": "up",
  "\x1b[B": "down",
  "\x1b[C": "right",
  "\x1b[D": "left",
  "\r": "enter",
  "\x1b": "esc",
  "\t": "tab",
  "\x7f": "backspace",
  "\x03": "ctrl+c",
  "\x04": "ctrl+d",
  "\x0c": "ctrl+l",
};

// eslint-disable-next-line no-control-regex
const ANSI_RE = /\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g;
const URL_RE = /https?:\/\/[^\s"'<>()]+/g;

function extractURLs(frame: string): string[] {
  const plain = frame.replace(ANSI_RE, "");
  const matches = plain.match(URL_RE) ?? [];
  const seen = new Set<string>();
  const out: string[] = [];
  for (const raw of matches) {
    const clean = raw.replace(/[.,;:\]]+$/, "");
    if (!seen.has(clean)) {
      seen.add(clean);
      out.push(clean);
    }
  }
  return out.slice(-3);
}

function readThemeColors() {
  const cs = getComputedStyle(document.documentElement);
  const bg = cs.getPropertyValue("--bg-2").trim();
  const fg = cs.getPropertyValue("--fg").trim();
  const cursor = cs.getPropertyValue("--fg-2").trim();
  const selBg = cs.getPropertyValue("--accent").trim();
  const selFg = cs.getPropertyValue("--accent-fg").trim();
  return { background: bg, foreground: fg, cursor, selectionBackground: selBg, selectionForeground: selFg };
}

export function TerminalScreen({ frame, onKey, onText, onToast }: Props) {
  const hostRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const latestFrameRef = useRef(frame);
  const onKeyRef = useRef(onKey);
  const onTextRef = useRef(onText);
  const onToastRef = useRef(onToast);
  onKeyRef.current = onKey;
  onTextRef.current = onText;
  onToastRef.current = onToast;
  latestFrameRef.current = frame;

  const urls = useMemo(() => extractURLs(frame), [frame]);

  useEffect(() => {
    const el = hostRef.current;
    if (!el) return;
    const term = new Terminal({
      disableStdin: false,
      convertEol: true,
      scrollback: 1000,
      fontSize: 12.5,
      lineHeight: 1.35,
      fontFamily: '"JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, monospace',
      theme: readThemeColors(),
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(el);
    fit.fit();
    termRef.current = term;
    fitRef.current = fit;

    // Allow Cmd+C / Ctrl+Shift+C to copy selected terminal text instead of sending Ctrl+C.
    term.attachCustomKeyEventHandler((ev) => {
      const isCopy = (ev.metaKey || (ev.ctrlKey && ev.shiftKey)) && ev.key.toLowerCase() === "c";
      if (isCopy && term.hasSelection()) {
        if (ev.type === "keydown") {
          void navigator.clipboard.writeText(term.getSelection()).then(() => onToastRef.current?.("已复制选中内容"));
        }
        return false;
      }
      return true;
    });

    const selSub = term.onSelectionChange(() => {
      if (!term.hasSelection() && latestFrameRef.current) {
        term.reset();
        term.write(latestFrameRef.current);
      }
    });

    const dataSub = term.onData((data) => {
      const mapped = KEY_MAP[data];
      if (mapped) {
        onKeyRef.current?.(mapped);
        return;
      }
      if (data && !data.startsWith("\x1b")) {
        onTextRef.current?.(data);
      }
    });

    const ro = new ResizeObserver(() => {
      try {
        fit.fit();
      } catch {
        // ignore resize race on unmount
      }
    });
    ro.observe(el);

    const mo = new MutationObserver(() => {
      term.options.theme = readThemeColors();
    });
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });

    return () => {
      selSub.dispose();
      dataSub.dispose();
      mo.disconnect();
      ro.disconnect();
      term.dispose();
      termRef.current = null;
      fitRef.current = null;
    };
  }, []);

  useEffect(() => {
    const term = termRef.current;
    if (!term || term.hasSelection()) return;
    term.reset();
    if (frame) {
      term.write(frame);
    } else {
      term.write("等待终端输出…");
    }
    try {
      fitRef.current?.fit();
    } catch {
      // ignore
    }
  }, [frame]);

  const copyLink = async (u: string) => {
    try {
      await navigator.clipboard.writeText(u);
      onToast?.("已复制链接");
    } catch {
      onToast?.("复制失败");
    }
  };

  return (
    <div className="w-full h-full flex flex-col gap-2 min-h-[360px]">
      <div
        ref={hostRef}
        className="w-full flex-1 min-h-[320px] rounded-xl border border-hairline surface-rail p-2.5 overflow-hidden"
      />
      {urls.length > 0 && (
        <div className="flex flex-wrap items-center gap-2 text-xs px-1">
          {urls.map((u) => (
            <div key={u} className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-lg border border-hairline surface-rail max-w-full">
              <a href={u} target="_blank" rel="noreferrer" className="font-mono-code text-primary hover:underline truncate max-w-[420px]">
                {u}
              </a>
              <button type="button" onClick={() => void copyLink(u)} className="text-muted hover:underline cursor-pointer shrink-0">
                复制
              </button>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

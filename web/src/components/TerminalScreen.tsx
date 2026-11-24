import { useEffect, useRef } from "react";
import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";

interface Props {
  frame: string;
  onKey?: (key: string) => void;
  onText?: (text: string) => void;
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

function readThemeColors() {
  const cs = getComputedStyle(document.documentElement);
  const bg = cs.getPropertyValue("--bg-2").trim();
  const fg = cs.getPropertyValue("--fg").trim();
  const cursor = cs.getPropertyValue("--fg-2").trim();
  return { background: bg, foreground: fg, cursor };
}

export function TerminalScreen({ frame, onKey, onText }: Props) {
  const hostRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const onKeyRef = useRef(onKey);
  const onTextRef = useRef(onText);
  onKeyRef.current = onKey;
  onTextRef.current = onText;

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
    if (!term) return;
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

  return (
    <div
      ref={hostRef}
      className="w-full h-full min-h-[360px] rounded-xl border border-hairline surface-rail p-2.5 overflow-hidden"
    />
  );
}

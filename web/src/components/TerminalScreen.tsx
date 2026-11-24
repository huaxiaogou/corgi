import { useEffect, useRef } from "react";
import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";

interface Props {
  frame: string;
}

function readThemeColors() {
  const cs = getComputedStyle(document.documentElement);
  const bg = cs.getPropertyValue("--bg-2").trim();
  const fg = cs.getPropertyValue("--fg").trim();
  const cursor = cs.getPropertyValue("--fg-2").trim();
  return { background: bg, foreground: fg, cursor };
}

export function TerminalScreen({ frame }: Props) {
  const hostRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);

  useEffect(() => {
    const el = hostRef.current;
    if (!el) return;
    const term = new Terminal({
      disableStdin: true,
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

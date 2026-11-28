export interface MermaidTokenPalette {
  bg: string;
  bg2: string;
  bg3: string;
  line: string;
  line2: string;
  fg: string;
  fg2: string;
  fg3: string;
  sans: string;
  mono: string;
}

const FLOWCHART_LR_RE = /^(\s*(?:flowchart|graph)\s+)(LR|RL)(\b.*)$/i;

/**
 * Rewrites horizontal flowchart/graph declarations (LR/RL) to vertical (TD) on narrow mobile screens
 * while leaving sequenceDiagram, stateDiagram, classDiagram, ER diagrams, and comments untouched.
 */
export function rewriteMermaidDirectionForMobile(code: string, isMobile: boolean): string {
  if (!isMobile || !code) return code;
  const lines = code.replace(/\r\n/g, "\n").split("\n");
  for (let i = 0; i < lines.length; i++) {
    const trimmed = lines[i].trim();
    if (!trimmed || trimmed.startsWith("%%")) continue;
    const m = lines[i].match(FLOWCHART_LR_RE);
    if (m) {
      lines[i] = `${m[1]}TD${m[3]}`;
      return lines.join("\n");
    }
    return code;
  }
  return code;
}

export function detectMermaidDiagramKind(code: string): string {
  const lines = code.replace(/\r\n/g, "\n").split("\n");
  for (const raw of lines) {
    const t = raw.trim();
    if (!t || t.startsWith("%%")) continue;
    if (/^(flowchart|graph)\b/i.test(t)) return "FLOWCHART";
    if (/^sequenceDiagram\b/i.test(t)) return "SEQUENCE";
    if (/^stateDiagram(?:-v2)?\b/i.test(t)) return "STATE";
    if (/^classDiagram\b/i.test(t)) return "CLASS";
    if (/^erDiagram\b/i.test(t)) return "ER";
    if (/^gantt\b/i.test(t)) return "GANTT";
    if (/^pie\b/i.test(t)) return "PIE";
    if (/^mindmap\b/i.test(t)) return "MINDMAP";
    if (/^timeline\b/i.test(t)) return "TIMELINE";
    return "DIAGRAM";
  }
  return "DIAGRAM";
}

export function readMermaidTokenPalette(
  getVar?: (name: string) => string
): MermaidTokenPalette {
  const read = (name: string): string => {
    if (getVar) return getVar(name).trim();
    if (typeof window === "undefined" || typeof document === "undefined") return "";
    return window.getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  };
  return {
    bg: read("--bg"),
    bg2: read("--bg-2"),
    bg3: read("--bg-3"),
    line: read("--line"),
    line2: read("--line-2"),
    fg: read("--fg"),
    fg2: read("--fg-2"),
    fg3: read("--fg-3"),
    sans: read("--sans") || "sans-serif",
    mono: read("--mono") || "monospace",
  };
}

export function buildMermaidInitConfig(palette: MermaidTokenPalette) {
  const rawThemeVars: Record<string, string> = {
    background: palette.bg,
    primaryColor: palette.bg2,
    primaryTextColor: palette.fg,
    primaryBorderColor: palette.line2,
    secondaryColor: palette.bg3,
    secondaryTextColor: palette.fg,
    secondaryBorderColor: palette.line2,
    tertiaryColor: palette.bg,
    tertiaryTextColor: palette.fg2,
    tertiaryBorderColor: palette.line,
    lineColor: palette.fg2,
    textColor: palette.fg,
    mainBkg: palette.bg2,
    nodeBorder: palette.line2,
    clusterBkg: palette.bg,
    clusterBorder: palette.line2,
    titleColor: palette.fg,
    edgeLabelBackground: palette.bg,
    actorBkg: palette.bg2,
    actorBorder: palette.line2,
    actorTextColor: palette.fg,
    actorLineColor: palette.line2,
    signalColor: palette.fg,
    signalTextColor: palette.fg,
    labelBoxBkgColor: palette.bg2,
    labelBoxBorderColor: palette.line2,
    labelTextColor: palette.fg,
    loopTextColor: palette.fg2,
    noteBkgColor: palette.bg3,
    noteBorderColor: palette.line2,
    noteTextColor: palette.fg,
    stateBkg: palette.bg2,
    stateLabelColor: palette.fg,
    transitionColor: palette.fg2,
    transitionLabelColor: palette.fg,
    fontFamily: palette.sans,
  };
  const themeVariables: Record<string, string> = {};
  for (const [k, v] of Object.entries(rawThemeVars)) {
    if (v && v.trim()) {
      themeVariables[k] = v.trim();
    }
  }
  return {
    startOnLoad: false,
    securityLevel: "strict" as const,
    theme: "base" as const,
    fontFamily: palette.sans,
    flowchart: {
      htmlLabels: false,
      useMaxWidth: true,
      curve: "basis" as const,
    },
    sequence: {
      useMaxWidth: true,
    },
    state: {
      useMaxWidth: true,
    },
    themeVariables,
  };
}

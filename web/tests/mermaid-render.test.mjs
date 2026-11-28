import assert from "node:assert/strict";
import { readFile, rm } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";
import { build } from "esbuild";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";

const root = path.resolve(import.meta.dirname, "..");

async function loadModule(entryRel) {
  const outFile = path.join(root, "tests", `.tmp-${path.basename(entryRel, path.extname(entryRel))}-${process.pid}.mjs`);
  await build({
    entryPoints: [path.join(root, entryRel)],
    outfile: outFile,
    bundle: true,
    format: "esm",
    jsx: "automatic",
    external: ["react", "react/jsx-runtime", "react-dom", "react-dom/server", "lucide-react", "mermaid"],
  });
  try {
    return await import(`${pathToFileURL(outFile).href}?t=${Date.now()}`);
  } finally {
    await rm(outFile, { force: true });
  }
}

test("rewriteMermaidDirectionForMobile rewrites flowchart/graph LR->TD on mobile only and leaves other diagrams intact", async () => {
  const { rewriteMermaidDirectionForMobile, detectMermaidDiagramKind } = await loadModule("src/lib/mermaidConfig.ts");

  const flow = "%% comment\nflowchart LR\n  A[Start] --> B[End]";
  assert.equal(rewriteMermaidDirectionForMobile(flow, false), flow);
  assert.equal(
    rewriteMermaidDirectionForMobile(flow, true),
    "%% comment\nflowchart TD\n  A[Start] --> B[End]"
  );
  assert.equal(detectMermaidDiagramKind(flow), "FLOWCHART");

  const graphRL = "graph RL\n  X --> Y";
  assert.equal(rewriteMermaidDirectionForMobile(graphRL, true), "graph TD\n  X --> Y");

  const seq = "sequenceDiagram\n  User->>Corgi: 发送指令\n  Corgi-->>User: 返回结论";
  assert.equal(rewriteMermaidDirectionForMobile(seq, true), seq);
  assert.equal(detectMermaidDiagramKind(seq), "SEQUENCE");

  const state = "stateDiagram-v2\n  [*] --> Idle\n  Idle --> Working";
  assert.equal(rewriteMermaidDirectionForMobile(state, true), state);
  assert.equal(detectMermaidDiagramKind(state), "STATE");
});

test("buildMermaidInitConfig enforces htmlLabels=false, strict security, and zero hardcoded hex colors in source", async () => {
  const { buildMermaidInitConfig, readMermaidTokenPalette } = await loadModule("src/lib/mermaidConfig.ts");
  const mockTokens = {
    "--bg": "rgb(255, 255, 255)",
    "--bg-2": "rgb(245, 244, 240)",
    "--bg-3": "rgb(235, 234, 229)",
    "--line": "rgb(225, 224, 219)",
    "--line-2": "rgb(190, 189, 184)",
    "--fg": "rgb(20, 20, 19)",
    "--fg-2": "rgb(82, 82, 78)",
    "--fg-3": "rgb(138, 138, 132)",
    "--sans": "system-ui",
    "--mono": "ui-monospace",
  };
  const palette = readMermaidTokenPalette((k) => mockTokens[k] ?? "");
  const cfg = buildMermaidInitConfig(palette);

  assert.equal(cfg.startOnLoad, false);
  assert.equal(cfg.securityLevel, "strict");
  assert.equal(cfg.theme, "base");
  assert.equal(cfg.flowchart.htmlLabels, false);
  assert.equal(cfg.themeVariables.background, mockTokens["--bg"]);
  assert.equal(cfg.themeVariables.primaryTextColor, mockTokens["--fg"]);
  assert.equal(cfg.themeVariables.primaryBorderColor, mockTokens["--line-2"]);

  // Enforce zero hardcoded hex literals in Mermaid modules.
  const cfgSrc = await readFile(path.join(root, "src/lib/mermaidConfig.ts"), "utf8");
  const blockSrc = await readFile(path.join(root, "src/components/markdown/MermaidBlock.tsx"), "utf8");
  assert.doesNotMatch(cfgSrc, /#[0-9a-fA-F]{3,8}\b/, "mermaidConfig.ts must not hardcode hex colors");
  assert.doesNotMatch(blockSrc, /#[0-9a-fA-F]{3,8}\b/, "MermaidBlock.tsx must not hardcode hex colors");
});

test("MarkdownText renders mermaid blocks lazily and keeps html-card strictly non-executable (D5)", async () => {
  const { MarkdownText } = await loadModule("src/components/MarkdownText.tsx");
  const md = [
    "```mermaid",
    "flowchart LR",
    "  A[Hub] --> B[Node]",
    "```",
    "",
    "```html-card",
    "<script>alert(1)</script><div>unsafe</div>",
    "```",
  ].join("\n");

  const html = renderToStaticMarkup(React.createElement(MarkdownText, { text: md }));
  assert.match(html, /data-mermaid-block="true"/);
  assert.match(html, /MERMAID · FLOWCHART/);
  assert.doesNotMatch(html, /<iframe/i, "html-card must NOT mount an executable iframe (D5)");
  assert.doesNotMatch(html, /<script>alert\(1\)<\/script>/i, "html-card script tag must remain escaped code");
});

test("installed mermaid package registers and detects flowchart, sequenceDiagram, and stateDiagram-v2 cleanly", async () => {
  const { buildMermaidInitConfig, readMermaidTokenPalette } = await loadModule("src/lib/mermaidConfig.ts");
  const mermaidPkg = await import("mermaid");
  const mermaid = mermaidPkg.default;
  assert.equal(typeof mermaid.render, "function");
  assert.equal(typeof mermaid.parse, "function");
  assert.equal(typeof mermaid.detectType, "function");

  mermaid.initialize(buildMermaidInitConfig(readMermaidTokenPalette(() => "")));

  const samples = [
    { kind: "flowchart", want: "flowchart-v2", src: "flowchart TD\n  A[输入] --> B{校验}\n  B -->|通过| C[落盘]" },
    { kind: "sequenceDiagram", want: "sequence", src: "sequenceDiagram\n  Web->>Hub: POST /api/super/say\n  Hub-->>Web: 200 OK" },
    { kind: "stateDiagram-v2", want: "stateDiagram", src: "stateDiagram-v2\n  [*] --> Pending\n  Pending --> Resolved" },
  ];

  for (const s of samples) {
    const detected = mermaid.detectType(s.src);
    assert.equal(detected, s.want, `expected mermaid to detect ${s.want} for ${s.kind}`);
  }
});

import assert from "node:assert/strict";
import { rm } from "node:fs/promises";
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
    external: ["react", "react/jsx-runtime", "react-dom", "react-dom/server", "lucide-react"],
  });
  try {
    return await import(`${pathToFileURL(outFile).href}?t=${Date.now()}`);
  } finally {
    await rm(outFile, { force: true });
  }
}

test("ToolMessageItem renders SubagentCall micro-card with role, type, model, and prompt", async () => {
  const { ToolMessageItem } = await loadModule("src/features/agents/ToolMessageItem.tsx");

  const html = renderToStaticMarkup(
    React.createElement(ToolMessageItem, {
      message: {
        kind: "tool",
        name: "invoke_subagent",
        input: "Codebase Researcher · inspect H1",
        subagent: {
          type: "research",
          role: "Codebase Researcher",
          model: "pro",
          prompt: "Audit every LocateTranscript call site across Go packages.",
        },
      },
    })
  );

  assert.ok(html.includes("Subagent"), html);
  assert.ok(html.includes("Codebase Researcher"), html);
  assert.ok(html.includes(">research<"), html);
  assert.ok(html.includes(">pro<"), html);
  assert.ok(html.includes("Audit every LocateTranscript call site"), html);
});

test("ToolMessageItem renders structured detail and preserves output and error indicator", async () => {
  const { ToolMessageItem } = await loadModule("src/features/agents/ToolMessageItem.tsx");

  const html = renderToStaticMarkup(
    React.createElement(ToolMessageItem, {
      message: {
        kind: "tool",
        name: "run_command",
        input: "go test -race ./...",
        detail: "CommandLine: go test -race ./...\nWaitMs: 5000",
        output: "FAIL: TestSomething",
        error: true,
      },
    })
  );

  assert.ok(html.includes("run_command"), html);
  assert.ok(html.includes("CommandLine: go test -race ./..."), html);
  assert.ok(html.includes("WaitMs: 5000"), html);
  assert.ok(html.includes("FAIL: TestSomething"), html);
  assert.ok(html.includes("var(--attn)"), html);
});

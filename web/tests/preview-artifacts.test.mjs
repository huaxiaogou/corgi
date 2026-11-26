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

test("previewLink builds Hub artifact API and trailing-slash public share URLs cleanly", async () => {
  const { buildArtifactAPIURL, buildPublicPreviewURL, formatByteSize } = await loadModule("src/lib/previewLink.ts");

  assert.equal(buildArtifactAPIURL("art_abc123"), "/api/artifacts/art_abc123");
  assert.equal(buildArtifactAPIURL("art_abc123", "raw"), "/api/artifacts/art_abc123/raw");
  assert.equal(buildArtifactAPIURL("art_abc123", "download"), "/api/artifacts/art_abc123/download");
  assert.equal(buildPublicPreviewURL("art_abc123.sigXYZ"), "/p/art_abc123.sigXYZ/");

  assert.equal(formatByteSize(512), "512 B");
  assert.equal(formatByteSize(42 * 1024), "42.0 KB");
  assert.equal(formatByteSize(2 * 1024 * 1024), "2.00 MB");
});

test("MarkdownText makes backtick-wrapped deliverable paths clickable instead of dead code", async () => {
  const { MarkdownText } = await loadModule("src/components/MarkdownText.tsx");

  const html = renderToStaticMarkup(
    React.createElement(MarkdownText, {
      text: "原型路径：`/Users/demo/proto.html`，普通代码 `npm run build` 保持原样。",
      defaultMachine: "mac-node",
      onOpenFile: () => {},
    })
  );
  assert.match(html, /<button[^>]*>\/Users\/demo\/proto\.html<\/button>/);
  assert.match(html, /<code[^>]*>npm run build<\/code>/);
});

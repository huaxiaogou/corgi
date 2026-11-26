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

test("classifyEnterKey blocks submission during IME composition and Safari post-composition Enter", async () => {
  const { classifyEnterKey, createIMETracker } = await loadModule("src/lib/composerInput.ts");

  const tracker = createIMETracker();

  // 1. Plain Enter outside IME submits.
  assert.equal(classifyEnterKey({ key: "Enter", shiftKey: false, timeStamp: 100 }, tracker), "submit");

  // 2. Shift+Enter inserts newline (ignored).
  assert.equal(classifyEnterKey({ key: "Enter", shiftKey: true, timeStamp: 120 }, tracker), "ignore");

  // 3. Active IME composition via native isComposing / keyCode 229 / Process / tracker.composing does not submit.
  assert.equal(classifyEnterKey({ key: "Enter", shiftKey: false, isComposing: true, timeStamp: 200 }, tracker), "ignore");
  assert.equal(classifyEnterKey({ key: "Enter", shiftKey: false, keyCode: 229, timeStamp: 210 }, tracker), "ignore");
  assert.equal(classifyEnterKey({ key: "Process", shiftKey: false, timeStamp: 220 }, tracker), "ignore");

  tracker.composing = true;
  assert.equal(classifyEnterKey({ key: "Enter", shiftKey: false, timeStamp: 230 }, tracker), "ignore");

  // 4. Safari macOS fires compositionend synchronously right before the confirming Enter keydown.
  tracker.composing = false;
  tracker.lastCompositionEndAt = 300;
  assert.equal(classifyEnterKey({ key: "Enter", shiftKey: false, isComposing: false, timeStamp: 302 }, tracker), "swallow_safari_post_ime");

  // 5. Real Enter pressed after composition ended (>30ms later, e.g. user confirmed with Space then pressed Enter) submits cleanly.
  assert.equal(classifyEnterKey({ key: "Enter", shiftKey: false, isComposing: false, timeStamp: 420 }, tracker), "submit");
});

test("computeTextareaLayout auto-grows, enables internal scroll, keeps bottom caret visible, and preserves middle-edit scroll", async () => {
  const { computeTextareaLayout } = await loadModule("src/lib/composerInput.ts");

  // Single line stays compact with hidden overflow.
  assert.deepEqual(
    computeTextareaLayout({
      scrollHeight: 21,
      minHeight: 24,
      maxHeight: 180,
      prevScrollTop: 0,
      atEnd: true,
      collapsedForMeasure: true,
    }),
    { height: 24, overflowY: "hidden", scrollTop: 0 }
  );

  // Multi-line within maxHeight expands to fit all lines.
  assert.deepEqual(
    computeTextareaLayout({
      scrollHeight: 96,
      minHeight: 24,
      maxHeight: 180,
      prevScrollTop: 0,
      atEnd: true,
      collapsedForMeasure: true,
    }),
    { height: 96, overflowY: "hidden", scrollTop: 0 }
  );

  // Overflowing multi-line with caret at end clamps at maxHeight, enables auto scroll, and follows caret to bottom.
  assert.deepEqual(
    computeTextareaLayout({
      scrollHeight: 340,
      minHeight: 24,
      maxHeight: 180,
      prevScrollTop: 0,
      atEnd: true,
      collapsedForMeasure: true,
    }),
    { height: 180, overflowY: "auto", scrollTop: 340 }
  );

  // Editing in the middle after height collapse restores previous scrollTop instead of snapping to top.
  assert.deepEqual(
    computeTextareaLayout({
      scrollHeight: 340,
      minHeight: 24,
      maxHeight: 180,
      prevScrollTop: 110,
      atEnd: false,
      collapsedForMeasure: true,
    }),
    { height: 180, overflowY: "auto", scrollTop: 110 }
  );

  // Fast-path editing in the middle (without collapsing height) leaves native scrollTop untouched.
  assert.deepEqual(
    computeTextareaLayout({
      scrollHeight: 340,
      minHeight: 24,
      maxHeight: 180,
      prevScrollTop: 110,
      atEnd: false,
      collapsedForMeasure: false,
    }),
    { height: 180, overflowY: "auto", scrollTop: null }
  );
});

test("ComposerBar renders scrollable, auto-sizing textarea with overscroll containment", async () => {
  const { ComposerBar } = await loadModule("src/features/chat/ComposerBar.tsx");

  const html = renderToStaticMarkup(
    React.createElement(ComposerBar, {
      embedded: true,
      machines: [],
      replyTo: null,
      onClearReply: () => {},
      onSend: async () => {},
      onToast: () => {},
    })
  );

  assert.ok(html.includes("<textarea"), html);
  assert.ok(html.includes("min-h-[24px]"), html);
  assert.ok(html.includes("max-h-[180px]"), html);
  assert.ok(html.includes("overscroll-contain"), html);
  assert.ok(html.includes("leading-[1.5]"), html);
});

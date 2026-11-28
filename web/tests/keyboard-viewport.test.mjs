import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { computeKeyboardMetrics } from "../src/hooks/useKeyboardViewport.ts";

const root = path.resolve(import.meta.dirname, "..");

test("computeKeyboardMetrics isolates keyboard open/close, jitter, zoom, and offsetTop accurately", () => {
  // Normal keyboard closed
  assert.deepEqual(
    computeKeyboardMetrics({
      innerHeight: 844,
      viewportHeight: 844,
      offsetTop: 0,
      scale: 1,
      editorFocused: false,
    }),
    { kbInset: 0, kbTop: 0 }
  );

  // Small browser chrome bar change (<80px) must not trigger keyboard inset
  assert.deepEqual(
    computeKeyboardMetrics({
      innerHeight: 844,
      viewportHeight: 790,
      offsetTop: 0,
      scale: 1,
      editorFocused: true,
    }),
    { kbInset: 0, kbTop: 0 }
  );

  // Real iOS soft keyboard open while editor focused
  assert.deepEqual(
    computeKeyboardMetrics({
      innerHeight: 844,
      viewportHeight: 508,
      offsetTop: 12,
      scale: 1,
      editorFocused: true,
    }),
    { kbInset: 324, kbTop: 12 }
  );

  // 2px integer hysteresis prevents oscillation
  assert.deepEqual(
    computeKeyboardMetrics({
      innerHeight: 844,
      viewportHeight: 507,
      offsetTop: 12,
      scale: 1,
      editorFocused: true,
      prevInset: 324,
    }),
    { kbInset: 324, kbTop: 12 }
  );

  // Pinch-zoom (scale > 1.02) must return zero keyboard inset
  assert.deepEqual(
    computeKeyboardMetrics({
      innerHeight: 844,
      viewportHeight: 420,
      offsetTop: 40,
      scale: 1.5,
      editorFocused: true,
    }),
    { kbInset: 0, kbTop: 0 }
  );

  // Invalid / zero height guards
  assert.deepEqual(
    computeKeyboardMetrics({
      innerHeight: 0,
      viewportHeight: 0,
      offsetTop: -5,
      scale: 1,
      editorFocused: true,
    }),
    { kbInset: 0, kbTop: 0 }
  );
});

test("CSS static contract enforces zero backdrop-filter, zero :has(), and registers @property --kb-inset / --kb-top", async () => {
  const appCss = await readFile(path.join(root, "src/styles/app.css"), "utf8");
  const tokensCss = await readFile(path.join(root, "src/styles/tokens.css"), "utf8");
  const combined = `${appCss}\n${tokensCss}`;

  assert.doesNotMatch(combined, /backdrop-filter/i, "backdrop-filter is strictly forbidden by H7 performance guardrail");
  assert.doesNotMatch(combined, /:has\(/i, ":has() selector is strictly forbidden by H7 performance guardrail");
  assert.match(appCss, /@property\s+--kb-inset\b/, "app.css must register @property --kb-inset");
  assert.match(appCss, /@property\s+--kb-top\b/, "app.css must register @property --kb-top");
  assert.match(appCss, /inherits:\s*false/, "keyboard Houdini properties must set inherits: false");
});

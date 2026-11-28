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

test("parseWorkerUserEnvelope handles single event, constraint header, boilerplate stripping, and spill retention", async () => {
  const { parseWorkerUserEnvelope } = await loadModule("src/lib/workerEnvelope.ts");
  const raw = [
    "【本会话红线】禁止修改主分支，未获拍板前禁止 git push",
    "",
    "[corgi] 1 new event",
    "",
    "#t4462 · chat:main · router · from corgi · 21:27",
    "  [corgi task · worker=agy-4506 · thread=chat:main]",
    "  第一行：完成 H7 移动端护栏",
    "  第二行：验证键盘视口隔离",
    "  第三行：补齐平板覆盖侧栏",
    "  第四行：折叠 Worker 派发信封",
    "  第五行：保留完整任务索引 … [spilled; run `corgi show t4462`]",
    "  ",
    "  How this works:",
    "  - Report completion with `corgi report \"outcome summary & file paths\"`.",
    "  - If blocked on a decision only the human can make, report `BLOCKED: ...`.",
    "  - Git Worktree Isolation: you are working in an isolated git worktree on branch `corgi/agy-4506`.",
  ].join("\n");

  const parsed = parseWorkerUserEnvelope(raw);
  assert.equal(parsed.isEnvelope, true);
  assert.equal(parsed.batched, false);
  assert.equal(parsed.constraint, "禁止修改主分支，未获拍板前禁止 git push");
  assert.deepEqual(parsed.triggerIds, ["#t4462"]);
  assert.deepEqual(parsed.spilledIds, ["t4462"]);
  assert.equal(parsed.shouldFold, true);
  assert.equal(parsed.preview.split("\n").length, 4);
  assert.ok(!parsed.body.includes("How this works:"), "boilerplate footer must be stripped");
  assert.ok(!parsed.body.includes("Git Worktree Isolation:"), "worktree boilerplate must be stripped");
  assert.ok(parsed.body.includes("第一行：完成 H7 移动端护栏"));
});

test("parseWorkerUserEnvelope handles batched events and preserves all Trigger IDs", async () => {
  const { parseWorkerUserEnvelope } = await loadModule("src/lib/workerEnvelope.ts");
  const raw = [
    "[corgi] 2 new events (batched)",
    "",
    "#t4501 · chat:main · router · from corgi · 22:01",
    "  先跑后端竞态单测",
    "",
    "#t4502 · chat:main · router · from corgi · 22:02",
    "  再补前端样式契约断言",
  ].join("\n");

  const parsed = parseWorkerUserEnvelope(raw);
  assert.equal(parsed.isEnvelope, true);
  assert.equal(parsed.batched, true);
  assert.equal(parsed.eventCount, 2);
  assert.deepEqual(parsed.triggerIds, ["#t4501", "#t4502"]);
  assert.deepEqual(parsed.spilledIds, []);
  assert.ok(parsed.body.includes("先跑后端竞态单测"));
  assert.ok(parsed.body.includes("再补前端样式契约断言"));
});

test("parseWorkerUserEnvelope passes ordinary human messages through verbatim", async () => {
  const { parseWorkerUserEnvelope } = await loadModule("src/lib/workerEnvelope.ts");
  const human = "帮我查一下今天的日志里有没有 502 错误\n顺便看看内存占用";
  const parsed = parseWorkerUserEnvelope(human);
  assert.equal(parsed.isEnvelope, false);
  assert.equal(parsed.body, human);
  assert.equal(parsed.shouldFold, false);
});

test("WorkerEnvelopeBubble renders Trigger ID badge and spilled corgi show action even while folded", async () => {
  const { WorkerEnvelopeBubble } = await loadModule("src/components/WorkerEnvelopeBubble.tsx");
  const raw = [
    "【本会话红线】仅在隔离分支改动",
    "",
    "[corgi] 1 new event",
    "",
    "#t4564 · chat:main · router · from corgi · 22:05",
    "  行1",
    "  行2",
    "  行3",
    "  行4",
    "  行5 … [spilled; run `corgi show t4564`]",
  ].join("\n");

  const html = renderToStaticMarkup(React.createElement(WorkerEnvelopeBubble, { text: raw }));
  assert.match(html, /#t4564/);
  assert.match(html, /正文已截断/);
  assert.match(html, /corgi show t4564/);
  assert.match(html, /展开完整任务/);
  assert.match(html, /查看原始信封/);

  const plainHtml = renderToStaticMarkup(React.createElement(WorkerEnvelopeBubble, { text: "普通直聊消息" }));
  assert.match(plainHtml, /pill-user/);
  assert.doesNotMatch(plainHtml, /data-worker-envelope/);
});

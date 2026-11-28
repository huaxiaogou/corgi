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

const now = "2026-11-28T12:00:00Z";

test("partitionInPlaceReplies groups loaded targets in-place and falls back missing targets to timeline", async () => {
  const { partitionInPlaceReplies } = await loadModule("src/lib/inPlaceReply.ts");
  const entries = [
    { id: "e1", seq: 1, role: "corgi", thread: "chat:main", text: "早期方案分析", at: now },
    { id: "e2", seq: 2, role: "corgi", thread: "chat:main", text: "最新进度汇报", at: now },
    {
      id: "e3",
      seq: 3,
      role: "user",
      thread: "chat:main",
      text: "补充条件 A",
      at: now,
      in_place: true,
      re: { entry: "e1", who: "Corgi", text: "早期方案分析" },
    },
    {
      id: "e4",
      seq: 4,
      role: "user",
      thread: "chat:main",
      text: "补充条件 B",
      at: now,
      in_place: true,
      re: { entry: "e1", who: "Corgi", text: "早期方案分析" },
    },
    {
      id: "e5",
      seq: 5,
      role: "user",
      thread: "chat:main",
      text: "目标在更早未加载历史里的原位回复",
      at: now,
      in_place: true,
      re: { entry: "e0-unloaded", who: "Corgi", text: "更早消息" },
    },
    {
      id: "e6",
      seq: 6,
      role: "user",
      thread: "chat:main",
      text: "普通底部引用回复",
      at: now,
      in_place: false,
      re: { entry: "e1", who: "Corgi", text: "早期方案分析" },
    },
  ];

  const { timelineEntries, inPlaceByTarget } = partitionInPlaceReplies(entries);
  assert.deepEqual(
    timelineEntries.map((e) => e.id),
    ["e1", "e2", "e5", "e6"],
    "missing-target in_place reply e5 and non-in_place e6 must remain in main timeline"
  );
  assert.deepEqual(
    (inPlaceByTarget.get("e1") ?? []).map((e) => e.id),
    ["e3", "e4"],
    "loaded-target in_place replies must group in seq order under e1"
  );
});

test("InPlaceReplies preserves each reply DOM #entry-<id> and data-entry-id and reveals full content when highlighted", async () => {
  const { InPlaceReplies } = await loadModule("src/components/InPlaceReplies.tsx");
  const longText = "第一段补充说明。".repeat(30);
  const replies = [
    {
      id: "e33",
      seq: 33,
      role: "user",
      thread: "chat:main",
      text: "简短原位补充",
      at: now,
      in_place: true,
      re: { entry: "e10", who: "Corgi", text: "目标消息" },
    },
    {
      id: "e34",
      seq: 34,
      role: "user",
      thread: "chat:main",
      text: longText,
      at: now,
      in_place: true,
      action_id: "a9",
      re: { entry: "e10", who: "Corgi", text: "目标消息" },
    },
  ];

  const htmlDefault = renderToStaticMarkup(
    React.createElement(InPlaceReplies, {
      replies,
      resolvedActions: {
        a9: {
          id: "a9",
          kind: "router_choice",
          thread: "chat:main",
          title: "请选择",
          status: "resolved",
          created_at: now,
        },
      },
      highlightedEntryId: "",
    })
  );
  assert.match(htmlDefault, /id="entry-e33"/);
  assert.match(htmlDefault, /data-entry-id="e33"/);
  assert.match(htmlDefault, /id="entry-e34"/);
  assert.match(htmlDefault, /data-entry-id="e34"/);
  assert.match(htmlDefault, /自定义拍板（原位）/);
  assert.match(htmlDefault, /展开全文/);

  const htmlHighlighted = renderToStaticMarkup(
    React.createElement(InPlaceReplies, {
      replies,
      resolvedActions: {},
      highlightedEntryId: "e34",
    })
  );
  assert.match(htmlHighlighted, /entry-highlight/);
  assert.ok(htmlHighlighted.includes(longText), "highlighted in-place reply must expand full text for search/jump");
});

test("D6 breakpoint split and draft map helpers preserve and clear drafts accurately", async () => {
  const { hasDraftText, setDraftEntry, shouldUseInlineReplyBox } = await loadModule("src/lib/inPlaceReply.ts");
  assert.equal(shouldUseInlineReplyBox(767), false);
  assert.equal(shouldUseInlineReplyBox(768), true);
  assert.equal(shouldUseInlineReplyBox(1280), true);

  let drafts = {};
  drafts = setDraftEntry(drafts, "e1", "待补充细节");
  assert.equal(hasDraftText(drafts, "e1"), true);
  assert.equal(hasDraftText(drafts, "e2"), false);

  drafts = setDraftEntry(drafts, "e1", "   ");
  assert.equal(hasDraftText(drafts, "e1"), false);

  drafts = setDraftEntry(drafts, "e1", "");
  assert.equal("e1" in drafts, false);
});

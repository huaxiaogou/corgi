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

test("decisionCard helpers implement Scheme 3 fast/multi/fold rules and inline routing", async () => {
  const mod = await loadModule("src/lib/decisionCard.ts");
  const filter = await loadModule("src/lib/chatFilter.ts");

  const fastTwo = {
    id: "a1",
    kind: "router_choice",
    thread: "chat:main",
    title: "是否继续发布？",
    status: "pending",
    created_at: new Date().toISOString(),
    choices: [
      { key: "1", label: "继续" },
      { key: "2", label: "暂缓" },
    ],
  };
  assert.equal(mod.isFastSingleAction(fastTwo), true);
  assert.equal(filter.isInlineBubbleAction(fastTwo), true);

  const threeSingle = {
    ...fastTwo,
    id: "a2",
    choices: [
      { key: "1", label: "方案A" },
      { key: "2", label: "方案B" },
      { key: "3", label: "方案C" },
    ],
  };
  assert.equal(mod.isFastSingleAction(threeSingle), false);
  assert.equal(filter.isInlineBubbleAction(threeSingle), false);

  const twoMulti = { ...fastTwo, id: "a3", mode: "multi" };
  assert.equal(mod.isMultiAction(twoMulti), true);
  assert.equal(mod.isFastSingleAction(twoMulti), false);
  assert.equal(filter.isInlineBubbleAction(twoMulti), false);

  // Toggle selection behavior for single vs multi.
  assert.deepEqual(mod.toggleChoiceSelection(false, ["1"], "2"), ["2"]);
  assert.deepEqual(mod.toggleChoiceSelection(true, ["1"], "2"), ["1", "2"]);
  assert.deepEqual(mod.toggleChoiceSelection(true, ["1", "2"], "1"), ["2"]);

  // >4 folding keeps first 4 visible and counts hidden selections without nested scroll.
  const sixChoices = [1, 2, 3, 4, 5, 6].map((n) => ({ key: String(n), label: `选项${n}` }));
  const folded = mod.computeDecisionFold(sixChoices, false, ["2", "5", "6"]);
  assert.equal(folded.folded, true);
  assert.equal(folded.visible.length, 4);
  assert.equal(folded.hiddenCount, 2);
  assert.equal(folded.hiddenSelectedCount, 2);

  const expanded = mod.computeDecisionFold(sixChoices, true, ["2", "5", "6"]);
  assert.equal(expanded.folded, false);
  assert.equal(expanded.visible.length, 6);
});

test("DecisionCard renders Scheme 3 multi-select, >4 inline fold without nested scroll, and queue badge", async () => {
  const { DecisionCard } = await loadModule("src/components/DecisionCard.tsx");
  const action = {
    id: "a_six",
    kind: "router_choice",
    thread: "chat:main",
    title: "选择发布检查项",
    summary: "请勾选需要的检查项后确认",
    mode: "multi",
    status: "pending",
    created_at: new Date().toISOString(),
    choices: [
      { key: "1", label: "灰度验证", recommended: true },
      { key: "2", label: "数据库备份" },
      { key: "3", label: "告警静默" },
      { key: "4", label: "性能压测" },
      { key: "5", label: "回滚预案" },
      { key: "6", label: "值班同步" },
    ],
  };

  const html = renderToStaticMarkup(
    React.createElement(DecisionCard, {
      action,
      queuePos: 1,
      queueTotal: 3,
      onSwitchQueue: () => {},
      onResolve: async () => {},
      onSnooze: async () => {},
      onDismiss: async () => {},
    })
  );

  assert.ok(html.includes("1/3"), html);
  assert.ok(html.includes("勾选推荐"), html);
  assert.ok(html.includes("灰度验证"), html);
  assert.ok(html.includes("性能压测"), html);
  assert.ok(!html.includes("回滚预案"), "5th option should be folded initially");
  assert.ok(html.includes("+ 展开其余 2 个选项"), html);
  assert.ok(html.includes("请先选择上方选项"), html);
  assert.ok(html.includes("稍后处理"), html);
  assert.ok(!html.includes("overflow-y-auto"), "options list must not use nested vertical scroll");
});

test("DecisionReceiptCard displays multi-select choices and channel without racy undo button", async () => {
  const { DecisionReceiptCard } = await loadModule("src/components/DecisionReceiptCard.tsx");
  const nowIso = new Date().toISOString();

  const html = renderToStaticMarkup(
    React.createElement(DecisionReceiptCard, {
      action: {
        id: "a_done",
        kind: "router_choice",
        mode: "multi",
        thread: "chat:main",
        title: "检查项确认",
        status: "resolved",
        resolved_by: "wecom",
        resolved_channel: "wecom",
        chosen_key: "1,5",
        chosen_keys: ["1", "5"],
        chosen_label: "已选 2 项：灰度验证、回滚预案",
        created_at: nowIso,
        resolved_at: nowIso,
        choices: [
          { key: "1", label: "灰度验证" },
          { key: "2", label: "数据库备份" },
          { key: "5", label: "回滚预案" },
        ],
      },
      at: nowIso,
    })
  );

  assert.ok(html.includes("企业微信"), html);
  assert.ok(html.includes("多选"), html);
  assert.ok(html.includes("已选 2 项：灰度验证、回滚预案"), html);
  assert.ok(html.includes("查看其余 1 个备选"), html);
  assert.ok(!html.includes("撤销重选"), "receipt must not expose racy post-submit undo");
});

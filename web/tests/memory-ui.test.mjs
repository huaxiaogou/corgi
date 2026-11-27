import assert from "node:assert/strict";
import { rm } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";
import { build } from "esbuild";

const root = path.resolve(import.meta.dirname, "..");

async function loadModule(entryRel) {
  const outFile = path.join(root, "tests", `.tmp-${path.basename(entryRel, path.extname(entryRel))}-${process.pid}.mjs`);
  await build({
    entryPoints: [path.join(root, entryRel)],
    outfile: outFile,
    bundle: true,
    format: "esm",
  });
  try {
    return await import(`${pathToFileURL(outFile).href}?t=${Date.now()}`);
  } finally {
    await rm(outFile, { force: true });
  }
}

test("parseInstructions splits explicit vs inferred rules and removeInstructionLine preserves formatting", async () => {
  const { parseInstructions, removeInstructionLine } = await loadModule("src/lib/memoryHelpers.ts");

  const doc = [
    "# 主人的长期指令与偏好",
    "",
    "- 默认在独立 worktree 开发并提测",
    "- 偏好深色简洁排版 (inferred)",
    "- 多项进展用分点 Markdown 汇报",
    "- 喜欢先给总结再给细节（推断）",
    "",
  ].join("\n");

  const parsed = parseInstructions(doc);
  assert.equal(parsed.explicit.length, 2);
  assert.equal(parsed.inferred.length, 2);
  assert.equal(parsed.explicit[0].text, "默认在独立 worktree 开发并提测");
  assert.equal(parsed.inferred[0].text, "偏好深色简洁排版");
  assert.equal(parsed.inferred[0].lineIndex, 3);

  const afterRemove = removeInstructionLine(doc, 3);
  assert.ok(!afterRemove.includes("偏好深色简洁排版"));
  assert.ok(afterRemove.includes("默认在独立 worktree 开发并提测"));
  assert.ok(afterRemove.includes("喜欢先给总结再给细节（推断）"));
});

test("extractDreamSummary and searchMemoryFiles return concise previews", async () => {
  const { extractDreamSummary, searchMemoryFiles } = await loadModule("src/lib/memoryHelpers.ts");

  const journal = [
    "# 2026-11-27",
    "合并了重复规则并归档了 2 份已完成笔记。",
    "- 清理 instructions.md 冗余条目",
    "- 更新 notes/agents.md 机型可用性",
    "- 保持首屏预算在 3.2 KB",
    "- 第四条不展示在顶部摘要",
  ].join("\n");

  const summary = extractDreamSummary(journal);
  assert.equal(summary.lead, "合并了重复规则并归档了 2 份已完成笔记。");
  assert.deepEqual(summary.bullets, [
    "清理 instructions.md 冗余条目",
    "更新 notes/agents.md 机型可用性",
    "保持首屏预算在 3.2 KB",
  ]);

  const hits = searchMemoryFiles(
    [
      { name: "instructions.md", text: "- 用中文提交 commit\n- 使用 worktree", size: 40, mod: "" },
      { name: "notes/agents.md", text: "Mac 上 codex 稳定", size: 20, mod: "" },
    ],
    "worktree"
  );
  assert.equal(hits.length, 1);
  assert.equal(hits[0].name, "instructions.md");
  assert.equal(hits[0].matches[0].line, 2);
});

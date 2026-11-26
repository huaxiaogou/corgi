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

test("normalizeEscapedNewlines expands legacy single-line escapes safely", async () => {
  const { normalizeEscapedNewlines } = await loadModule("src/lib/text.ts");

  assert.equal(
    normalizeEscapedNewlines("结论如下：\\n\\n1. 第一项\\n2. 第二项\\n- 附注"),
    "结论如下：\n\n1. 第一项\n2. 第二项\n- 附注"
  );
  assert.equal(
    normalizeEscapedNewlines("第一行\\r\\n第二行\\r\\n\\r\\n第三行"),
    "第一行\n第二行\n\n第三行"
  );
  // Real multiline content must be preserved verbatim (including literal \n prose and code blocks).
  const multiline = "排查结果：\n- 避免字面 \\n\\n、\\n 显示\n```go\nfmt.Printf(\"hi\\n\")\n```";
  assert.equal(normalizeEscapedNewlines(multiline), multiline);

  // Inline code backticks, double backslash, and Windows drive paths are preserved.
  assert.equal(
    normalizeEscapedNewlines("说明：\\n1. 保留 `\\n` 与 `printf 'a\\nb'`\\n2. 正常换行"),
    "说明：\n1. 保留 `\\n` 与 `printf 'a\\nb'`\n2. 正常换行"
  );
  assert.equal(normalizeEscapedNewlines("正则 \\\\n 保持不变"), "正则 \\\\n 保持不变");
  assert.equal(normalizeEscapedNewlines("文件在 C:\\new\\report.md"), "文件在 C:\\new\\report.md");
});

test("MarkdownText renders paragraphs, headings, lists and preserves inline code \\n", async () => {
  const { MarkdownText } = await loadModule("src/components/MarkdownText.tsx");

  // 1. Legacy single-line text with literal \n\n and \n renders structured Markdown without visible \n.
  const htmlLegacy = renderToStaticMarkup(
    React.createElement(MarkdownText, {
      text: "## 巡检结论\\n\\n已完成两项核查：\\n- Hub 状态正常\\n- 节点在线\\n\\n注意保留 `\\n` 示例",
    })
  );
  assert.ok(htmlLegacy.includes("巡检结论"), htmlLegacy);
  assert.ok(htmlLegacy.includes("<ul"), htmlLegacy);
  assert.ok(htmlLegacy.includes("<li>") || htmlLegacy.includes("<li "), htmlLegacy);
  assert.ok(htmlLegacy.includes("Hub 状态正常"), htmlLegacy);
  assert.ok(htmlLegacy.includes("节点在线"), htmlLegacy);
  assert.ok(htmlLegacy.includes(">\\n</code>"), htmlLegacy);
  assert.equal(htmlLegacy.includes("\\n\\n"), false, `unexpected literal \\n\\n in ${htmlLegacy}`);

  // 2. Real multiline reply with fenced code block preserves \n inside code.
  const htmlReal = renderToStaticMarkup(
    React.createElement(MarkdownText, {
      text: "第一段\n\n第二段\n```go\nfmt.Printf(\"a\\nb\")\n```",
    })
  );
  assert.ok(htmlReal.includes("<p") && htmlReal.includes("第一段") && htmlReal.includes("第二段"), htmlReal);
  assert.ok(htmlReal.includes("<pre") && htmlReal.includes("fmt.Printf(&quot;a\\nb&quot;)"), htmlReal);
});

test("DecisionCard renders patrol_digest with Markdown list, category badge, worker chips, and no fake recommendation tag", async () => {
  const { DecisionCard } = await loadModule("src/components/DecisionCard.tsx");

  const html = renderToStaticMarkup(
    React.createElement(DecisionCard, {
      action: {
        id: "a900",
        kind: "patrol_digest",
        category: "patrol",
        thread: "chat:main",
        title: "有两项待你确认…",
        summary: "有两项待你确认：\n- `agent-flow-plan` 分支待验收\n- `personal-agent-inbox-research` 待批准开工",
        choices: [
          { key: "1", label: "查看物言分支 diff" },
          { key: "2", label: "批准收纳箱开工" },
        ],
        status: "pending",
        created_at: "2026-11-26T11:05:00+08:00",
      },
      workerNames: ["agent-flow-plan", "personal-agent-inbox-research", "unrelated-worker"],
      onResolve: async () => {},
      onOpenWorker: () => {},
    })
  );

  assert.ok(html.includes("待办巡检提醒"), html);
  assert.equal(html.includes("空闲巡检 / 晨间简报"), false, html);
  assert.equal(html.includes(">推荐<"), false, html);
  assert.ok(html.includes("<ul") && (html.includes("<li>") || html.includes("<li ")), html);
  assert.ok(html.includes("agent-flow-plan") && html.includes("personal-agent-inbox-research"), html);
  assert.equal(html.includes("unrelated-worker"), false, html);
  assert.ok(html.includes("发送指令"), html);

  const htmlMorning = renderToStaticMarkup(
    React.createElement(DecisionCard, {
      action: {
        id: "a901",
        kind: "patrol_digest",
        category: "morning",
        thread: "chat:main",
        title: "早报",
        summary: "早报：1 个未合分支",
        choices: [{ key: "1", label: "处理" }],
        status: "pending",
        created_at: "2026-11-26T09:00:00+08:00",
      },
      onResolve: async () => {},
    })
  );
  assert.ok(htmlMorning.includes("晨间待办简报"), htmlMorning);
});

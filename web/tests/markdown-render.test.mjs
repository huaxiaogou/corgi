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
    external: ["react", "react/jsx-runtime", "react-dom", "react-dom/server", "lucide-react", "mermaid"],
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

test("MarkdownText renders ordered lists (1. / 1) / 1）) as <ol> and keeps unordered lists as <ul>", async () => {
  const { MarkdownText } = await loadModule("src/components/MarkdownText.tsx");

  const html = renderToStaticMarkup(
    React.createElement(MarkdownText, {
      text: [
        "进度清单：",
        "1. HTML 预览已发布",
        "2) 输入框待修复",
        "3）收纳箱待批准",
        "3.14 不是列表",
        "- 无序项 A",
        "- 无序项 B",
        "",
        "3. 从第三步继续",
        "4. 收尾验收",
      ].join("\n"),
    })
  );

  assert.ok(html.includes("<ol"), html);
  assert.ok(html.includes("list-decimal"), html);
  assert.ok(html.includes("HTML 预览已发布"), html);
  assert.ok(html.includes("输入框待修复"), html);
  assert.ok(html.includes("收纳箱待批准"), html);
  assert.ok(html.includes("<p") && html.includes("3.14 不是列表"), html);
  assert.ok(html.includes("<ul") && html.includes("无序项 A"), html);
  assert.ok(html.includes('start="3"'), html);
});

test("chatFilter folds routine worker chatter, hides redundant quotes, and routes inline bubble actions", async () => {
  const {
    isDecisionAction,
    isRoutineChatterEvent,
    isInlineBubbleAction,
    shouldHideRedundantQuote,
    workerSpawnStatusBadge,
  } = await loadModule("src/lib/chatFilter.ts");

  assert.equal(
    isRoutineChatterEvent({ id: "e1", seq: 1, role: "event", thread: "chat:main", text: "agy-1 完成了一轮", at: "", ref: "agy-1", tone: "done" }, {}),
    true
  );
  assert.equal(
    isRoutineChatterEvent({ id: "e2", seq: 2, role: "event", thread: "chat:main", text: "已停止 agy-1（会话已保留）", at: "", ref: "agy-1", tone: "info" }, {}),
    true
  );
  assert.equal(
    isRoutineChatterEvent({ id: "e3", seq: 3, role: "event", thread: "chat:main", text: "派给了 agy-1", at: "", ref: "agy-1", tone: "spawn" }, {}),
    false
  );
  assert.equal(
    isRoutineChatterEvent({ id: "e4", seq: 4, role: "event", thread: "chat:main", text: "Corgi 已切换至全新会话并完成记忆交接", at: "", ref: "h:h42", tone: "info" }, {}),
    false
  );
  assert.equal(
    isRoutineChatterEvent({ id: "e5", seq: 5, role: "event", thread: "chat:main", text: "agy-1 出错了：rate limit", at: "", ref: "agy-1", tone: "error" }, {}),
    false
  );

  assert.equal(isInlineBubbleAction({ id: "a1", kind: "router_choice", thread: "chat:main", title: "q", status: "pending", created_at: "" }), true);
  assert.equal(isInlineBubbleAction({ id: "a2", kind: "router_check", thread: "chat:main", title: "q", status: "pending", created_at: "" }), true);
  assert.equal(isInlineBubbleAction({ id: "a3", kind: "worker_dialog", thread: "chat:main", title: "q", status: "pending", created_at: "" }), false);
  assert.equal(isInlineBubbleAction({ id: "a4", kind: "patrol_digest", thread: "chat:main", title: "q", status: "pending", created_at: "" }), false);

  assert.equal(isDecisionAction({ id: "d1", kind: "router_choice", thread: "chat:main", title: "q", status: "pending", choices: [{ key: "1", label: "继续" }], created_at: "" }), true);
  assert.equal(isDecisionAction({ id: "d2", kind: "router_check", thread: "chat:main", title: "q", status: "pending", created_at: "" }), false);
  assert.equal(isDecisionAction({ id: "d3", kind: "worker_dialog", thread: "chat:main", title: "q", status: "pending", created_at: "" }), true);
  assert.equal(isDecisionAction({ id: "d4", kind: "router_choice", thread: "chat:main", title: "q", status: "pending", choices: [], created_at: "" }), false);

  assert.equal(
    shouldHideRedundantQuote(
      { id: "e10", seq: 10, role: "corgi", thread: "chat:main", text: "收到", at: "", re: { entry: "e9", who: "你", text: "查一下" } },
      { lastUserId: "e9", seenSinceUser: false, lastEventRef: "" }
    ),
    true
  );
  assert.equal(
    shouldHideRedundantQuote(
      { id: "e11", seq: 11, role: "corgi", thread: "chat:main", text: "补充", at: "", re: { entry: "e9", who: "你", text: "查一下" } },
      { lastUserId: "e9", seenSinceUser: true, lastEventRef: "" }
    ),
    false
  );

  assert.equal(workerSpawnStatusBadge({ name: "w1", kind: "agy", cwd: "~", task: "", thread: "chat:main", status: "working", created_at: "" }).label, "还在跑");
  assert.equal(workerSpawnStatusBadge({ name: "w1", kind: "agy", cwd: "~", task: "", thread: "chat:main", status: "blocked", created_at: "" }).label, "等你确认");
  assert.equal(workerSpawnStatusBadge({ name: "w1", kind: "agy", cwd: "~", task: "", thread: "chat:main", status: "done", ended: true, created_at: "" }).label, "已结束");
});

test("MarkdownText 2.0 renders multi-backtick fences, diff stats, 18-line fold, tables, callouts, details XSS-safe, metrics, steps, and inline extensions", async () => {
  const { MarkdownText } = await loadModule("src/components/MarkdownText.tsx");

  const longCode = Array.from({ length: 22 }, (_, i) => `line ${i + 1}`).join("\n");
  const sample = [
    "````markdown",
    "```ts",
    "const nested = 1;",
    "```",
    "````",
    "",
    "```diff",
    "--- a/foo.go",
    "+++ b/foo.go",
    "-oldLine()",
    "+newLine1()",
    "+newLine2()",
    "```",
    "",
    "```ts",
    longCode,
    "```",
    "",
    "| 指标 | 数值 | 变化 |",
    "| --- | --- | --- |",
    "| 首屏 | 155 KB | -82% |",
    "",
    "| A | B | C | D |",
    "| --- | --- | --- | --- |",
    "| x | 1 | 2 | 3 |",
    "",
    "> [!NOTE] 提示标题",
    "> 普通 *斜体* ~~删除~~ ==高亮== <kbd>Cmd+K</kbd> `internal/core/engine.go:42` PR #128",
    "",
    "> [!TIP]",
    "> 小技巧",
    "",
    "> [!IMPORTANT]",
    "> 重要提醒",
    "",
    "> [!WARNING]",
    "> 警告内容",
    "",
    "> [!CAUTION]",
    "> 警示内容",
    "",
    "<details>",
    "<summary>安全折叠</summary>",
    "<script>alert(1)</script> <img src=x onerror=alert(2)>",
    "</details>",
    "",
    "```metrics",
    "首屏体积 | 155 KB | -82%",
    "单测 | 33/33",
    "```",
    "",
    "```steps",
    "[x] 完成 Batch 1",
    "[~] 执行 Batch 2",
    "[!] 待核验",
    "[ ] 待规划",
    "```",
    "",
    "```html-card",
    "<script>alert('never-exec')</script>",
    "```",
  ].join("\n");

  const html = renderToStaticMarkup(
    React.createElement(MarkdownText, {
      text: sample,
      onOpenFile: () => {},
    })
  );

  // Nested 4-backtick fence preserved inner ```ts.
  assert.ok(html.includes("const nested = 1;") && html.includes("```ts"), html);
  // Diff stats (+2 -1) and token vars.
  assert.ok(html.includes("+2") && html.includes("-1") && html.includes("var(--diff-add-bg)") && html.includes("var(--diff-del-bg)"), html);
  // 18-line code folding.
  assert.ok(html.includes("展开剩余 4 行（共 22 行）") && html.includes('data-code-folded="true"'), html);
  // Tables: 3-col non-wide vs 4-col wide + numeric tabular-nums right alignment.
  assert.ok(html.includes('data-cols="3"') && html.includes('data-wide="false"'), html);
  assert.ok(html.includes('data-cols="4"') && html.includes('data-wide="true"'), html);
  assert.ok(html.includes("text-right tabular-nums"), html);
  // All 5 callout levels.
  for (const kind of ["note", "tip", "important", "warning", "caution"]) {
    assert.ok(html.includes(`data-callout="${kind}"`), `missing callout ${kind}`);
  }
  // Inline enhancements.
  assert.ok(html.includes("<em") && html.includes("<del") && html.includes("<mark") && html.includes("<kbd"), html);
  assert.ok(html.includes("internal/core/engine.go:42") && html.includes("PR #128"), html);
  // <details> renders structurally with 0 raw HTML execution (script/onerror escaped as text).
  assert.ok(html.includes("<details") && html.includes("安全折叠"), html);
  assert.equal(html.includes("<script>"), false, "raw <script> tag must never appear unescaped");
  assert.equal(html.includes("<img src=x"), false, "raw <img onerror> must never appear unescaped");
  assert.ok(html.includes("&lt;script&gt;alert(1)&lt;/script&gt;"), html);
  // Metrics & steps blocks.
  assert.ok(html.includes('data-metrics-strip="true"') && html.includes("155 KB"), html);
  assert.ok(html.includes('data-steps-timeline="true"') && html.includes('data-step-status="done"') && html.includes('data-step-status="warn"'), html);
  // Unenabled html-card degrades to plain code block (zero iframe / zero script execution).
  assert.equal(html.includes("<iframe"), false, "html-card must not execute inside chat messages");
});

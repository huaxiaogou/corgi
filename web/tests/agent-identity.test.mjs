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
    jsx: "automatic",
    external: ["react", "react/jsx-runtime", "react-dom", "react-dom/server", "lucide-react"],
  });
  try {
    return await import(`${pathToFileURL(outFile).href}?t=${Date.now()}`);
  } finally {
    await rm(outFile, { force: true });
  }
}

const FORBIDDEN_RE = /不经过\s*Corgi|不经\s*Corgi/i;

function assertNoForbiddenCopy(label, value) {
  assert.equal(FORBIDDEN_RE.test(value), false, `${label} must not contain forbidden copy: ${value}`);
}

test("isCorgiRouterPane and isInternalCorgiPane identify internal router/dream panes accurately", async () => {
  const { isCorgiRouterPane, isInternalCorgiPane } = await loadModule("src/lib/agentIdentity.ts");

  const cases = [
    {
      name: "router pane matched by explicit routerPaneId",
      pane: { pane_id: "w1:p1", name: "custom", workspace_label: "other" },
      routerPaneId: "w1:p1",
      wantRouter: true,
      wantInternal: false,
    },
    {
      name: "router pane matched by corgi name",
      pane: { pane_id: "w2:p1", name: "corgi", workspace_label: "corgi · router" },
      routerPaneId: undefined,
      wantRouter: true,
      wantInternal: true,
    },
    {
      name: "dream pane is internal but not router",
      pane: { pane_id: "w3:p1", name: "corgi-dream", workspace_label: "corgi · dream · 2026" },
      routerPaneId: "w1:p1",
      wantRouter: false,
      wantInternal: true,
    },
    {
      name: "normal worker pane is neither router nor internal",
      pane: { pane_id: "w4:p1", name: "agy-3637", workspace_label: "corgi · agy-3637" },
      routerPaneId: "w1:p1",
      wantRouter: false,
      wantInternal: false,
    },
    {
      name: "null pane returns false for both",
      pane: null,
      routerPaneId: "w1:p1",
      wantRouter: false,
      wantInternal: false,
    },
  ];

  for (const tc of cases) {
    assert.equal(isCorgiRouterPane(tc.pane, tc.routerPaneId), tc.wantRouter, tc.name);
    assert.equal(isInternalCorgiPane(tc.pane), tc.wantInternal, tc.name);
  }
});

test("formatDocumentTitle, formatMobileAgentBar, formatAgentComposerContext, and formatAgentSpeakerLabel", async () => {
  const {
    formatPaneHandle,
    formatDocumentTitle,
    formatMobileAgentBar,
    formatAgentComposerContext,
    formatAgentSpeakerLabel,
  } = await loadModule("src/lib/agentIdentity.ts");

  const workerWithTopic = {
    pane_id: "w9:p1",
    name: "agy-3637",
    topic: "会话身份提醒",
    workspace_label: "corgi · agy-3637",
    kind: "agy",
    is_agent: true,
  };
  const workerPlain = {
    pane_id: "w10:p1",
    name: "api-profiler",
    topic: "",
    workspace_label: "corgi · api-profiler",
    kind: "claude",
    is_agent: true,
  };
  const shellPane = {
    pane_id: "w11:p1",
    name: "zsh-1",
    topic: "",
    workspace_label: "shell",
    kind: "shell",
    is_agent: false,
  };
  const routerPane = {
    pane_id: "w1:p1",
    name: "corgi",
    topic: "",
    workspace_label: "corgi · router",
    kind: "claude",
    is_agent: true,
  };

  // 1. formatPaneHandle keeps #name when topic is distinct.
  assert.deepEqual(formatPaneHandle(workerWithTopic), {
    title: "会话身份提醒",
    handle: "#agy-3637",
    showHandle: true,
  });
  assert.deepEqual(formatPaneHandle(workerPlain), {
    title: "api-profiler",
    handle: "#api-profiler",
    showHandle: false,
  });

  // 2. formatDocumentTitle table-driven.
  const titleCases = [
    { route: { view: "corgi", thread: "chat:main" }, pane: null, want: "Corgi" },
    { route: { view: "agent", paneId: "w9:p1" }, pane: workerWithTopic, want: "直聊 · #agy-3637 · Corgi" },
    { route: { view: "agent", paneId: "w-gone" }, pane: null, want: "会话已结束 · Corgi" },
    { route: { view: "agent", paneId: "w1:p1" }, pane: routerPane, want: "Corgi 底层终端 · Corgi" },
  ];
  for (const tc of titleCases) {
    const got = formatDocumentTitle(tc.route, tc.pane, "w1:p1");
    assert.equal(got, tc.want);
    assertNoForbiddenCopy("document.title", got);
  }

  // 3. formatMobileAgentBar table-driven.
  const mobileCases = [
    { pane: workerWithTopic, want: { title: "会话身份提醒", badge: "直聊", isCorgiBrain: false, ended: false } },
    { pane: workerPlain, want: { title: "#api-profiler", badge: "直聊", isCorgiBrain: false, ended: false } },
    { pane: shellPane, want: { title: "#zsh-1", badge: "终端", isCorgiBrain: false, ended: false } },
    { pane: routerPane, want: { title: "Corgi 底层终端", badge: "调试", isCorgiBrain: true, ended: false } },
    { pane: null, want: { title: "会话已结束", badge: "", isCorgiBrain: false, ended: true } },
  ];
  for (const tc of mobileCases) {
    const got = formatMobileAgentBar(tc.pane, "w1:p1");
    assert.deepEqual(got, tc.want);
    assertNoForbiddenCopy("mobile.title", got.title);
    assertNoForbiddenCopy("mobile.badge", got.badge);
  }

  // 4. formatAgentComposerContext table-driven + redline copy audit.
  const idleCtx = formatAgentComposerContext(workerWithTopic, { routerPaneId: "w1:p1", mode: "chat", busy: false });
  assert.equal(idleCtx.stripDesktop, "正在直聊 · 会话身份提醒 (#agy-3637)");
  assert.equal(idleCtx.stripMobile, "直聊 · 会话身份提醒");
  assert.equal(idleCtx.placeholder, "发给 会话身份提醒（支持粘贴或拖入文件）…");
  assert.equal(idleCtx.badge, "直聊");
  assert.equal(idleCtx.backLabel, "回 Corgi");

  const busyCtx = formatAgentComposerContext(workerWithTopic, { routerPaneId: "w1:p1", mode: "chat", busy: true });
  assert.equal(busyCtx.placeholder, "发给 会话身份提醒（运行中自动排队）…");

  const brainCtx = formatAgentComposerContext(routerPane, { routerPaneId: "w1:p1", mode: "screen", busy: false });
  assert.equal(brainCtx.isCorgiBrain, true);
  assert.equal(brainCtx.badge, "调试");
  assert.equal(brainCtx.stripMobile, "Corgi 底层终端 · 调试");

  for (const ctx of [idleCtx, busyCtx, brainCtx, formatAgentComposerContext(shellPane, { mode: "screen" })]) {
    for (const val of [ctx.stripDesktop, ctx.stripMobile, ctx.backLabel, ctx.placeholder, ctx.badge]) {
      assertNoForbiddenCopy("composerContext", val);
    }
  }

  // 5. formatAgentSpeakerLabel table-driven.
  assert.equal(
    formatAgentSpeakerLabel({ name: "agy-3637", kind: "agy", machine: "MacBook", isAgent: true }),
    "agy-3637 · agy · MacBook"
  );
  assert.equal(
    formatAgentSpeakerLabel({ name: "claude", kind: "claude", machine: "", isAgent: true }),
    "claude"
  );
  assert.equal(
    formatAgentSpeakerLabel({ name: "corgi", kind: "claude", isAgent: true, isCorgiBrain: true }),
    "Corgi 底层终端 · claude"
  );
});

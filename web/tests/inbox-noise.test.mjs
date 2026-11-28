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

test("chatFilter & inboxFilter isolate FYI notices from decisions, review badges, and inline buttons", async () => {
  const filter = await loadModule("src/lib/chatFilter.ts");
  const inbox = await loadModule("src/lib/inboxFilter.ts");
  const now = new Date().toISOString();

  const decision = {
    id: "a1",
    kind: "router_choice",
    thread: "chat:main",
    title: "契约分支拦截发布，待拍板",
    status: "pending",
    choices: [{ key: "1", label: "补规则" }, { key: "2", label: "手动删" }],
    created_at: now,
  };
  const realCheck = {
    id: "a2",
    kind: "router_check",
    category: "check",
    thread: "chat:main",
    title: "原型 v2 已交付，请验收",
    status: "pending",
    created_at: now,
  };
  const legacyCachedFYI = {
    id: "a3",
    kind: "router_check",
    category: "fyi",
    thread: "chat:main",
    title: "暂无新进展，无需干预",
    status: "pending",
    created_at: now,
  };
  const archivedFYI = {
    id: "a4",
    kind: "router_check",
    category: "fyi",
    thread: "chat:main",
    title: "巡检正常推进中",
    status: "resolved",
    resolved_by: "auto_fyi",
    created_at: now,
  };

  assert.equal(filter.isFYIAction(archivedFYI), true);
  assert.equal(filter.isFYIAction(realCheck), false);
  assert.equal(filter.isSubstantiveCheckAction(realCheck), true);
  assert.equal(filter.isSubstantiveCheckAction(legacyCachedFYI), false);
  assert.equal(filter.isInlineBubbleAction(legacyCachedFYI), false);
  assert.equal(filter.isInlineBubbleAction(realCheck), true);

  const part = inbox.partitionInboxActions([decision, realCheck, legacyCachedFYI], [archivedFYI], "chat:main");
  assert.deepEqual(part.pendingDecisions.map((a) => a.id), ["a1"]);
  assert.deepEqual(part.pendingChecks.map((a) => a.id), ["a2"]);
  assert.deepEqual(part.pendingActions.map((a) => a.id), ["a1", "a2"]);
  assert.deepEqual(part.activityNotices.map((a) => a.id), ["a4", "a3"]);

  // Amber blocking flag is true ONLY when pendingDecisions > 0; pure FYI never renders numeric badge.
  assert.deepEqual(inbox.formatInboxSummaryMeta(2, 0, 0, 5), {
    visible: true,
    isBlocking: true,
    headline: "待你拍板 (2)",
  });
  assert.deepEqual(inbox.formatInboxSummaryMeta(0, 1, 0, 5), {
    visible: true,
    isBlocking: false,
    headline: "审阅提醒 (1)",
  });
  assert.deepEqual(inbox.formatInboxSummaryMeta(0, 0, 0, 5), {
    visible: true,
    isBlocking: false,
    headline: "近期巡检动态",
  });
  assert.deepEqual(inbox.formatInboxSummaryMeta(0, 0, 0, 0), {
    visible: false,
    isBlocking: false,
    headline: "",
  });
});

test("deriveWorkItems ignores FYI checks for review status and indexes latest_update for search", async () => {
  const { deriveWorkItems, filterAndPartitionWorkItems } = await loadModule("src/lib/workItems.ts");
  const now = Date.parse("2026-11-28T23:00:00Z");
  const iso = new Date(now - 60_000).toISOString();

  const items = [
    {
      id: "wi1",
      thread: "chat:main",
      title: "配置管理方案",
      latest_update: "Codex 评审进行中，无需干预",
      latest_update_at: iso,
      created_at: iso,
      updated_at: iso,
    },
  ];
  const fyiCheck = {
    id: "a_fyi",
    kind: "router_check",
    category: "fyi",
    work_item_id: "wi1",
    thread: "chat:main",
    title: "暂无新进展",
    status: "pending",
    created_at: iso,
  };

  const derived = deriveWorkItems(items, [], [fyiCheck], "chat:main", now);
  assert.equal(derived[0].derived, "idle", "FYI check must never turn WorkItem into review status");

  const found = filterAndPartitionWorkItems(derived, "Codex", "all");
  assert.equal(found.active.length, 1);
  assert.equal(found.active[0].item.id, "wi1");
});

test("ChatPendingInbox keeps amber strictly for blocking decisions and renders quiet clearable activity feed", async () => {
  const { ChatPendingInbox } = await loadModule("src/features/chat/ChatPendingInbox.tsx");
  const now = new Date().toISOString();
  const baseProps = {
    thread: "chat:main",
    superReady: true,
    inboxOpen: false,
    onToggleOpen: () => {},
    onSetOpen: () => {},
    snoozedCount: 0,
    onJumpToEntry: () => {},
    onOpenWorker: () => {},
    onSelectFloating: () => {},
    onToast: () => {},
  };

  const fyi = {
    id: "a_fyi_1",
    kind: "router_check",
    category: "fyi",
    thread: "chat:main",
    entry_id: "e1",
    title: "巡检确认：正常推进中，无需干预",
    status: "resolved",
    created_at: now,
  };
  const realCheck = {
    id: "a_chk_1",
    kind: "router_check",
    category: "check",
    thread: "chat:main",
    entry_id: "e2",
    title: "原型 v2 已交付，请验收",
    status: "pending",
    created_at: now,
  };
  const decision = {
    id: "a_dec_1",
    kind: "router_choice",
    thread: "chat:main",
    title: "是否发版部署",
    status: "pending",
    choices: [{ key: "1", label: "发版" }, { key: "2", label: "暂缓" }],
    created_at: now,
  };

  // 1. Only FYI activity -> zero amber, zero numeric pending badge, quiet view & clear controls.
  const quietHtml = renderToStaticMarkup(
    React.createElement(ChatPendingInbox, {
      ...baseProps,
      pendingActions: [],
      pendingDecisions: [],
      pendingChecks: [],
      activityNotices: [fyi],
    })
  );
  assert.ok(quietHtml.includes("近期巡检动态"), quietHtml);
  assert.ok(!quietHtml.includes("var(--attn)"), "FYI-only bar must never use amber var(--attn)");
  assert.ok(!quietHtml.includes("审阅提醒"), "FYI-only bar must not claim 审阅提醒");
  assert.ok(quietHtml.includes("清空"), quietHtml);

  // 2. Only substantive check -> non-amber dot + 审阅提醒 (1) + one-click 全部已看.
  const checkOnlyHtml = renderToStaticMarkup(
    React.createElement(ChatPendingInbox, {
      ...baseProps,
      pendingActions: [realCheck],
      pendingDecisions: [],
      pendingChecks: [realCheck],
      activityNotices: [],
    })
  );
  assert.ok(checkOnlyHtml.includes("审阅提醒 (1)"), checkOnlyHtml);
  assert.ok(checkOnlyHtml.includes("全部已看"), checkOnlyHtml);
  assert.ok(!checkOnlyHtml.includes("var(--attn)"), "Check-only bar must not use amber var(--attn)");

  // 3. Blocking decision + FYI activity -> amber dot + 待你拍板 (1) only; expanded "全部" excludes FYI while offering 巡检动态 tab.
  const expandedHtml = renderToStaticMarkup(
    React.createElement(ChatPendingInbox, {
      ...baseProps,
      inboxOpen: true,
      pendingActions: [decision],
      pendingDecisions: [decision],
      pendingChecks: [],
      activityNotices: [fyi],
    })
  );
  assert.ok(expandedHtml.includes("var(--attn)"), expandedHtml);
  assert.ok(expandedHtml.includes("待你拍板 (1)"), expandedHtml);
  assert.ok(expandedHtml.includes("巡检动态"), expandedHtml);
  assert.ok(!expandedHtml.includes("巡检确认：正常推进中，无需干预"), "default All tab must not mix FYI rows into pending decisions");
});

test("WorkNotebookRow surfaces latest_update as collapsed fallback and expanded patrol detail", async () => {
  const { WorkNotebookRow } = await loadModule("src/features/chat/WorkNotebookRow.tsx");
  const iso = new Date().toISOString();
  const baseEntry = {
    item: {
      id: "wi1",
      thread: "chat:main",
      title: "配置管理方案",
      next_step: "",
      latest_update: "Codex 评审进行中，无需干预",
      latest_update_at: iso,
      created_at: iso,
      updated_at: iso,
    },
    derived: "idle",
    statusText: "待推进",
    isStale: false,
    linkedWorkers: [],
    updatedMs: Date.parse(iso),
    createdMs: Date.parse(iso),
  };
  const noop = () => {};
  const sharedProps = {
    isEditing: false,
    isEditingTitle: false,
    isExiting: false,
    isPulsing: false,
    isSaving: false,
    artsExpanded: false,
    isDesktop: true,
    editDraft: "",
    titleDraft: "",
    editInputRef: { current: null },
    onToggleOpen: noop,
    onStartEditTitle: noop,
    onCancelEditTitle: noop,
    onChangeTitleDraft: noop,
    onSaveTitle: noop,
    onStartEditNextStep: noop,
    onCancelEditNextStep: noop,
    onChangeEditDraft: noop,
    onSaveNextStep: noop,
    onExpandArts: noop,
    onOpenWorker: noop,
    onUnbindWorker: noop,
    onOpenFile: noop,
    onAskCorgi: noop,
    onHideMobile: noop,
    onMarkDone: noop,
  };

  // Collapsed without next_step falls back to latest_update.
  const collapsedHtml = renderToStaticMarkup(
    React.createElement(WorkNotebookRow, { ...sharedProps, entry: baseEntry, isOpen: false })
  );
  assert.ok(collapsedHtml.includes("Codex 评审进行中，无需干预"), collapsedHtml);

  // Expanded renders separate 最新巡检 block alongside next_step.
  const expandedHtml = renderToStaticMarkup(
    React.createElement(WorkNotebookRow, {
      ...sharedProps,
      entry: { ...baseEntry, item: { ...baseEntry.item, next_step: "等评审结论后合入" } },
      isOpen: true,
    })
  );
  assert.ok(expandedHtml.includes("等评审结论后合入"), expandedHtml);
  assert.ok(expandedHtml.includes("最新巡检"), expandedHtml);
  assert.ok(expandedHtml.includes("Codex 评审进行中，无需干预"), expandedHtml);
});

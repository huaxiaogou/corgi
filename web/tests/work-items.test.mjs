import assert from "node:assert/strict";
import { rm } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";
import { build } from "esbuild";

const root = path.resolve(import.meta.dirname, "..");

async function loadWorkItems() {
  const outFile = path.join(root, "tests", `.tmp-work-items-${process.pid}.mjs`);
  await build({
    entryPoints: [path.join(root, "src/lib/workItems.ts")],
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

test("deriveWorkItems prioritizes blocked -> working -> idle, sorts by updated_at, and detects stale > 7d", async () => {
  const { deriveWorkItems, DAY_MS } = await loadWorkItems();
  const now = Date.parse("2026-11-26T12:00:00Z");
  const isoAgo = (ms) => new Date(now - ms).toISOString();

  const items = [
    { id: "wi1", thread: "chat:main", title: "旧闲置", created_at: isoAgo(12 * DAY_MS), updated_at: isoAgo(10 * DAY_MS) },
    { id: "wi2", thread: "chat:main", title: "近期闲置", created_at: isoAgo(2 * DAY_MS), updated_at: isoAgo(3600_000) },
    { id: "wi3", thread: "chat:main", title: "运行中任务", workers: ["w-run"], created_at: isoAgo(DAY_MS), updated_at: isoAgo(600_000) },
    { id: "wi4", thread: "chat:main", title: "待确认任务", workers: ["w-blk"], created_at: isoAgo(DAY_MS), updated_at: isoAgo(7200_000) },
    { id: "wi5", thread: "chat:main", title: "已归档", created_at: isoAgo(DAY_MS), updated_at: isoAgo(60_000), closed_at: isoAgo(30_000) },
    { id: "wi6", thread: "wecom:grp1", title: "其他群聊", created_at: isoAgo(DAY_MS), updated_at: isoAgo(10_000) },
  ];
  const workers = [
    { name: "w-run", kind: "codex", cwd: "/tmp", task: "", thread: "chat:main", created_at: isoAgo(DAY_MS), status: "working" },
    { name: "w-blk", kind: "claude", cwd: "/tmp", task: "", thread: "chat:main", created_at: isoAgo(DAY_MS), status: "idle" },
  ];
  const actions = [
    { id: "act1", kind: "worker_dialog", thread: "chat:main", target_worker: "w-blk", title: "确认建索引", status: "pending", created_at: isoAgo(30_000) },
  ];

  const derived = deriveWorkItems(items, workers, actions, "chat:main", now);
  assert.deepEqual(
    derived.map((x) => [x.item.id, x.derived, x.isStale]),
    [
      ["wi4", "blocked", false],
      ["wi3", "working", false],
      ["wi2", "idle", false],
      ["wi1", "idle", true],
    ]
  );
  assert.equal(derived[3].statusText, "搁置较久");
});

test("applyFrozenOrder preserves hovered ordering while reflecting live status updates", async () => {
  const { deriveWorkItems, applyFrozenOrder } = await loadWorkItems();
  const now = Date.parse("2026-11-26T12:00:00Z");
  const iso = (m) => new Date(now - m * 60_000).toISOString();

  const items = [
    { id: "wiA", thread: "chat:main", title: "任务 A", workers: ["wA"], created_at: iso(10), updated_at: iso(1) },
    { id: "wiB", thread: "chat:main", title: "任务 B", workers: ["wB"], created_at: iso(20), updated_at: iso(5) },
  ];
  // Now wiB turns blocked (would naturally jump to #1), but frozen order keeps [wiA, wiB]
  const workers = [
    { name: "wA", kind: "codex", cwd: "/tmp", task: "", thread: "chat:main", created_at: iso(10), status: "working" },
    { name: "wB", kind: "codex", cwd: "/tmp", task: "", thread: "chat:main", created_at: iso(20), status: "blocked" },
  ];
  const natural = deriveWorkItems(items, workers, [], "chat:main", now);
  assert.deepEqual(natural.map((x) => x.item.id), ["wiB", "wiA"]);

  const frozen = applyFrozenOrder(natural, ["wiA", "wiB"]);
  assert.deepEqual(frozen.map((x) => x.item.id), ["wiA", "wiB"]);
  assert.equal(frozen[1].derived, "blocked");
});

test("filterAndPartitionWorkItems folds stale items by default but surfaces stale matches during search", async () => {
  const { deriveWorkItems, filterAndPartitionWorkItems, DAY_MS } = await loadWorkItems();
  const now = Date.parse("2026-11-26T12:00:00Z");
  const isoAgo = (ms) => new Date(now - ms).toISOString();

  const items = [
    { id: "wi1", thread: "chat:main", title: "飞书 Webhook 调研", created_at: isoAgo(15 * DAY_MS), updated_at: isoAgo(12 * DAY_MS) },
    { id: "wi2", thread: "chat:main", title: "SQLite 索引压测", created_at: isoAgo(DAY_MS), updated_at: isoAgo(60_000) },
  ];
  const enriched = deriveWorkItems(items, [], [], "chat:main", now);

  const idleView = filterAndPartitionWorkItems(enriched, "", "all");
  assert.deepEqual(idleView.active.map((x) => x.item.id), ["wi2"]);
  assert.deepEqual(idleView.stale.map((x) => x.item.id), ["wi1"]);
  assert.equal(idleView.filteringActive, false);

  const searched = filterAndPartitionWorkItems(enriched, "飞书", "all");
  assert.deepEqual(searched.active.map((x) => x.item.id), ["wi1"]);
  assert.deepEqual(searched.stale, []);
  assert.equal(searched.filteringActive, true);
});

test("formatShortAge, artifactBasename, and formatWorkItemPrompt produce clean UI text", async () => {
  const { formatShortAge, artifactBasename, formatWorkItemPrompt, DAY_MS } = await loadWorkItems();
  const now = Date.parse("2026-11-26T12:00:00Z");
  assert.equal(formatShortAge(new Date(now - 20_000).toISOString(), now), "刚刚");
  assert.equal(formatShortAge(new Date(now - 12 * 60_000).toISOString(), now), "12m");
  assert.equal(formatShortAge(new Date(now - 3 * 3600_000).toISOString(), now), "3h");
  assert.equal(formatShortAge(new Date(now - DAY_MS).toISOString(), now), "昨天");
  assert.equal(formatShortAge(new Date(now - 5 * DAY_MS).toISOString(), now), "5天前");

  assert.equal(artifactBasename("/Users/demo/docs/report.html"), "report.html");
  assert.equal(
    formatWorkItemPrompt({ id: "wi2749", title: "SQLite 索引压测" }, "同步一下进度"),
    "[小本本 wi2749 · SQLite 索引压测]\n同步一下进度"
  );
});

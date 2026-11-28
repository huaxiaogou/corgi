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

function createMemoryStorage() {
  const map = new Map();
  return {
    get length() {
      return map.size;
    },
    key(i) {
      return Array.from(map.keys())[i] ?? null;
    },
    getItem(k) {
      return map.has(k) ? map.get(k) : null;
    },
    setItem(k, v) {
      map.set(k, String(v));
    },
    removeItem(k) {
      map.delete(k);
    },
    _map: map,
  };
}

function hasLoneSurrogate(str) {
  for (let i = 0; i < str.length; i++) {
    const code = str.charCodeAt(i);
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = str.charCodeAt(i + 1);
      if (!(next >= 0xdc00 && next <= 0xdfff)) return true;
      i++;
    } else if (code >= 0xdc00 && code <= 0xdfff) {
      return true;
    }
  }
  return false;
}

function makeBaseSuperView(overrides = {}) {
  return {
    router: { alive: true, status: "idle", kind: "api", model: "deepseek-r1" },
    entries: [
      {
        id: "e1",
        seq: 1,
        role: "corgi",
        thread: "chat:main",
        text: "你好世界 🎉",
        at: "2026-11-28T10:00:00Z",
        artifacts: [
          {
            id: "art1",
            name: "report.md",
            kind: "markdown",
            size: 120,
            share_token: "SUPER_SECRET_SHARE_TOKEN",
          },
        ],
      },
    ],
    actions: [],
    pinned: [],
    workers: [],
    work_items: [
      {
        id: "wi1",
        thread: "chat:main",
        title: "Batch 1",
        created_at: "2026-11-28T10:00:00Z",
        updated_at: "2026-11-28T10:00:00Z",
        snapshots: [
          {
            id: "snap1",
            name: "demo.html",
            kind: "html",
            size: 2048,
            share_token: "ANOTHER_SECRET_SHARE_TOKEN",
          },
        ],
      },
    ],
    threads: [{ id: "chat:main", subscribers: [], last_at: "2026-11-28T10:00:00Z" }],
    queued: 0,
    queued_router: 0,
    paused: false,
    wecom_online: true,
    // Simulate unexpected extra sensitive field that must never survive whitelist compaction.
    settings: { wecom_secret: "TOP_SECRET_WECOM_KEY" },
    ...overrides,
  };
}

test("snapshotCache enforces scope isolation, TTL expiry, schemaVersion, and secret stripping", async () => {
  const mod = await loadModule("src/lib/snapshotCache.ts");
  const st = createMemoryStorage();
  const origin = "https://corgi.example.com";
  const now = 1_700_000_000_000;

  const sv = makeBaseSuperView();
  const ps = { ok: true, agents: [], shells: [], machines: [] };

  // Empty scope (legacy Hub fallback) skips write and returns null on read.
  assert.equal(mod.writeSnapshotCache(origin, "", "chat:main", { superView: sv, paneState: ps }, st, now), false);
  assert.equal(mod.readSnapshotCache(origin, "", "chat:main", st, now), null);

  // Normal write & read with scope A.
  assert.equal(mod.writeSnapshotCache(origin, "a1b2c3d4e5f6", "chat:main", { superView: sv, paneState: ps }, st, now), true);
  const hit = mod.readSnapshotCache(origin, "a1b2c3d4e5f6", "chat:main", st, now + 1000);
  assert.ok(hit && hit.superView, "expected cache hit");
  assert.equal(hit.superView.entries[0].text, "你好世界 🎉");

  // Verify share_token & settings never persisted.
  const storedRaw = Array.from(st._map.values()).join("\n");
  assert.equal(storedRaw.includes("SUPER_SECRET_SHARE_TOKEN"), false);
  assert.equal(storedRaw.includes("ANOTHER_SECRET_SHARE_TOKEN"), false);
  assert.equal(storedRaw.includes("TOP_SECRET_WECOM_KEY"), false);
  assert.equal(storedRaw.includes('"share_token"'), false);
  assert.equal(storedRaw.includes('"settings"'), false);

  // Different scope must NOT read scope A's snapshot.
  assert.equal(mod.readSnapshotCache(origin, "ffffffffffff", "chat:main", st, now + 1000), null);

  // Expired after 30 days TTL -> returns null and evicts key.
  assert.equal(mod.readSnapshotCache(origin, "a1b2c3d4e5f6", "chat:main", st, now + mod.SNAPSHOT_TTL_MS + 1), null);
  assert.equal(st.length, 0);

  // Schema version mismatch -> returns null and evicts key.
  const key = mod.buildSnapshotKey(origin, "a1b2c3d4e5f6", "chat:main");
  st.setItem(key, JSON.stringify({ schemaVersion: 99, savedAt: now, superView: sv, paneState: ps }));
  assert.equal(mod.readSnapshotCache(origin, "a1b2c3d4e5f6", "chat:main", st, now), null);
  assert.equal(st.length, 0);
});

test("snapshotCache trims oversized Chinese/Emoji SuperView to <= 200 KiB UTF-8 cleanly and preserves non-snapshot keys", async () => {
  const mod = await loadModule("src/lib/snapshotCache.ts");
  const st = createMemoryStorage();
  st.setItem("corgi.theme", "dark");
  st.setItem("corgi.notebook.mode", "expanded");

  const chunk = "中文测试🚀🔥✨".repeat(4000);
  const entries = Array.from({ length: 60 }, (_, i) => ({
    id: `e${i}`,
    seq: i + 1,
    role: "corgi",
    thread: "chat:main",
    text: chunk,
    at: "2026-11-28T10:00:00Z",
  }));
  const hugeSV = makeBaseSuperView({ entries });
  const origin = "https://corgi.example.com";
  const ok = mod.writeSnapshotCache(origin, "001122334455", "chat:main", { superView: hugeSV, paneState: null }, st, 1_700_000_000_000);
  assert.equal(ok, true);

  const key = mod.buildSnapshotKey(origin, "001122334455", "chat:main");
  const raw = st.getItem(key);
  assert.ok(raw, "expected stored snapshot");
  assert.ok(mod.utf8ByteLength(raw) <= mod.SNAPSHOT_MAX_BYTES, `stored size ${mod.utf8ByteLength(raw)} exceeds 200 KiB`);

  const parsed = mod.readSnapshotCache(origin, "001122334455", "chat:main", st, 1_700_000_000_000);
  assert.ok(parsed && parsed.superView.entries.length > 0);
  for (const e of parsed.superView.entries) {
    assert.equal(hasLoneSurrogate(e.text), false, "truncated text must not split UTF-16 surrogate pairs");
  }

  // clearAllSnapshotCaches clears only corgi.snap.* keys and keeps corgi.theme & corgi.notebook.mode.
  mod.clearAllSnapshotCaches(st);
  assert.equal(st.getItem(key), null);
  assert.equal(st.getItem("corgi.theme"), "dark");
  assert.equal(st.getItem("corgi.notebook.mode"), "expanded");
});

test("DecisionCard disables resolution buttons and custom form while !superReady, and AppShellSkeleton has zero sensitive text", async () => {
  const { DecisionCard } = await loadModule("src/components/DecisionCard.tsx");
  const { AppShellSkeleton } = await loadModule("src/components/AppShellSkeleton.tsx");

  const cardHtml = renderToStaticMarkup(
    React.createElement(DecisionCard, {
      disabled: true,
      action: {
        id: "a1",
        kind: "router_choice",
        thread: "chat:main",
        title: "确认发布？",
        choices: [
          { key: "1", label: "同意" },
          { key: "2", label: "暂缓" },
        ],
        status: "pending",
        created_at: "2026-11-28T10:00:00Z",
      },
      onResolve: async () => {},
      onSnooze: async () => {},
      onDismiss: async () => {},
    })
  );
  assert.ok(cardHtml.includes("<textarea") && cardHtml.includes("disabled=\"\""), cardHtml);
  // Every choice button & action button should be disabled while !superReady.
  const enabledChoice = cardHtml.match(/<button(?![^>]*disabled)[^>]*>[\s\S]*?(同意|暂缓|稍后处理|暂不处理)/);
  assert.equal(enabledChoice, null, `expected all decision controls disabled while !superReady: ${cardHtml}`);

  const skelHtml = renderToStaticMarkup(React.createElement(AppShellSkeleton));
  assert.ok(skelHtml.includes("aria-busy=\"true\""), skelHtml);
  assert.equal(skelHtml.includes("SUPER_SECRET"), false);
});

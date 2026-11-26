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

test("isOldVersion compares semver accurately and ignores dev builds", async () => {
  const { isOldVersion } = await loadModule("src/lib/fleetUpdate.ts");

  assert.equal(isOldVersion("v0.1.40", "v0.1.41"), true);
  assert.equal(isOldVersion("v0.1.41", "v0.1.41"), false);
  assert.equal(isOldVersion("v0.2.0", "v0.1.99"), false);
  assert.equal(isOldVersion("dev", "v0.1.41"), false);
  assert.equal(isOldVersion("", "v0.1.41"), false);
});

test("selectOutdatedOnline orders remote online Nodes before Hub and excludes offline Nodes", async () => {
  const { selectOutdatedOnline } = await loadModule("src/lib/fleetUpdate.ts");

  const verInfo = {
    version: "v0.1.40",
    latest: "v0.1.41",
    outdated: [],
    machines: [
      { id: "", name: "hub-box", version: "v0.1.40", local: true, online: true },
      { id: "n-zeta", name: "Zeta", version: "v0.1.40", local: false, online: true },
      { id: "n-alpha", name: "Alpha", version: "v0.1.39", local: false, online: true },
      { id: "n-off", name: "OfflineMac", version: "v0.1.38", local: false, online: false },
      { id: "n-new", name: "Fresh", version: "v0.1.41", local: false, online: true },
    ],
  };

  const list = selectOutdatedOnline(verInfo, []);
  assert.deepEqual(
    list.map((x) => x.id),
    ["n-alpha", "n-zeta", ""],
    "expected online remote nodes alphabetically before Hub"
  );
});

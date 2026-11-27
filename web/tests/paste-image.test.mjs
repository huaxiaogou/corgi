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

test("resolvePastedFiles collapses desktop hash-named sandbox duplicate and falls back across clipboard API & HTML", async () => {
  const { resolvePastedFiles, extractDataImageFilesFromHTML } = await loadModule("src/lib/pasteImage.ts");

  // 1. Desktop messaging app dual pair (hash-named sandbox jpg + memory png bitmap) -> keeps single png.
  const sandboxJpg = new File([new Uint8Array([1, 2, 3])], "a1b2c3d4e5f60718293a.jpg", { type: "image/jpeg" });
  const memoryPng = new File([new Uint8Array([4, 5, 6, 7])], "image.png", { type: "image/png" });
  const pairRes = await resolvePastedFiles({
    files: [sandboxJpg, memoryPng],
    itemFiles: [memoryPng],
    html: "",
    plainText: "",
    hasMediaHint: true,
  });
  assert.equal(pairRes.files.length, 1);
  assert.equal(pairRes.files[0].name, "image.png");

  // 2. Unreadable sandbox file falls back to navigator.clipboard.read().
  const brokenFile = {
    name: "deadbeef1234567890ab.jpg",
    type: "image/jpeg",
    size: 100,
    slice() {
      return {
        arrayBuffer() {
          return Promise.reject(new Error("NotReadableError"));
        },
      };
    },
  };
  const clipRes = await resolvePastedFiles(
    {
      files: [brokenFile],
      itemFiles: [],
      html: "",
      plainText: "",
      hasMediaHint: true,
    },
    {
      async read() {
        return [
          {
            types: ["image/png"],
            async getType() {
              return new Blob([new Uint8Array([9, 8, 7])], { type: "image/png" });
            },
          },
        ];
      },
    }
  );
  assert.equal(clipRes.files.length, 1);
  assert.equal(clipRes.files[0].type, "image/png");
  assert.equal(clipRes.error, undefined);

  // 3. HTML data:image/png;base64 fallback when clipboard API unavailable.
  const tinyPngB64 = Buffer.from([137, 80, 78, 71]).toString("base64");
  const htmlRes = await resolvePastedFiles({
    files: [],
    itemFiles: [],
    html: `<img src="data:image/png;base64,${tinyPngB64}">`,
    plainText: "",
    hasMediaHint: true,
  });
  assert.equal(htmlRes.files.length, 1);
  assert.equal(htmlRes.files[0].name, "pasted-image-1.png");
  assert.equal(extractDataImageFilesFromHTML(`<img src="data:image/jpeg;base64,${tinyPngB64}">`)[0].name, "pasted-image-1.jpg");

  // 4. All layers fail while media hinted -> returns helpful Chinese error toast message.
  const failRes = await resolvePastedFiles({
    files: [brokenFile],
    itemFiles: [],
    html: "",
    plainText: "",
    hasMediaHint: true,
  });
  assert.equal(failRes.files.length, 0);
  assert.ok(failRes.error && failRes.error.includes("剪贴板"));
});

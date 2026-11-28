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

test("partitionEntryArtifacts & partitionEntryFiles deduplicate deterministically and preserve worker report & expired cards", async () => {
  const mod = await loadModule("src/lib/imageGallery.ts");

  // Artifacts: idx 0 worker report card + expired image card stay in cards; duplicate artifact.id deduped.
  const artPart = mod.partitionEntryArtifacts(
    [
      { id: "a-report", name: "report.png", kind: "image", size: 500 },
      { id: "a-exp", name: "old.png", kind: "image", size: 400, expired: true },
      { id: "a-img1", name: "shot.png", kind: "image", size: 1200 },
      { id: "a-img1", name: "shot.png", kind: "image", size: 1200 },
      { id: "a-img2", name: "shot.png", kind: "image", size: 1300 },
      { id: "a-doc", name: "notes.md", kind: "markdown", size: 800 },
    ],
    { name: "agy-1" }
  );

  assert.equal(artPart.images.length, 2, "two distinct artifact IDs with same filename must both remain");
  assert.deepEqual(
    artPart.images.map((x) => x.id),
    ["a-img1", "a-img2"]
  );
  assert.equal(artPart.cards.length, 3, "worker report, expired image, and markdown doc must stay in cards");
  assert.equal(artPart.cards[0].art.id, "a-report");
  assert.equal(artPart.cards[0].isWorkerReport, true);
  assert.equal(artPart.cards[1].art.id, "a-exp");
  assert.equal(artPart.cards[2].art.id, "a-doc");

  // Files: different directories with identical filename NOT deduped; exact duplicate path IS deduped.
  const filePart = mod.partitionEntryFiles(
    ["/tmp/a/shot.png", "/tmp/b/shot.png", "/tmp/a/shot.png", "/tmp/readme.md"],
    "mac-1"
  );
  assert.equal(filePart.images.length, 2);
  assert.equal(filePart.images[0].dedupKey, "file:mac-1:/tmp/a/shot.png");
  assert.equal(filePart.images[1].dedupKey, "file:mac-1:/tmp/b/shot.png");
  assert.deepEqual(filePart.nonImageFiles, ["/tmp/readme.md"]);
});

test("ImageGallery renders single aspect-preserving image vs 2/4/6-image grid capped at 4 <img> with +N badge", async () => {
  const { ImageGallery } = await loadModule("src/components/ImageGallery.tsx");

  const makeItems = (n) =>
    Array.from({ length: n }, (_, i) => ({
      id: `img-${i + 1}`,
      dedupKey: `artifact:img-${i + 1}`,
      src: `/api/artifacts/img-${i + 1}/raw`,
      downloadUrl: `/api/artifacts/img-${i + 1}/download`,
      name: `shot-${i + 1}.png`,
      size: 1024 * (i + 1),
    }));

  // 1 image: min-h-[140px], max-h-[320px] sm:max-h-[380px], no 2-col grid.
  const html1 = renderToStaticMarkup(
    React.createElement(ImageGallery, { items: makeItems(1), onOpenLightbox: () => {} })
  );
  assert.ok(html1.includes("min-h-[140px]") && html1.includes("sm:max-h-[380px]"), html1);
  assert.equal(html1.includes('data-gallery-grid="true"'), false);

  // 2 & 4 images: 2-col grid, aspect-[4/3], object-cover object-top, no +N badge.
  const html2 = renderToStaticMarkup(
    React.createElement(ImageGallery, { items: makeItems(2), onOpenLightbox: () => {} })
  );
  assert.ok(html2.includes("grid-cols-2") && html2.includes("aspect-[4/3]") && html2.includes("object-top"), html2);
  assert.equal((html2.match(/<img /g) ?? []).length, 2);
  assert.equal(html2.includes("data-gallery-overflow"), false);

  // 6 images: strictly max 4 <img> elements rendered on initial pass + "+2" overflow scrim.
  const html6 = renderToStaticMarkup(
    React.createElement(ImageGallery, { items: makeItems(6), onOpenLightbox: () => {} })
  );
  assert.equal((html6.match(/<img /g) ?? []).length, 4, "must render at most 4 inline <img> elements");
  assert.ok(html6.includes('data-gallery-overflow="2"') && html6.includes("+2"), html6);
});

test("computeSwipeOffset applies 0.25x damping clamped at +/-48px on boundaries with no wrap-around", async () => {
  const { computeSwipeOffset, LIGHTBOX_DAMP_FACTOR, LIGHTBOX_DAMP_MAX_PX } = await loadModule("src/lib/imageGallery.ts");

  assert.equal(LIGHTBOX_DAMP_FACTOR, 0.25);
  assert.equal(LIGHTBOX_DAMP_MAX_PX, 48);

  // Middle slide: 1:1 tracking both directions.
  assert.equal(computeSwipeOffset(80, 1, 3), 80);
  assert.equal(computeSwipeOffset(-80, 1, 3), -80);

  // First slide dragging right (past start): damped 0.25x and capped at +48px.
  assert.equal(computeSwipeOffset(40, 0, 3), 10);
  assert.equal(computeSwipeOffset(400, 0, 3), 48);
  // First slide dragging left (to slide 2): 1:1.
  assert.equal(computeSwipeOffset(-60, 0, 3), -60);

  // Last slide dragging left (past end): damped 0.25x and capped at -48px.
  assert.equal(computeSwipeOffset(-40, 2, 3), -10);
  assert.equal(computeSwipeOffset(-400, 2, 3), -48);
  // Last slide dragging right (to slide 1): 1:1.
  assert.equal(computeSwipeOffset(60, 2, 3), 60);

  // Single image: damped both ways.
  assert.equal(computeSwipeOffset(100, 0, 1), 25);
  assert.equal(computeSwipeOffset(-300, 0, 1), -48);
});

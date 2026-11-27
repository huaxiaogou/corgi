import assert from "node:assert/strict";
import test from "node:test";
import {
  TOKENHUB_MODELS,
  getRouterAPIEffortOptions,
  resolveRouterAPIEffort,
} from "../src/lib/routerApiModels.ts";

test("TokenHub models expose exact supported reasoning efforts without fake recommendations", () => {
  for (const m of TOKENHUB_MODELS) {
    assert.ok(!m.label.includes("推荐"), `model label should not contain 推荐: ${m.label}`);
    for (const opt of getRouterAPIEffortOptions("tokenhub", m.id)) {
      assert.ok(!opt.label.includes("推荐"), `effort label should not contain 推荐: ${opt.label}`);
    }
  }

  // hy4 supports only high and no_think (never low or max)
  const hy4Efforts = getRouterAPIEffortOptions("tokenhub", "hy4").map((o) => o.id);
  assert.deepEqual(hy4Efforts, ["high", "no_think"]);
  assert.equal(resolveRouterAPIEffort("tokenhub", "hy4", "max"), "high");
  assert.equal(resolveRouterAPIEffort("tokenhub", "hy4", "low"), "high");
  assert.equal(resolveRouterAPIEffort("tokenhub", "hy4", "no_think"), "no_think");
  assert.equal(resolveRouterAPIEffort("tokenhub", "hy4", "high"), "high");

  // glm-5.3 / glm-5.3-flash / kimi-k3 support only low / high / max (never no_think)
  for (const id of ["glm-5.3", "glm-5.3-flash", "kimi-k3"]) {
    const opts = getRouterAPIEffortOptions("tokenhub", id).map((o) => o.id);
    assert.deepEqual(opts, ["low", "high", "max"]);
    assert.equal(resolveRouterAPIEffort("tokenhub", id, "no_think"), "low");
    assert.equal(resolveRouterAPIEffort("tokenhub", id, ""), "low");
    assert.equal(resolveRouterAPIEffort("tokenhub", id, "high"), "high");
    assert.equal(resolveRouterAPIEffort("tokenhub", id, "max"), "max");
  }

  // Custom OpenAI keeps generic efforts including empty model default
  const customOpts = getRouterAPIEffortOptions("openai", "gpt-4o").map((o) => o.id);
  assert.ok(customOpts.includes(""));
  assert.ok(customOpts.includes("medium"));
  assert.equal(resolveRouterAPIEffort("openai", "gpt-4o", ""), "");
});

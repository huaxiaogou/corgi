import test from "node:test";
import assert from "node:assert/strict";
import {
  formatDispatchSummary,
  matchLiveWorkerForRef,
  resolveEffectiveDispatch,
} from "../src/lib/dispatchDefaults.ts";

const machines = [
  { id: "", name: "VM-247-217-tencentos", agents: ["codex", "codebuddy"], local: true, online: true },
  { id: "kiperjing-mb0-2", name: "KIPERJING-MB0", agents: ["claude", "codex", "agy", "codebuddy"], local: false, online: true },
];

const defaults = {
  worker_machine: "kiperjing-mb0-2",
  worker_kind: "agy",
  worker_model: "gemini-4-neon",
  worker_effort: "xhigh",
  kind_defaults: {
    agy: { model: "gemini-4-neon", effort: "xhigh" },
    codebuddy: { model: "claude-sonnet-5.5", effort: "high" },
    codex: { model: "gpt-6.2-astra", effort: "xhigh" },
  },
  machine_kind_overrides: {
    hub: {
      codebuddy: { model: "claude-opus-5.5", effort: "high" },
    },
  },
  projects: [],
};

test("resolveEffectiveDispatch follows 4-tier precedence and shows real defaults", () => {
  const base = resolveEffectiveDispatch(defaults, machines);
  assert.equal(base.machine, "kiperjing-mb0-2");
  assert.equal(base.kind, "agy");
  assert.equal(base.model, "gemini-4-neon");
  assert.equal(base.effort, "xhigh");
  assert.equal(base.source, "kind_default");
  assert.equal(base.isOverride, false);
  assert.equal(formatDispatchSummary(base), "KIPERJING-MB0 · agy · gemini-4-neon (xhigh)");

  // Switching machine+kind to Hub codebuddy hits machine_override.
  const hubCb = resolveEffectiveDispatch(defaults, machines, { machine: "hub", kind: "codebuddy" });
  assert.equal(hubCb.machine, "");
  assert.equal(hubCb.machineLabel, "Hub");
  assert.equal(hubCb.model, "claude-opus-5.5");
  assert.equal(hubCb.effort, "high");
  assert.equal(hubCb.source, "machine_override");
  assert.equal(hubCb.isOverride, true);

  // Switching kind to Mac codebuddy hits kind_default.
  const macCb = resolveEffectiveDispatch(defaults, machines, { kind: "codebuddy" });
  assert.equal(macCb.model, "claude-sonnet-5.5");
  assert.equal(macCb.effort, "high");
  assert.equal(macCb.source, "kind_default");

  // Unconfigured kind explicitly reports cli_default.
  const claude = resolveEffectiveDispatch(defaults, machines, { kind: "claude" });
  assert.equal(claude.model, "");
  assert.equal(claude.source, "cli_default");
  assert.equal(formatDispatchSummary(claude), "KIPERJING-MB0 · claude · 本机默认");

  // Explicit model overrides everything.
  const explicit = resolveEffectiveDispatch(defaults, machines, {
    machine: "hub",
    kind: "codebuddy",
    model: "gpt-6-luna",
    effort: "low",
  });
  assert.equal(explicit.model, "gpt-6-luna");
  assert.equal(explicit.effort, "low");
  assert.equal(explicit.source, "explicit");
});

test("matchLiveWorkerForRef binds strictly to session and machine to prevent name reuse hijack", () => {
  const workers = [
    {
      name: "flow-plan",
      kind: "agy",
      machine: "kiperjing-mb0-2",
      cwd: "/repo",
      task: "new task reusing same name",
      thread: "chat:main",
      pane_id: "w9:p1@kiperjing-mb0-2",
      created_at: "2026-11-27T00:00:00Z",
      status: "idle",
      session: "sess-NEW-999",
    },
  ];

  // Different session with same name must NOT hijack the old delivery card.
  const stale = matchLiveWorkerForRef(
    { name: "flow-plan", machine: "kiperjing-mb0-2", session: "sess-OLD-111" },
    workers
  );
  assert.equal(stale, null);

  // Exact session match binds directly.
  const matched = matchLiveWorkerForRef(
    { name: "flow-plan", machine: "kiperjing-mb0-2", session: "sess-NEW-999" },
    workers
  );
  assert.equal(matched?.pane_id, "w9:p1@kiperjing-mb0-2");
});

import type { EntryWorkerRef, Machine, ModelEffortSpec, Worker, WorkerDispatchDefaults } from "../types/api";

export type ModelResolveSource = "explicit" | "machine_override" | "kind_default" | "cli_default";

export interface ResolvedDispatch {
  machine: string; // "" for Hub local, else machine ID
  machineLabel: string;
  kind: string;
  model: string;
  effort: string;
  source: ModelResolveSource;
  isOverride: boolean;
}

export function normalizeMachineKey(machineId?: string): string {
  const m = (machineId ?? "").trim();
  if (!m || m.toLowerCase() === "local" || m.toLowerCase() === "hub") return "hub";
  return m;
}

export function normalizeKind(kind?: string): string {
  const k = (kind ?? "").trim().toLowerCase();
  if (k === "antigravity") return "agy";
  if (k === "cbc") return "codebuddy";
  return k;
}

function lookupSpec(map: Record<string, ModelEffortSpec> | undefined, normKind: string): ModelEffortSpec | null {
  if (!map || !normKind) return null;
  if (map[normKind]) {
    return { model: (map[normKind].model ?? "").trim(), effort: (map[normKind].effort ?? "").trim() };
  }
  for (const [k, spec] of Object.entries(map)) {
    if (normalizeKind(k) === normKind) {
      return { model: (spec.model ?? "").trim(), effort: (spec.effort ?? "").trim() };
    }
  }
  return null;
}

export function resolveMachineDisplayName(machineId: string, machines: Machine[]): string {
  const clean = (machineId ?? "").trim();
  const isLocal = !clean || clean === "local" || clean === "hub";
  const found = machines.find((m) => (isLocal ? m.local : m.id === clean));
  if (found) return found.local ? `${found.name}（Hub）` : found.name;
  return isLocal ? "Hub" : clean;
}

export function resolveShortMachineName(machineId: string, machines: Machine[]): string {
  const clean = (machineId ?? "").trim();
  const isLocal = !clean || clean === "local" || clean === "hub";
  const found = machines.find((m) => (isLocal ? m.local : m.id === clean));
  if (found) return found.local ? "Hub" : found.name;
  return isLocal ? "Hub" : clean;
}

/**
 * Mirrors backend ResolveWorkerModel 4-tier precedence:
 * 1. explicit -> 2. machine_override -> 3. kind_default (or legacy worker_model/effort) -> 4. cli_default
 */
export function resolveEffectiveDispatch(
  defaults: WorkerDispatchDefaults | null | undefined,
  machines: Machine[],
  override: { machine?: string; kind?: string; model?: string; effort?: string } = {}
): ResolvedDispatch {
  const ovrMachineRaw = (override.machine ?? "").trim();
  const ovrKind = normalizeKind(override.kind);
  const ovrModel = (override.model ?? "").trim();
  const ovrEffort = (override.effort ?? "").trim();

  const defMachine = (defaults?.worker_machine ?? "").trim();
  const defKind = normalizeKind(defaults?.worker_kind) || "claude";

  const effMachine = ovrMachineRaw ? (ovrMachineRaw === "local" || ovrMachineRaw === "hub" ? "" : ovrMachineRaw) : defMachine;
  const effKind = ovrKind || defKind;

  let baseModel = "";
  let baseEffort = "";
  let baseSource: ModelResolveSource = "cli_default";

  const mk = normalizeMachineKey(effMachine);
  const machineMap = defaults?.machine_kind_overrides?.[mk] ?? (mk === "hub" ? defaults?.machine_kind_overrides?.[""] : undefined);
  const machineSpec = lookupSpec(machineMap, effKind);
  if (machineSpec && (machineSpec.model || machineSpec.effort)) {
    baseModel = machineSpec.model ?? "";
    baseEffort = machineSpec.effort ?? "";
    baseSource = "machine_override";
  } else {
    const kindSpec = lookupSpec(defaults?.kind_defaults, effKind);
    if (kindSpec && (kindSpec.model || kindSpec.effort)) {
      baseModel = kindSpec.model ?? "";
      baseEffort = kindSpec.effort ?? "";
      baseSource = "kind_default";
    } else if (effKind === defKind && ((defaults?.worker_model ?? "").trim() || (defaults?.worker_effort ?? "").trim())) {
      baseModel = (defaults?.worker_model ?? "").trim();
      baseEffort = (defaults?.worker_effort ?? "").trim();
      baseSource = "kind_default";
    }
  }

  let finalModel = baseModel;
  let finalEffort = baseEffort;
  let source: ModelResolveSource = baseSource;
  if (ovrModel || ovrEffort) {
    source = "explicit";
    if (ovrModel) {
      finalModel = ovrModel;
      finalEffort = ovrEffort;
    } else {
      finalEffort = ovrEffort;
    }
  }

  const isOverride = Boolean(ovrMachineRaw || ovrKind || ovrModel || ovrEffort);
  return {
    machine: effMachine,
    machineLabel: resolveShortMachineName(effMachine, machines),
    kind: effKind,
    model: finalModel,
    effort: finalEffort,
    source,
    isOverride,
  };
}

export function formatDispatchSummary(r: ResolvedDispatch): string {
  const modelPart = r.model
    ? r.effort
      ? `${r.model} (${r.effort})`
      : r.model
    : r.effort
      ? `本机默认 (${r.effort})`
      : "本机默认";
  return `${r.machineLabel} · ${r.kind} · ${modelPart}`;
}

export function modelSourceLabel(source?: string): string {
  switch (source) {
    case "explicit":
      return "本次指定";
    case "machine_override":
      return "电脑单独覆盖";
    case "kind_default":
      return "工具默认模型";
    case "cli_default":
      return "跟随本机 CLI 默认";
    default:
      return "";
  }
}

/**
 * Matches an EntryWorkerRef against live active workers strictly by session first,
 * then name+machine (never accidentally binding a reused worker name on another session).
 */
export function matchLiveWorkerForRef(ref: EntryWorkerRef | undefined, workers: Worker[] | undefined): Worker | null {
  if (!ref || !workers || workers.length === 0) return null;
  const live = workers.filter((w) => !w.ended && Boolean(w.pane_id));
  const refSession = (ref.session ?? "").trim();
  const refMachine = (ref.machine ?? "").trim();
  if (refSession) {
    const bySession = live.find(
      (w) => w.session === refSession && (!refMachine || (w.machine ?? "") === refMachine)
    );
    if (bySession) return bySession;
  }
  const byNameMachine = live.find(
    (w) =>
      w.name === ref.name &&
      (!refMachine || (w.machine ?? "") === refMachine) &&
      (!refSession || !w.session || w.session === refSession)
  );
  return byNameMachine ?? null;
}

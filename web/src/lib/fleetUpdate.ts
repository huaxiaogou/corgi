import type { Machine, VersionMachine, VersionStatus } from "../types/api";

export function parseSemverTuple(v?: string): [number, number, number] | null {
  const m = /^v(\d+)\.(\d+)\.(\d+)$/.exec((v ?? "").trim());
  if (!m) return null;
  return [Number(m[1]), Number(m[2]), Number(m[3])];
}

export function isOldVersion(have?: string, latest?: string): boolean {
  const a = parseSemverTuple(have);
  const b = parseSemverTuple(latest);
  if (!a || !b) return false;
  for (let i = 0; i < 3; i++) {
    if (a[i] !== b[i]) return a[i] < b[i];
  }
  return false;
}

export interface OutdatedTarget {
  id: string;
  name: string;
  version: string;
  local: boolean;
}

/**
 * Computes outdated online machines ordered strictly remote Nodes first (alphabetical),
 * then Hub last, matching the Hub's sequential fleet update order.
 */
export function selectOutdatedOnline(
  verInfo: VersionStatus | null,
  liveMachines: Machine[] = []
): OutdatedTarget[] {
  const latest = verInfo?.latest ?? "";
  if (!latest) return [];

  const byId = new Map<string, VersionMachine>();
  for (const m of verInfo?.machines ?? []) {
    const id = m.local ? "" : m.id;
    byId.set(id, {
      id,
      name: m.name || (m.local ? "Hub" : id),
      version: m.version ?? "",
      os: m.os,
      local: Boolean(m.local),
      online: m.local ? true : Boolean(m.online),
    });
  }
  for (const m of liveMachines) {
    const id = m.local ? "" : m.id;
    const prev = byId.get(id);
    byId.set(id, {
      id,
      name: m.name || prev?.name || (m.local ? "Hub" : id),
      version: m.local ? m.version || verInfo?.version || prev?.version || "" : m.version || prev?.version || "",
      os: m.os || prev?.os,
      local: Boolean(m.local),
      online: m.local ? true : Boolean(m.online),
    });
  }

  const out: OutdatedTarget[] = [];
  if (byId.size > 0) {
    for (const m of byId.values()) {
      if ((m.local || m.online) && isOldVersion(m.version, latest)) {
        out.push({ id: m.id, name: m.name, version: m.version ?? "", local: m.local });
      }
    }
  } else {
    for (const o of verInfo?.outdated ?? []) {
      const local = o.id === "";
      if (isOldVersion(o.version, latest)) {
        out.push({ id: o.id, name: o.name || (local ? "Hub" : o.id), version: o.version, local });
      }
    }
  }

  out.sort((a, b) => (a.local === b.local ? a.name.localeCompare(b.name) : a.local ? 1 : -1));
  return out;
}

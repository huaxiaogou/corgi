import { useEffect, useState } from "react";
import { FolderOpen, Plus, Trash2 } from "lucide-react";
import { DirPicker } from "../../components/DirPicker";
import { normalizeMachineKey } from "../../lib/dispatchDefaults";
import type { Machine, ProjectWorkspace, Settings } from "../../types/api";

interface Props {
  settings: Settings;
  machines: Machine[];
  onSavePatch: (patch: Partial<Settings>, okMsg: string) => Promise<void>;
}

function makeProjectId(name: string, existing: ProjectWorkspace[]): string {
  const slug = name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9_-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 24);
  const base = slug || `proj-${existing.length + 1}`;
  let cand = base;
  let i = 2;
  const used = new Set(existing.map((p) => p.id));
  while (used.has(cand)) {
    cand = `${base}-${i++}`;
  }
  return cand;
}

export function ProjectsTab({ settings, machines, onSavePatch }: Props) {
  const [projects, setProjects] = useState<ProjectWorkspace[]>(() => settings.projects ?? []);
  const [aliasInputs, setAliasInputs] = useState<Record<string, string>>(() => {
    const init: Record<string, string> = {};
    for (const p of settings.projects ?? []) {
      init[p.id] = (p.aliases ?? []).join(", ");
    }
    return init;
  });
  const [browsing, setBrowsing] = useState<{ projId: string; machineKey: string; machineId: string } | null>(null);

  useEffect(() => {
    const list = settings.projects ?? [];
    setProjects(list);
    const nextAliases: Record<string, string> = {};
    for (const p of list) {
      nextAliases[p.id] = (p.aliases ?? []).join(", ");
    }
    setAliasInputs(nextAliases);
  }, [settings.projects]);

  const machineRows =
    machines.length > 0
      ? machines.map((m) => ({
          key: normalizeMachineKey(m.local ? "" : m.id),
          machineId: m.local ? "" : m.id,
          label: m.local ? `${m.name}（Hub）` : m.name,
          online: m.online,
        }))
      : [{ key: "hub", machineId: "", label: "Hub 本机", online: true }];

  const addProject = () => {
    const id = makeProjectId("project", projects);
    const next: ProjectWorkspace = { id, name: "", aliases: [], note: "", paths: {} };
    setProjects((prev) => [...prev, next]);
    setAliasInputs((prev) => ({ ...prev, [id]: "" }));
  };

  const updateProject = (id: string, fn: (p: ProjectWorkspace) => ProjectWorkspace) => {
    setProjects((prev) => prev.map((p) => (p.id === id ? fn(p) : p)));
  };

  const removeProject = (id: string) => {
    setProjects((prev) => prev.filter((p) => p.id !== id));
    if (browsing?.projId === id) setBrowsing(null);
  };

  const save = async () => {
    const cleaned: ProjectWorkspace[] = [];
    for (const p of projects) {
      const name = p.name.trim();
      if (!name) continue;
      const rawAliases = aliasInputs[p.id] ?? (p.aliases ?? []).join(", ");
      const aliases = rawAliases
        .split(/[,，、\n]/)
        .map((s) => s.trim())
        .filter(Boolean);
      const paths: Record<string, string> = {};
      for (const [k, v] of Object.entries(p.paths ?? {})) {
        const cleanV = v.trim();
        if (cleanV) paths[normalizeMachineKey(k)] = cleanV;
      }
      cleaned.push({
        id: p.id || makeProjectId(name, cleaned),
        name,
        aliases: aliases.length > 0 ? aliases : undefined,
        note: p.note?.trim() || undefined,
        paths: Object.keys(paths).length > 0 ? paths : undefined,
      });
    }
    await onSavePatch({ projects: cleaned }, "项目工作区已保存");
  };

  return (
    <div className="space-y-5">
      <section className="p-4 rounded-xl border border-hairline space-y-3">
        <div className="flex items-start justify-between gap-3">
          <div>
            <div className="font-semibold text-sm">项目工作区（跨电脑、跨工具通用）</div>
            <div className="text-[11.5px] text-muted mt-0.5 leading-relaxed">
              记住常用项目在每台电脑上的仓库根目录。无论派给 Antigravity、Claude Code、Codex 还是 CodeBuddy，Corgi
              都会自动切到对应目录，并附带你的原话与项目备注，不再从 <span className="font-mono-code">~</span> 盲找。
            </div>
          </div>
          <button
            type="button"
            onClick={addProject}
            className="px-2.5 py-1.5 rounded-lg border border-strong surface-bg surface-hover text-xs font-medium inline-flex items-center gap-1 shrink-0 cursor-pointer"
          >
            <Plus className="w-3.5 h-3.5" />
            <span>添加项目</span>
          </button>
        </div>

        {projects.length === 0 ? (
          <div className="p-6 rounded-xl border border-dashed border-hairline text-center text-muted space-y-2">
            <div>还没有登记项目工作区</div>
            <div className="text-[11px]">添加常用仓库后，在聊天输入框即可绑定或自动识别项目路径。</div>
          </div>
        ) : (
          <div className="space-y-3">
            {projects.map((p) => (
              <div key={p.id} className="p-3.5 rounded-xl border border-hairline surface-rail space-y-3">
                <div className="grid grid-cols-1 sm:grid-cols-2 gap-2.5 items-start">
                  <label className="space-y-1">
                    <span className="text-muted text-[11px]">项目名称</span>
                    <input
                      value={p.name}
                      onChange={(e) => updateProject(p.id, (cur) => ({ ...cur, name: e.target.value }))}
                      placeholder="例如：物言 Agent / Corgi"
                      className="w-full px-2.5 py-1.5 rounded-lg surface-bg border border-hairline outline-none"
                    />
                  </label>
                  <div className="flex items-end gap-2">
                    <label className="space-y-1 flex-1 min-w-0">
                      <span className="text-muted text-[11px]">识别别名（逗号分隔，可选）</span>
                      <input
                        value={aliasInputs[p.id] ?? ""}
                        onChange={(e) => setAliasInputs((prev) => ({ ...prev, [p.id]: e.target.value }))}
                        placeholder="物言, wuyan, agentic-mall"
                        className="w-full px-2.5 py-1.5 rounded-lg surface-bg border border-hairline outline-none"
                      />
                    </label>
                    <button
                      type="button"
                      onClick={() => removeProject(p.id)}
                      className="h-[34px] w-[34px] rounded-lg border border-hairline surface-bg surface-hover flex items-center justify-center text-muted hover:text-primary cursor-pointer shrink-0"
                      title="删除项目"
                    >
                      <Trash2 className="w-3.5 h-3.5" />
                    </button>
                  </div>
                </div>

                <div className="space-y-2">
                  <div className="text-[11px] text-muted">各台电脑对应的仓库目录</div>
                  {machineRows.map((mr) => {
                    const val = p.paths?.[mr.key] ?? "";
                    const isBrowsing = browsing?.projId === p.id && browsing.machineKey === mr.key;
                    return (
                      <div key={mr.key} className="space-y-1.5">
                        <div className="flex flex-col sm:flex-row sm:items-center gap-1.5 sm:gap-2">
                          <span className="sm:w-40 text-[11.5px] text-secondary truncate shrink-0">
                            {mr.label}
                            {!mr.online ? "（离线）" : ""}
                          </span>
                          <div className="flex gap-1.5 flex-1 min-w-0">
                            <input
                              value={val}
                              onChange={(e) =>
                                updateProject(p.id, (cur) => ({
                                  ...cur,
                                  paths: { ...(cur.paths ?? {}), [mr.key]: e.target.value },
                                }))
                              }
                              placeholder="留空表示该电脑无此项目"
                              className="flex-1 min-w-0 px-2.5 py-1.5 rounded-lg surface-bg border border-hairline font-mono-code text-[11.5px] outline-none"
                            />
                            {mr.online && (
                              <button
                                type="button"
                                onClick={() =>
                                  setBrowsing(isBrowsing ? null : { projId: p.id, machineKey: mr.key, machineId: mr.machineId })
                                }
                                className="px-2.5 py-1.5 rounded-lg border border-hairline surface-bg surface-hover text-secondary inline-flex items-center gap-1 cursor-pointer shrink-0"
                                title="浏览目录"
                              >
                                <FolderOpen className="w-3.5 h-3.5" />
                                <span className="text-[11px]">浏览</span>
                              </button>
                            )}
                          </div>
                        </div>
                        {isBrowsing && (
                          <DirPicker
                            value={val || "~"}
                            machine={mr.machineId}
                            onChange={(dir) =>
                              updateProject(p.id, (cur) => ({
                                ...cur,
                                paths: { ...(cur.paths ?? {}), [mr.key]: dir },
                              }))
                            }
                          />
                        )}
                      </div>
                    );
                  })}
                </div>

                <label className="block space-y-1">
                  <span className="text-muted text-[11px]">项目默认约束 / 备注（派活时自动附给 Worker，可选）</span>
                  <input
                    value={p.note ?? ""}
                    onChange={(e) => updateProject(p.id, (cur) => ({ ...cur, note: e.target.value }))}
                    placeholder="例如：默认只读调研不改共享 DB / Nacos；改代码走独立分支"
                    className="w-full px-2.5 py-1.5 rounded-lg surface-bg border border-hairline outline-none"
                  />
                </label>
              </div>
            ))}
          </div>
        )}
      </section>

      <button type="button" onClick={() => void save()} className="px-4 py-2 rounded-lg btn-accent font-medium cursor-pointer">
        保存项目工作区
      </button>
    </div>
  );
}

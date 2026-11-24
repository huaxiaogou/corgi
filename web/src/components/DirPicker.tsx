import { useEffect, useState } from "react";
import { ChevronUp, Folder, Home } from "lucide-react";
import { api } from "../lib/api";

interface DirListing {
  path: string;
  display: string;
  parent: string;
  dirs: string[];
}

interface Props {
  machine: string;
  value: string;
  onChange: (path: string) => void;
}

const RECENT_KEY = "corgi.recentDirs";

export function rememberRecentDir(machine: string, dir: string) {
  const clean = dir.trim();
  if (!clean) return;
  try {
    const map = JSON.parse(localStorage.getItem(RECENT_KEY) || "{}") as Record<string, string[]>;
    const key = machine || "hub";
    const next = [clean, ...(map[key] ?? []).filter((d) => d !== clean)].slice(0, 6);
    map[key] = next;
    localStorage.setItem(RECENT_KEY, JSON.stringify(map));
  } catch {
    // ignore storage errors
  }
}

function readRecentDirs(machine: string): string[] {
  try {
    const map = JSON.parse(localStorage.getItem(RECENT_KEY) || "{}") as Record<string, string[]>;
    return map[machine || "hub"] ?? [];
  } catch {
    return [];
  }
}

export function DirPicker({ machine, value, onChange }: Props) {
  const [listing, setListing] = useState<DirListing | null>(null);
  const [err, setErr] = useState("");
  const [recent, setRecent] = useState<string[]>([]);

  useEffect(() => {
    setRecent(readRecentDirs(machine));
  }, [machine]);

  useEffect(() => {
    let active = true;
    const t = window.setTimeout(() => {
      const qs = new URLSearchParams({ path: value || "~" });
      if (machine) qs.set("machine", machine);
      void api<DirListing>(`/api/dirs?${qs.toString()}`)
        .then((res) => {
          if (!active) return;
          setListing(res);
          setErr("");
        })
        .catch((e) => {
          if (!active) return;
          setListing(null);
          setErr(e instanceof Error ? e.message : "无法读取目录");
        });
    }, 180);
    return () => {
      active = false;
      window.clearTimeout(t);
    };
  }, [machine, value]);

  const enterChild = (name: string) => {
    const base = listing?.path || value || "~";
    const joined = base.endsWith("/") ? `${base}${name}` : `${base}/${name}`;
    onChange(joined);
  };

  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between">
        <label className="text-secondary">项目目录</label>
        <div className="flex items-center gap-1.5">
          <button
            type="button"
            onClick={() => onChange("~")}
            className="inline-flex items-center gap-1 px-2 py-0.5 rounded border border-hairline surface-rail text-secondary surface-hover cursor-pointer"
            title="回到主目录"
          >
            <Home className="w-3 h-3" />
            <span>~</span>
          </button>
          <button
            type="button"
            disabled={!listing || listing.parent === listing.path}
            onClick={() => listing && onChange(listing.parent)}
            className="inline-flex items-center gap-0.5 px-2 py-0.5 rounded border border-hairline surface-rail text-secondary surface-hover cursor-pointer disabled:opacity-40"
            title="返回上一级"
          >
            <ChevronUp className="w-3 h-3" />
            <span>上一级</span>
          </button>
        </div>
      </div>

      <input
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="输入路径或从下方点选目录"
        className="w-full px-3 py-2 rounded-lg font-mono-code surface-rail border border-hairline outline-none"
      />

      {recent.length > 0 && (
        <div className="flex flex-wrap gap-1">
          {recent.map((d) => (
            <button
              key={d}
              type="button"
              onClick={() => onChange(d)}
              className="px-2 py-0.5 rounded-md border border-hairline surface-rail font-mono-code text-[11px] text-secondary surface-hover cursor-pointer truncate max-w-[200px]"
              title={d}
            >
              {d}
            </button>
          ))}
        </div>
      )}

      <div className="rounded-lg border border-hairline surface-rail max-h-32 overflow-y-auto divide-y divide-[var(--line)]">
        {err ? (
          <div className="px-3 py-2 text-muted">{err}</div>
        ) : !listing ? (
          <div className="px-3 py-2 text-muted">读取目录中…</div>
        ) : listing.dirs.length === 0 ? (
          <div className="px-3 py-2 text-muted">当前目录下没有子文件夹</div>
        ) : (
          listing.dirs.map((d) => (
            <button
              key={d}
              type="button"
              onClick={() => enterChild(d)}
              className="w-full px-3 py-1.5 flex items-center gap-2 text-left font-mono-code text-secondary surface-hover cursor-pointer"
            >
              <Folder className="w-3.5 h-3.5 shrink-0 text-muted" strokeWidth={1.6} />
              <span className="truncate">{d}</span>
            </button>
          ))
        )}
      </div>
    </div>
  );
}

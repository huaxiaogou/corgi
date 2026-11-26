import { useEffect, useState } from "react";
import type { MemorySnapshot, Settings } from "../../types/api";

interface Props {
  memory: MemorySnapshot;
  settings: Settings | null;
  onSaveFile: (path: string, text: string) => Promise<void>;
  onSaveSettingsPatch: (patch: Partial<Settings>, okMsg: string) => Promise<void>;
  onStartDream: () => Promise<void>;
}

export function MemoryTab({ memory, settings, onSaveFile, onSaveSettingsPatch, onStartDream }: Props) {
  const [section, setSection] = useState<"files" | "dreams">("files");
  const [activeMem, setActiveMem] = useState("instructions.md");
  const [draft, setDraft] = useState("");
  const [activeDream, setActiveDream] = useState("");
  const [dreamAt, setDreamAt] = useState(settings?.dream_at ?? "04:00");
  const [rotatePct, setRotatePct] = useState(
    settings?.rotate_pct === undefined || settings.rotate_pct === 0 ? 70 : settings.rotate_pct
  );

  useEffect(() => {
    const picked = memory.files.find((f) => f.name === activeMem) ?? memory.files[0];
    if (picked) {
      setActiveMem(picked.name);
      setDraft(picked.text);
    }
    if (!activeDream && memory.dreams.length > 0) {
      setActiveDream(memory.dreams[0].name);
    }
  }, [memory]);

  useEffect(() => {
    if (!settings) return;
    setDreamAt(settings.dream_at ?? "04:00");
    setRotatePct(settings.rotate_pct === undefined || settings.rotate_pct === 0 ? 70 : settings.rotate_pct);
  }, [settings]);

  const budgetPct = Math.min(100, Math.round((memory.up_front / Math.max(memory.budget, 1)) * 100));
  const selectedDream = memory.dreams.find((d) => d.name === activeDream) ?? memory.dreams[0];

  return (
    <div className="space-y-4">
      <div className="p-4 rounded-xl border border-hairline space-y-3">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="space-y-1">
            <div className="font-medium">
              首屏预算 {(memory.up_front / 1024).toFixed(1)} KB / {(memory.budget / 1024).toFixed(0)} KB（{budgetPct}%）
            </div>
            <div className="text-muted font-mono-code">
              目录 {memory.dir}
              {memory.last_dream ? ` · 上次做梦 ${new Date(memory.last_dream).toLocaleString()}` : ""}
              {memory.next_dream ? ` · 下次 ${new Date(memory.next_dream).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}` : ""}
            </div>
          </div>
          <button
            type="button"
            disabled={Boolean(memory.dreaming)}
            onClick={() => void onStartDream()}
            className="px-3.5 py-1.5 rounded-lg border border-strong font-medium cursor-pointer disabled:opacity-50 surface-hover"
          >
            {memory.dreaming ? "做梦整理中…" : "立即做梦整理"}
          </button>
        </div>

        {settings && (
          <div className="pt-3 border-t border-hairline flex flex-wrap items-end gap-3">
            <label className="space-y-1">
              <span className="text-muted block">夜间做梦时间</span>
              <input
                placeholder="04:00 或 off"
                value={dreamAt}
                onChange={(e) => setDreamAt(e.target.value)}
                className="w-36 px-3 py-1.5 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
              />
            </label>
            <label className="space-y-1">
              <span className="text-muted block">上下文交接阈值 (%，-1 关闭)</span>
              <input
                type="number"
                min={-1}
                max={95}
                value={rotatePct}
                onChange={(e) => setRotatePct(Number(e.target.value))}
                className="w-40 px-3 py-1.5 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
              />
            </label>
            <button
              type="button"
              onClick={() =>
                void onSaveSettingsPatch({ dream_at: dreamAt.trim() || "04:00", rotate_pct: rotatePct }, "做梦与交接配置已保存")
              }
              className="px-3.5 py-1.5 rounded-lg btn-accent font-medium cursor-pointer"
            >
              保存配置
            </button>
          </div>
        )}
      </div>

      <div className="inline-flex p-0.5 rounded-lg surface-rail border border-hairline">
        <button
          type="button"
          onClick={() => setSection("files")}
          className={`px-3 py-1 rounded-md cursor-pointer ${section === "files" ? "surface-bg font-medium text-primary" : "text-secondary"}`}
        >
          记忆文件 ({memory.files.length})
        </button>
        <button
          type="button"
          onClick={() => setSection("dreams")}
          className={`px-3 py-1 rounded-md cursor-pointer ${section === "dreams" ? "surface-bg font-medium text-primary" : "text-secondary"}`}
        >
          做梦日记 ({memory.dreams.length})
        </button>
      </div>

      {section === "files" ? (
        <div className="space-y-3">
          <div className="flex flex-wrap gap-1.5">
            {memory.files.map((f) => (
              <button
                key={f.name}
                type="button"
                onClick={() => {
                  setActiveMem(f.name);
                  setDraft(f.text);
                }}
                className={`px-2.5 py-1 rounded-lg font-mono-code border cursor-pointer ${
                  activeMem === f.name ? "border-strong surface-rail font-medium" : "border-hairline text-secondary"
                }`}
              >
                {f.name}
              </button>
            ))}
          </div>
          <textarea
            rows={14}
            value={draft}
            readOnly={activeMem === "messages.md"}
            onChange={(e) => setDraft(e.target.value)}
            className="w-full p-3.5 rounded-xl font-mono-code surface-rail border border-hairline outline-none leading-relaxed"
          />
          {activeMem !== "messages.md" && (
            <button type="button" onClick={() => void onSaveFile(activeMem, draft)} className="px-4 py-2 rounded-lg btn-accent font-medium cursor-pointer">
              保存 {activeMem}
            </button>
          )}
        </div>
      ) : (
        <div className="space-y-3">
          {memory.dreams.length === 0 ? (
            <div className="p-4 rounded-xl border border-hairline text-muted">暂无做梦日记；夜间定时或点击「立即做梦整理」后会生成记录。</div>
          ) : (
            <>
              <div className="flex flex-wrap gap-1.5">
                {memory.dreams.map((d) => (
                  <button
                    key={d.name}
                    type="button"
                    onClick={() => setActiveDream(d.name)}
                    className={`px-2.5 py-1 rounded-lg font-mono-code border cursor-pointer ${
                      selectedDream?.name === d.name ? "border-strong surface-rail font-medium" : "border-hairline text-secondary"
                    }`}
                  >
                    {d.name}
                  </button>
                ))}
              </div>
              {selectedDream && (
                <pre className="p-4 rounded-xl font-mono-code surface-rail border border-hairline whitespace-pre-wrap leading-relaxed">
                  {selectedDream.text}
                </pre>
              )}
            </>
          )}
        </div>
      )}
    </div>
  );
}

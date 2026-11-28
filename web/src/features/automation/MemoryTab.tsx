import { useEffect, useMemo, useRef, useState } from "react";
import { RotateCcw, Search, Trash2, Pencil, ChevronDown, ChevronUp } from "lucide-react";
import type { MemorySnapshot, Settings } from "../../types/api";
import {
  extractDreamSummary,
  parseInstructions,
  removeInstructionLine,
  searchMemoryFiles,
} from "../../lib/memoryHelpers";

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
  const [instructionsMode, setInstructionsMode] = useState<"rules" | "raw">("rules");
  const [draft, setDraft] = useState("");
  const [searchQuery, setSearchQuery] = useState("");
  const [activeDream, setActiveDream] = useState("");
  const [dreamSummaryExpanded, setDreamSummaryExpanded] = useState(false);
  const [dreamAt, setDreamAt] = useState(settings?.dream_at ?? "04:00");
  const [undoState, setUndoState] = useState<{ prevText: string; label: string } | null>(null);
  const undoTimerRef = useRef<number | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);

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
  }, [settings]);

  useEffect(() => {
    return () => {
      if (undoTimerRef.current !== null) {
        window.clearTimeout(undoTimerRef.current);
      }
    };
  }, []);

  const budgetPct = Math.min(100, Math.round((memory.up_front / Math.max(memory.budget, 1)) * 100));
  const selectedDream = memory.dreams.find((d) => d.name === activeDream) ?? memory.dreams[0];
  const latestDream = memory.dreams[0];
  const latestDreamSummary = useMemo(
    () => (latestDream ? extractDreamSummary(latestDream.text) : null),
    [latestDream?.name, latestDream?.text]
  );

  const parsedInstructions = useMemo(
    () => (activeMem === "instructions.md" ? parseInstructions(draft) : null),
    [activeMem, draft]
  );

  const searchHits = useMemo(() => searchMemoryFiles(memory.files, searchQuery), [memory.files, searchQuery]);

  const jumpToLine = (fileName: string, lineIndex: number) => {
    const target = memory.files.find((f) => f.name === fileName);
    if (!target) return;
    setSection("files");
    setActiveMem(fileName);
    setDraft(target.text);
    if (fileName === "instructions.md") {
      setInstructionsMode("raw");
    }
    window.requestAnimationFrame(() => {
      const el = textareaRef.current;
      if (!el) return;
      const lines = target.text.split(/\r?\n/);
      let start = 0;
      for (let i = 0; i < Math.min(lineIndex, lines.length); i++) {
        start += lines[i].length + 1;
      }
      const end = start + (lines[lineIndex]?.length ?? 0);
      el.focus();
      el.setSelectionRange(start, end);
    });
  };

  const handleDeleteInferred = async (lineIndex: number, label: string) => {
    const prevText = draft;
    const nextText = removeInstructionLine(prevText, lineIndex);
    setDraft(nextText);
    await onSaveFile("instructions.md", nextText);
    if (undoTimerRef.current !== null) {
      window.clearTimeout(undoTimerRef.current);
    }
    setUndoState({ prevText, label });
    undoTimerRef.current = window.setTimeout(() => {
      setUndoState(null);
      undoTimerRef.current = null;
    }, 12000);
  };

  const handleUndoDelete = async () => {
    if (!undoState) return;
    const prev = undoState.prevText;
    if (undoTimerRef.current !== null) {
      window.clearTimeout(undoTimerRef.current);
      undoTimerRef.current = null;
    }
    setUndoState(null);
    setDraft(prev);
    await onSaveFile("instructions.md", prev);
  };

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

        {latestDream && latestDreamSummary && latestDreamSummary.lead && (
          <div className="pt-2.5 border-t border-hairline space-y-1.5 text-xs">
            <div className="flex items-start justify-between gap-2">
              <div className="min-w-0">
                <span className="text-muted mr-1.5">最近做梦 ({latestDream.name.replace(/\.md$/, "")}):</span>
                <span className="text-secondary">{latestDreamSummary.lead}</span>
              </div>
              <button
                type="button"
                onClick={() => setDreamSummaryExpanded((v) => !v)}
                className="inline-flex items-center gap-0.5 text-muted hover:text-primary shrink-0 cursor-pointer"
              >
                {dreamSummaryExpanded ? (
                  <>
                    收起 <ChevronUp className="w-3.5 h-3.5" />
                  </>
                ) : (
                  <>
                    展开日记 <ChevronDown className="w-3.5 h-3.5" />
                  </>
                )}
              </button>
            </div>
            {!dreamSummaryExpanded && latestDreamSummary.bullets.length > 0 && (
              <ul className="pl-4 list-disc text-muted space-y-0.5">
                {latestDreamSummary.bullets.map((b, idx) => (
                  <li key={idx} className="truncate">
                    {b}
                  </li>
                ))}
              </ul>
            )}
            {dreamSummaryExpanded && (
              <pre className="mt-1 p-3 rounded-lg font-mono-code surface-rail border border-hairline whitespace-pre-wrap leading-relaxed text-[12px]">
                {latestDream.text}
              </pre>
            )}
          </div>
        )}

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
            <button
              type="button"
              onClick={() =>
                void onSaveSettingsPatch({ dream_at: dreamAt.trim() || "04:00" }, "做梦配置已保存")
              }
              className="px-3.5 py-1.5 rounded-lg btn-accent font-medium cursor-pointer"
            >
              保存配置
            </button>
          </div>
        )}
      </div>

      <div className="flex flex-wrap items-center justify-between gap-2">
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

        {section === "files" && (
          <div className="relative flex-1 min-w-[200px] max-w-xs">
            <Search className="w-3.5 h-3.5 text-muted absolute left-2.5 top-1/2 -translate-y-1/2 pointer-events-none" />
            <input
              type="search"
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
              placeholder="搜索记忆文件与规则…"
              className="w-full pl-8 pr-3 py-1.5 rounded-lg surface-rail border border-hairline text-xs outline-none"
            />
          </div>
        )}
      </div>

      {section === "files" ? (
        <div className="space-y-3">
          {searchQuery.trim() && (
            <div className="p-3 rounded-xl border border-hairline surface-rail space-y-2 text-xs">
              <div className="text-muted">
                匹配 {searchHits.length} 个文件
              </div>
              {searchHits.length === 0 ? (
                <div className="text-muted">未找到包含「{searchQuery.trim()}」的记忆内容。</div>
              ) : (
                <div className="space-y-2">
                  {searchHits.map((hit) => (
                    <div key={hit.name} className="space-y-1">
                      <button
                        type="button"
                        onClick={() => {
                          jumpToLine(hit.name, Math.max(0, (hit.matches[0]?.line ?? 1) - 1));
                          setSearchQuery("");
                        }}
                        className="font-mono-code font-medium text-primary hover:underline cursor-pointer"
                      >
                        {hit.name}
                      </button>
                      {hit.matches.map((m) => (
                        <div key={`${hit.name}:${m.line}`} className="text-secondary pl-3 truncate">
                          <span className="text-muted font-mono-code mr-1.5">L{m.line}</span>
                          {m.text}
                        </div>
                      ))}
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}

          <div className="flex flex-wrap items-center justify-between gap-2">
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

            {activeMem === "instructions.md" && (
              <div className="inline-flex p-0.5 rounded-lg surface-rail border border-hairline text-xs">
                <button
                  type="button"
                  onClick={() => setInstructionsMode("rules")}
                  className={`px-2.5 py-1 rounded-md cursor-pointer ${instructionsMode === "rules" ? "surface-bg font-medium text-primary" : "text-secondary"}`}
                >
                  规则视图
                </button>
                <button
                  type="button"
                  onClick={() => setInstructionsMode("raw")}
                  className={`px-2.5 py-1 rounded-md cursor-pointer ${instructionsMode === "raw" ? "surface-bg font-medium text-primary" : "text-secondary"}`}
                >
                  原文编辑
                </button>
              </div>
            )}
          </div>

          {undoState && (
            <div className="flex items-center justify-between gap-2 px-3 py-2 rounded-lg border border-strong surface-rail text-xs">
              <span className="text-secondary truncate">已删除推断规则：{undoState.label}</span>
              <button
                type="button"
                onClick={() => void handleUndoDelete()}
                className="inline-flex items-center gap-1 px-2.5 py-1 rounded-md border border-hairline surface-bg text-primary font-medium cursor-pointer shrink-0"
              >
                <RotateCcw className="w-3 h-3" /> 撤销
              </button>
            </div>
          )}

          {activeMem === "instructions.md" && instructionsMode === "rules" && parsedInstructions ? (
            <div className="space-y-4">
              <div className="p-3.5 rounded-xl border border-hairline space-y-2">
                <div className="flex items-center justify-between">
                  <div className="text-xs font-medium text-primary">
                    明确指令（{parsedInstructions.explicit.length}）
                  </div>
                  <span className="text-[11px] text-muted">你显式提出并保留的长期偏好</span>
                </div>
                {parsedInstructions.explicit.length === 0 ? (
                  <div className="text-xs text-muted">暂无明确指令。</div>
                ) : (
                  <ul className="divide-y divide-[var(--border-hairline)]">
                    {parsedInstructions.explicit.map((item) => (
                      <li key={item.lineIndex} className="py-2 flex items-start justify-between gap-3 text-xs">
                        <span className="text-primary leading-relaxed">{item.text}</span>
                        <button
                          type="button"
                          onClick={() => jumpToLine("instructions.md", item.lineIndex)}
                          title="编辑该行"
                          className="inline-flex items-center gap-1 px-2 py-0.5 rounded border border-hairline text-muted hover:text-primary surface-hover shrink-0 cursor-pointer"
                        >
                          <Pencil className="w-3 h-3" /> 编辑
                        </button>
                      </li>
                    ))}
                  </ul>
                )}
              </div>

              <div className="p-3.5 rounded-xl border border-hairline space-y-2">
                <div className="flex items-center justify-between">
                  <div className="text-xs font-medium text-primary">
                    AI 自动推断（{parsedInstructions.inferred.length}）
                  </div>
                  <span className="text-[11px] text-muted">带 (inferred) 标记，不需要可直接一键删除</span>
                </div>
                {parsedInstructions.inferred.length === 0 ? (
                  <div className="text-xs text-muted">暂无 AI 推断规则。</div>
                ) : (
                  <ul className="divide-y divide-[var(--border-hairline)]">
                    {parsedInstructions.inferred.map((item) => (
                      <li key={item.lineIndex} className="py-2 flex items-start justify-between gap-3 text-xs">
                        <span className="text-secondary leading-relaxed">{item.text}</span>
                        <div className="flex items-center gap-1.5 shrink-0">
                          <button
                            type="button"
                            onClick={() => jumpToLine("instructions.md", item.lineIndex)}
                            title="编辑该行"
                            className="inline-flex items-center gap-1 px-2 py-0.5 rounded border border-hairline text-muted hover:text-primary surface-hover cursor-pointer"
                          >
                            <Pencil className="w-3 h-3" /> 编辑
                          </button>
                          <button
                            type="button"
                            onClick={() => void handleDeleteInferred(item.lineIndex, item.text)}
                            title="一键删除该推断规则"
                            className="inline-flex items-center gap-1 px-2 py-0.5 rounded border border-hairline text-muted hover:text-primary surface-hover cursor-pointer"
                          >
                            <Trash2 className="w-3 h-3" /> 删除
                          </button>
                        </div>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </div>
          ) : (
            <>
              <textarea
                ref={textareaRef}
                rows={14}
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                className="w-full p-3.5 rounded-xl font-mono-code surface-rail border border-hairline outline-none leading-relaxed"
              />
              <button type="button" onClick={() => void onSaveFile(activeMem, draft)} className="px-4 py-2 rounded-lg btn-accent font-medium cursor-pointer">
                保存 {activeMem}
              </button>
            </>
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

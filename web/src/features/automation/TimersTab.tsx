import { useState } from "react";
import type { Timer } from "../../types/api";

interface Props {
  timers: Timer[];
  onAdd: (spec: string, text: string) => Promise<void>;
  onDelete: (id: string) => Promise<void>;
}

export function TimersTab({ timers, onAdd, onDelete }: Props) {
  const [spec, setSpec] = useState("daily 09:00");
  const [text, setText] = useState("");

  const submit = async () => {
    if (!text.trim()) return;
    await onAdd(spec, text.trim());
    setText("");
  };

  return (
    <div className="space-y-4">
      <div className="flex gap-2">
        <input
          value={spec}
          onChange={(e) => setSpec(e.target.value)}
          className="w-40 px-3 py-2 rounded-lg font-mono-code surface-rail border border-hairline outline-none"
        />
        <input
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder="定时任务内容，例如：汇总昨日慢查询"
          className="flex-1 px-3 py-2 rounded-lg surface-rail border border-hairline outline-none"
        />
        <button type="button" onClick={() => void submit()} className="px-3.5 py-2 rounded-lg btn-accent font-medium cursor-pointer">
          添加
        </button>
      </div>
      <div className="rounded-xl border border-hairline divide-y divide-[var(--line)]">
        {timers.length === 0 && <div className="p-4 text-muted">暂无定时器</div>}
        {timers.map((tm) => (
          <div key={tm.id} className="px-4 py-3 flex items-center justify-between gap-2">
            <div>
              <span className="font-mono-code px-1.5 py-0.5 rounded surface-rail mr-2">{tm.spec}</span>
              <span>{tm.text}</span>
            </div>
            <button type="button" onClick={() => void onDelete(tm.id)} className="text-muted hover:underline cursor-pointer">
              删除
            </button>
          </div>
        ))}
      </div>
    </div>
  );
}

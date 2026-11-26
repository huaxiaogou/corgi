import { useEffect, useState } from "react";
import type { Settings, Timer } from "../../types/api";

interface Props {
  timers: Timer[];
  settings: Settings | null;
  onAdd: (spec: string, text: string) => Promise<void>;
  onDelete: (id: string) => Promise<void>;
  onSaveSettingsPatch: (patch: Partial<Settings>, okMsg: string) => Promise<void>;
}

export function TimersTab({ timers, settings, onAdd, onDelete, onSaveSettingsPatch }: Props) {
  const [spec, setSpec] = useState("daily 09:00");
  const [text, setText] = useState("");
  const [idlePatrolMinutes, setIdlePatrolMinutes] = useState(settings?.idle_patrol_minutes ?? 30);
  const [activeHours, setActiveHours] = useState(settings?.active_hours ?? "09:00-22:30");
  const [staleArchiveDays, setStaleArchiveDays] = useState(settings?.stale_archive_days ?? 3);
  const [morningDigest, setMorningDigest] = useState(settings?.morning_digest !== false);

  useEffect(() => {
    if (!settings) return;
    setIdlePatrolMinutes(settings.idle_patrol_minutes ?? 30);
    setActiveHours(settings.active_hours ?? "09:00-22:30");
    setStaleArchiveDays(settings.stale_archive_days ?? 3);
    setMorningDigest(settings.morning_digest !== false);
  }, [settings]);

  const submit = async () => {
    if (!text.trim()) return;
    await onAdd(spec, text.trim());
    setText("");
  };

  return (
    <div className="space-y-5">
      <section className="p-4 rounded-xl border border-hairline space-y-3">
        <div>
          <div className="font-semibold text-sm">自定义定时任务</div>
          <div className="text-[11.5px] text-muted mt-0.5">
            支持 <span className="font-mono-code">daily 09:00</span>、<span className="font-mono-code">weekdays 09:00</span>、
            <span className="font-mono-code">every 2h</span>、<span className="font-mono-code">in 30m</span> 等语法。
          </div>
        </div>
        <div className="flex flex-wrap gap-2">
          <input
            value={spec}
            onChange={(e) => setSpec(e.target.value)}
            className="w-40 px-3 py-2 rounded-lg font-mono-code surface-rail border border-hairline outline-none"
          />
          <input
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder="定时任务内容，例如：汇总昨日慢查询"
            className="flex-1 min-w-[200px] px-3 py-2 rounded-lg surface-rail border border-hairline outline-none"
          />
          <button type="button" onClick={() => void submit()} className="px-3.5 py-2 rounded-lg btn-accent font-medium cursor-pointer">
            添加
          </button>
        </div>
        <div className="rounded-xl border border-hairline divide-y divide-[var(--line)]">
          {timers.length === 0 && <div className="p-3.5 text-muted">暂无定时器</div>}
          {timers.map((tm) => (
            <div key={tm.id} className="px-3.5 py-2.5 flex items-center justify-between gap-2">
              <div>
                <span className="font-mono-code px-1.5 py-0.5 rounded surface-rail mr-2">{tm.spec}</span>
                <span>{tm.text}</span>
              </div>
              <button type="button" onClick={() => void onDelete(tm.id)} className="text-muted hover:underline cursor-pointer shrink-0">
                删除
              </button>
            </div>
          ))}
        </div>
      </section>

      {settings && (
        <section className="p-4 rounded-xl border border-hairline space-y-3">
          <div>
            <div className="font-semibold text-sm">空闲巡检与晨间待办简报</div>
            <div className="text-[11.5px] text-muted mt-0.5">
              Hub 先做零成本门控，仅在活跃时段内检查持续空闲会话；无待办则静默跳过，多任务自动合并为 1 条摘要卡。
            </div>
          </div>
          <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
            <label className="space-y-1">
              <span className="text-muted">首次空闲巡检 (分钟，-1 关闭)</span>
              <input
                type="number"
                min={-1}
                max={480}
                value={idlePatrolMinutes}
                onChange={(e) => setIdlePatrolMinutes(Number(e.target.value))}
                className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
              />
            </label>
            <label className="space-y-1">
              <span className="text-muted">活跃免打扰时段 (如 09:00-22:30 / 24h)</span>
              <input
                placeholder="09:00-22:30"
                value={activeHours}
                onChange={(e) => setActiveHours(e.target.value)}
                className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
              />
            </label>
            <label className="space-y-1">
              <span className="text-muted">旧会话清理提示 (天，-1 关闭)</span>
              <input
                type="number"
                min={-1}
                max={60}
                value={staleArchiveDays}
                onChange={(e) => setStaleArchiveDays(Number(e.target.value))}
                className="w-full px-3 py-2 rounded-lg surface-rail border border-hairline font-mono-code outline-none"
              />
            </label>
          </div>
          <label className="flex items-center gap-2 cursor-pointer">
            <input type="checkbox" checked={morningDigest} onChange={(e) => setMorningDigest(e.target.checked)} />
            <span>开启每日晨间汇总简报（在活跃时段起点汇总跨天待办与超期旧会话，全部干净时自动跳过）</span>
          </label>
          <div className="px-3 py-2 rounded-lg surface-rail border border-hairline text-[11.5px] text-secondary space-y-0.5">
            <div>
              当天退避：空闲满{" "}
              <span className="font-mono-code text-primary">
                {idlePatrolMinutes < 0 ? "已关闭" : `${idlePatrolMinutes || 30}m`}
              </span>{" "}
              第 1 次 → 隔 <span className="font-mono-code text-primary">4h</span> 第 2 次 → 当天静默
            </div>
            <div>
              跨天防忘：次日{" "}
              <span className="font-mono-code text-primary">{(activeHours || "09:00-22:30").split("-")[0] || "09:00"}</span>{" "}
              发唯一 1 条晨报；手动会话与常驻保留会话永久豁免归档
            </div>
          </div>
          <button
            type="button"
            onClick={() =>
              void onSaveSettingsPatch(
                {
                  idle_patrol_minutes: idlePatrolMinutes,
                  active_hours: activeHours.trim() || "09:00-22:30",
                  stale_archive_days: staleArchiveDays,
                  morning_digest: morningDigest,
                },
                "巡检与晨报设置已保存"
              )
            }
            className="px-4 py-1.5 rounded-lg btn-accent font-medium cursor-pointer"
          >
            保存巡检设置
          </button>
        </section>
      )}
    </div>
  );
}

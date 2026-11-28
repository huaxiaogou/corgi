import { CorgiMark } from "./CorgiMark";

/**
 * Ultra-quiet monochrome wireframe shell displayed strictly BEFORE /api/session confirms auth.
 * Contains zero cached conversation text, worker paths, or attachment metadata.
 */
export function AppShellSkeleton() {
  return (
    <div
      aria-busy="true"
      aria-label="正在验证会话"
      className="h-full flex flex-col md:flex-row overflow-hidden surface-bg text-primary select-none"
    >
      <div className="md:hidden h-12 px-3 flex items-center justify-between border-b border-hairline surface-rail shrink-0">
        <div className="w-7 h-7 rounded-lg surface-active" />
        <div className="flex items-center gap-2 text-sm font-semibold text-secondary">
          <CorgiMark className="w-4 h-4" />
          <span>Corgi</span>
        </div>
        <div className="w-7 h-7 rounded-lg surface-active" />
      </div>

      <aside className="hidden md:flex w-[252px] shrink-0 flex-col surface-rail border-r border-hairline">
        <div className="h-14 px-4 flex items-center gap-2.5">
          <div className="w-7 h-7 rounded-lg flex items-center justify-center surface-bg border border-hairline">
            <CorgiMark className="w-[18px] h-[18px]" />
          </div>
          <span className="font-semibold text-[15px] tracking-tight">Corgi</span>
        </div>
        <div className="px-3 pt-2 space-y-2.5">
          <div className="h-10 rounded-xl surface-active" />
          <div className="pt-3 space-y-2">
            <div className="h-3 w-16 rounded surface-active" />
            <div className="h-9 rounded-xl surface-bg border border-hairline" />
            <div className="h-9 rounded-xl surface-bg border border-hairline" />
          </div>
        </div>
      </aside>

      <main className="flex-1 flex flex-col min-w-0 min-h-0">
        <div className="h-14 px-5 flex items-center justify-between border-b border-hairline shrink-0">
          <div className="space-y-1.5">
            <div className="h-4 w-20 rounded surface-active" />
            <div className="h-3 w-32 rounded surface-rail" />
          </div>
          <div className="flex items-center gap-1.5">
            <div className="w-8 h-8 rounded-lg surface-rail" />
            <div className="w-8 h-8 rounded-lg surface-rail" />
          </div>
        </div>
        <div className="flex-1 px-5 py-8">
          <div className="max-w-[740px] mx-auto space-y-6">
            <div className="flex items-start gap-3">
              <div className="w-7 h-7 rounded-lg border border-hairline shrink-0" />
              <div className="flex-1 space-y-2 pt-1">
                <div className="h-3.5 w-3/4 rounded surface-rail" />
                <div className="h-3.5 w-1/2 rounded surface-rail" />
              </div>
            </div>
            <div className="flex justify-end">
              <div className="h-9 w-44 rounded-2xl surface-rail" />
            </div>
          </div>
        </div>
        <div className="px-5 pb-4 pt-2 shrink-0">
          <div className="max-w-[740px] mx-auto h-14 rounded-2xl border border-hairline surface-rail" />
        </div>
      </main>
    </div>
  );
}

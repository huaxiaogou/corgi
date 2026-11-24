import type { DeviceView } from "../../types/api";

interface PairResult {
  code: string;
  kind: string;
  pair_url: string;
  addr: string;
}

interface Props {
  devices: DeviceView[];
  pairResult: PairResult | null;
  onGenPair: (kind: "browser" | "machine") => Promise<void>;
  onRevoke: (id: string) => Promise<void>;
}

const INSTALL_URL = "https://github.com/huaxiaogou/corgi/releases/latest/download/install.sh";

export function DevicesTab({ devices, pairResult, onGenPair, onRevoke }: Props) {
  const joinCmd =
    pairResult && pairResult.kind === "machine"
      ? `curl -fsSL ${INSTALL_URL} | sh -s -- node --hub ${pairResult.addr} --code ${pairResult.code}`
      : "";

  return (
    <div className="space-y-4">
      <div className="flex gap-2">
        <button type="button" onClick={() => void onGenPair("browser")} className="px-3.5 py-2 rounded-lg btn-accent font-medium cursor-pointer">
          连接手机 / 浏览器
        </button>
        <button type="button" onClick={() => void onGenPair("machine")} className="px-3.5 py-2 rounded-lg border border-strong text-primary cursor-pointer">
          连接一台新电脑 (Node)
        </button>
      </div>
      {pairResult && (
        <div className="p-4 rounded-xl border border-hairline surface-rail space-y-2.5">
          <div className="font-medium">
            一次性配对码（10 分钟内有效）：<span className="font-mono-code ml-1">{pairResult.code}</span>
          </div>
          {pairResult.kind === "machine" ? (
            <div className="space-y-2">
              <div className="text-muted">在目标电脑执行（自动安装 + 配对 + 后台常驻）：</div>
              <pre className="p-2.5 rounded-lg surface-bg border border-hairline font-mono-code overflow-x-auto">{joinCmd}</pre>
              <div className="text-muted">
                已装 corgi 仅前台调试：
                <code className="font-mono-code ml-1">
                  corgi node --hub {pairResult.addr} --code {pairResult.code}
                </code>
              </div>
            </div>
          ) : (
            <img
              src={`/api/qr?text=${encodeURIComponent(pairResult.pair_url)}`}
              alt="配对二维码"
              className="w-40 h-40 rounded-lg border border-hairline"
            />
          )}
        </div>
      )}
      <div className="rounded-xl border border-hairline divide-y divide-[var(--line)]">
        {devices.length === 0 && <div className="p-4 text-muted">暂无已配对设备</div>}
        {devices.map((d) => (
          <div key={d.id} className="px-4 py-3 flex items-center justify-between">
            <div>
              <span className="font-medium">{d.name}</span>
              <span className="ml-2 font-mono-code text-muted">
                {d.kind} · {d.os}
              </span>
            </div>
            <button type="button" onClick={() => void onRevoke(d.id)} className="text-muted hover:underline cursor-pointer">
              移除
            </button>
          </div>
        ))}
      </div>
    </div>
  );
}

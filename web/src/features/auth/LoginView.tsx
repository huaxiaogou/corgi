import { useState, type FormEvent } from "react";
import { CorgiMark } from "../../components/CorgiMark";
import { api } from "../../lib/api";

export function LoginView({ onSuccess }: { onSuccess: () => void }) {
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!token.trim() || busy) return;
    setBusy(true);
    setError("");
    try {
      await api("/api/login", { token: token.trim() });
      onSuccess();
    } catch (err) {
      setError(err instanceof Error ? err.message : "登录失败");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="h-full flex items-center justify-center p-4 surface-bg">
      <form onSubmit={(e) => void submit(e)} className="w-full max-w-[360px] card-floating rounded-2xl p-6 space-y-4">
        <div className="flex items-center gap-2.5">
          <div className="w-10 h-10 rounded-xl border border-hairline flex items-center justify-center">
            <CorgiMark className="w-6 h-6" />
          </div>
          <div>
            <h1 className="text-base font-semibold">Corgi</h1>
            <p className="text-xs text-muted">输入 8 位配对码或主访问令牌</p>
          </div>
        </div>
        <input
          type="password"
          value={token}
          onChange={(e) => setToken(e.target.value)}
          placeholder="配对码 / 访问令牌"
          autoFocus
          required
          className="w-full px-3.5 py-2.5 rounded-xl text-sm surface-rail border border-hairline outline-none"
        />
        {error && (
          <p className="text-xs" style={{ color: "var(--attn)" }}>
            {error}
          </p>
        )}
        <button
          type="submit"
          disabled={busy}
          className="w-full py-2.5 rounded-xl text-sm font-medium btn-accent cursor-pointer disabled:opacity-50"
        >
          {busy ? "验证中…" : "进入"}
        </button>
      </form>
    </div>
  );
}

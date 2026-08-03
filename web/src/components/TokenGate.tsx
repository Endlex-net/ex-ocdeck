import { useState } from 'react';
import { setToken, api, ApiError } from '../api';

/** 首次使用 / token 失效后的 token 输入页。 */
export function TokenGate({ onSaved }: { onSaved: (token: string) => void }) {
  const [value, setValue] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const token = value.trim();
    if (!token || busy) return;
    setBusy(true);
    setError('');
    setToken(token);
    try {
      await api.listProjects(); // 验证 token 有效
      onSaved(token);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '验证失败');
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="token-gate">
      <form className="token-card" onSubmit={submit}>
        <div className="token-logo">ocdeck</div>
        <p className="token-hint">
          输入服务端访问 token（ocdeck-server 启动时生成，全部 API 需 Bearer 认证）。
        </p>
        <input
          className="input"
          type="password"
          placeholder="Bearer token"
          value={value}
          autoFocus
          onChange={(e) => setValue(e.target.value)}
        />
        {error && <div className="error-line">{error}</div>}
        <button className="btn btn-primary" type="submit" disabled={busy || !value.trim()}>
          {busy ? '验证中…' : '连接'}
        </button>
      </form>
    </div>
  );
}

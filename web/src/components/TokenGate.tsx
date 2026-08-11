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
    <div className="od-token-gate">
      <form className="od-card od-token-card" onSubmit={submit}>
        <div className="od-brand-mark od-token-logo">oc</div>
        <h1 className="od-token-title">ocdeck</h1>
        <p className="muted od-token-hint">
          输入服务端访问 token（ocdeck-server 启动时生成，全部 API 需 Bearer 认证）。
        </p>
        <input
          className="od-input"
          type="password"
          placeholder="Bearer token"
          value={value}
          autoFocus
          onChange={(e) => setValue(e.target.value)}
        />
        {error && (
          <div className="od-alert od-alert-danger od-token-error">
            <span className="od-alert-body">{error}</span>
          </div>
        )}
        <button className="od-btn od-btn-primary od-token-submit" type="submit" disabled={busy || !value.trim()}>
          {busy ? '验证中…' : '连接'}
        </button>
      </form>
    </div>
  );
}

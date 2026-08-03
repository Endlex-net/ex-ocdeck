import { useEffect, useState } from 'react';
import { api, ApiError } from '../api';
import type { EnvVar } from '../types';

const RESTART_HINT = '需重启任务（挂起后激活）生效';

/**
 * env 编辑器（design.md 2.7）：key-value 列表 + 添加/编辑/删除。
 * base 为 `/projects/:id/env` 或 `/tasks/:id/env`，两端 DTO 同构。
 */
export function EnvEditor({ base }: { base: string }) {
  const [vars, setVars] = useState<EnvVar[]>([]);
  const [warning, setWarning] = useState('');
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState('');
  const [restartHint, setRestartHint] = useState(false);
  const [newKey, setNewKey] = useState('');
  const [newValue, setNewValue] = useState('');
  const [adding, setAdding] = useState(false);
  const [editKey, setEditKey] = useState<string | null>(null);
  const [editValue, setEditValue] = useState('');
  const [confirmDel, setConfirmDel] = useState<string | null>(null);
  const [busyKey, setBusyKey] = useState<string | null>(null);

  const load = async () => {
    try {
      const res = await api.getEnv(base);
      setVars(res.vars ?? []);
      setWarning(res.warning ?? '');
      setRestartHint(res.restartRequired);
      setError('');
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '加载 env 失败');
    } finally {
      setLoaded(true);
    }
  };

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [base]);

  const applyMutation = (res: { restartRequired: boolean; warning?: string }) => {
    if (res.warning) setWarning(res.warning);
    if (res.restartRequired) setRestartHint(true);
  };

  const add = async (e: React.FormEvent) => {
    e.preventDefault();
    const key = newKey.trim();
    if (adding || !key) return;
    setAdding(true);
    setError('');
    try {
      applyMutation(await api.putEnv(base, key, newValue));
      setNewKey('');
      setNewValue('');
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '保存失败');
    } finally {
      setAdding(false);
    }
  };

  const saveEdit = async (key: string) => {
    if (busyKey) return;
    setBusyKey(key);
    setError('');
    try {
      applyMutation(await api.putEnv(base, key, editValue));
      setEditKey(null);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '保存失败');
    } finally {
      setBusyKey(null);
    }
  };

  const remove = async (key: string) => {
    if (busyKey) return;
    setBusyKey(key);
    setError('');
    try {
      applyMutation(await api.deleteEnv(base, key));
      setConfirmDel(null);
      await load();
    } catch (err) {
      setConfirmDel(null);
      setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '删除失败');
    } finally {
      setBusyKey(null);
    }
  };

  return (
    <div className="env-editor">
      {warning && (
        <div className="warn-box env-warning">
          <p>⚠ {warning}</p>
        </div>
      )}
      {restartHint && <div className="env-hint">ⓘ {RESTART_HINT}</div>}
      {error && <div className="error-line">{error}</div>}

      {loaded && vars.length === 0 && <div className="env-empty">暂无环境变量。</div>}

      {vars.length > 0 && (
        <ul className="env-list">
          {vars.map((v) => (
            <li key={v.key} className="env-row">
              <span className="env-key mono" title={v.key}>
                {v.key}
              </span>
              {editKey === v.key ? (
                <>
                  <input
                    className="input input-grow mono"
                    value={editValue}
                    onChange={(e) => setEditValue(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter') void saveEdit(v.key);
                      if (e.key === 'Escape') setEditKey(null);
                    }}
                    autoFocus
                  />
                  <button
                    className="btn btn-small btn-primary"
                    disabled={busyKey === v.key}
                    onClick={() => void saveEdit(v.key)}
                  >
                    保存
                  </button>
                  <button className="btn btn-small btn-ghost" onClick={() => setEditKey(null)}>
                    取消
                  </button>
                </>
              ) : (
                <>
                  <span className="env-value mono" title={v.value}>
                    {v.value}
                  </span>
                  <button
                    className="btn btn-small btn-ghost"
                    onClick={() => {
                      setEditKey(v.key);
                      setEditValue(v.value);
                      setConfirmDel(null);
                    }}
                  >
                    编辑
                  </button>
                  {confirmDel === v.key ? (
                    <>
                      <button
                        className="btn btn-small btn-danger"
                        disabled={busyKey === v.key}
                        onClick={() => void remove(v.key)}
                      >
                        确认删除
                      </button>
                      <button
                        className="btn btn-small btn-ghost"
                        onClick={() => setConfirmDel(null)}
                      >
                        取消
                      </button>
                    </>
                  ) : (
                    <button
                      className="btn btn-small btn-ghost"
                      onClick={() => {
                        setConfirmDel(v.key);
                        setEditKey(null);
                      }}
                    >
                      删除
                    </button>
                  )}
                </>
              )}
            </li>
          ))}
        </ul>
      )}

      <form className="env-add" onSubmit={add}>
        <input
          className="input mono"
          placeholder="KEY"
          value={newKey}
          onChange={(e) => setNewKey(e.target.value)}
        />
        <input
          className="input input-grow mono"
          placeholder="value"
          value={newValue}
          onChange={(e) => setNewValue(e.target.value)}
        />
        <button className="btn btn-primary" type="submit" disabled={adding || !newKey.trim()}>
          {adding ? '保存中…' : '添加'}
        </button>
      </form>
    </div>
  );
}

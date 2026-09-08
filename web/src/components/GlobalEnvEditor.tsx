import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../api';
import type { GlobalEnvMode, GlobalEnvVar } from '../types';
import { WarnIcon, InfoIcon } from '../icons';

const RESTART_HINT = '需重启任务（挂起后激活）生效';

const MODE_META: Record<GlobalEnvMode, { label: string; cls: string }> = {
  follow_host: { label: '跟随宿主', cls: 'badge-pending' },
  manual: { label: '手动', cls: 'badge-archived' },
};

/** 模式切换：跟随宿主 / 手动配置。 */
function ModeToggle({
  mode,
  onChange,
  disabled,
}: {
  mode: GlobalEnvMode;
  onChange: (m: GlobalEnvMode) => void;
  disabled?: boolean;
}) {
  return (
    <span className="env-mode-toggle">
      <button
        type="button"
        className={`env-mode-opt ${mode === 'follow_host' ? 'env-mode-opt-active' : ''}`}
        disabled={disabled}
        onClick={() => onChange('follow_host')}
      >
        跟随宿主
      </button>
      <button
        type="button"
        className={`env-mode-opt ${mode === 'manual' ? 'env-mode-opt-active' : ''}`}
        disabled={disabled}
        onClick={() => onChange('manual')}
      >
        手动配置
      </button>
    </span>
  );
}

/**
 * 全局环境变量编辑器（design.md 2.9）：跨项目生效，每项可选 follow_host / manual。
 * follow_host 展示服务端当前解析值，宿主未设置时置灰提示。
 * reloadTick / onChanged 为兄弟组件共享的 reload 信号（host-env-sync-and-display D5）：
 * 收到信号时重载以更新 resolvedValue；本组件增删改成功后经 onChanged 广播，
 * 使 SystemEnvPanel 同步「已配置」标注（避免把 manual 配置误当未配置而 upsert 覆盖）。
 */
export function GlobalEnvEditor({
  reloadTick = 0,
  onChanged,
}: {
  reloadTick?: number;
  onChanged?: () => void;
}) {
  const [vars, setVars] = useState<GlobalEnvVar[]>([]);
  const [warning, setWarning] = useState('');
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState('');
  const [restartHint, setRestartHint] = useState(false);
  const [newKey, setNewKey] = useState('');
  const [newMode, setNewMode] = useState<GlobalEnvMode>('follow_host');
  const [newValue, setNewValue] = useState('');
  const [adding, setAdding] = useState(false);
  const [editKey, setEditKey] = useState<string | null>(null);
  const [editMode, setEditMode] = useState<GlobalEnvMode>('manual');
  const [editValue, setEditValue] = useState('');
  const [confirmDel, setConfirmDel] = useState<string | null>(null);
  const [busyKey, setBusyKey] = useState<string | null>(null);
  /** 请求序号：仅当前请求可提交成功/失败/完成状态；迟到的旧响应（含旧失败）直接丢弃，
   *  防止覆盖 tick 广播后新 load 的结果（如 resolvedValue 刷新）。 */
  const loadSeq = useRef(0);

  const load = async () => {
    const seq = ++loadSeq.current;
    try {
      const res = await api.getGlobalEnv();
      if (seq !== loadSeq.current) return;
      setVars(res.vars ?? []);
      setWarning(res.warning ?? '');
      setRestartHint(res.restartRequired);
      setError('');
    } catch (err) {
      if (seq !== loadSeq.current) return;
      setError(err instanceof ApiError ? err.message : '加载全局环境变量失败');
    } finally {
      if (seq === loadSeq.current) setLoaded(true);
    }
  };

  useEffect(() => {
    void load();
  }, [reloadTick]);

  const applyMutation = (res: { restartRequired: boolean; warning?: string }) => {
    if (res.warning) setWarning(res.warning);
    if (res.restartRequired) setRestartHint(true);
  };

  const showError = (err: unknown, fallback: string) =>
    setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : fallback);

  /** 变更成功后的重载：有共享信号则广播（tick 驱动本组件与 SystemEnvPanel 同时重载），否则自载。 */
  const reloadAfterMutation = () => {
    if (onChanged) onChanged();
    else void load();
  };

  const add = async (e: React.FormEvent) => {
    e.preventDefault();
    const key = newKey.trim();
    if (adding || !key) return;
    setAdding(true);
    setError('');
    try {
      applyMutation(await api.putGlobalEnv(key, newMode, newMode === 'manual' ? newValue : ''));
      setNewKey('');
      setNewValue('');
      reloadAfterMutation();
    } catch (err) {
      showError(err, '保存失败');
    } finally {
      setAdding(false);
    }
  };

  const startEdit = (v: GlobalEnvVar) => {
    setEditKey(v.key);
    setEditMode(v.mode);
    setEditValue(v.mode === 'manual' ? v.value : '');
    setConfirmDel(null);
  };

  const saveEdit = async (key: string) => {
    if (busyKey) return;
    setBusyKey(key);
    setError('');
    try {
      applyMutation(
        await api.putGlobalEnv(key, editMode, editMode === 'manual' ? editValue : ''),
      );
      setEditKey(null);
      reloadAfterMutation();
    } catch (err) {
      showError(err, '保存失败');
    } finally {
      setBusyKey(null);
    }
  };

  const remove = async (key: string) => {
    if (busyKey) return;
    setBusyKey(key);
    setError('');
    try {
      applyMutation(await api.deleteGlobalEnv(key));
      setConfirmDel(null);
      reloadAfterMutation();
    } catch (err) {
      setConfirmDel(null);
      showError(err, '删除失败');
    } finally {
      setBusyKey(null);
    }
  };

  return (
    <div className="env-editor">
      {warning && (
        <div className="warn-box env-warning">
          <p><WarnIcon /> {warning}</p>
        </div>
      )}
      {restartHint && <div className="env-hint"><InfoIcon /> {RESTART_HINT}</div>}
      {error && <div className="error-line">{error}</div>}

      {loaded && vars.length === 0 && <div className="env-empty">暂无全局环境变量。</div>}

      {vars.length > 0 && (
        <ul className="env-list">
          {vars.map((v) => (
            <li key={v.key} className="env-row">
              <span className="env-key mono" title={v.key}>
                {v.key}
              </span>
              {editKey === v.key ? (
                <>
                  <ModeToggle mode={editMode} onChange={setEditMode} disabled={busyKey === v.key} />
                  {editMode === 'manual' && (
                    <input
                      className="input input-grow mono"
                      placeholder="value"
                      value={editValue}
                      onChange={(e) => setEditValue(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter') void saveEdit(v.key);
                        if (e.key === 'Escape') setEditKey(null);
                      }}
                      autoFocus
                    />
                  )}
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
                  <span className={`badge ${MODE_META[v.mode].cls}`}>
                    {MODE_META[v.mode].label}
                  </span>
                  {v.mode === 'follow_host' ? (
                    v.resolvedValue ? (
                      <span className="env-value mono" title={`当前解析值：${v.resolvedValue}`}>
                        {v.resolvedValue}
                      </span>
                    ) : (
                      <span className="env-value env-value-unset" title="宿主环境未设置该变量">
                        宿主未设置
                      </span>
                    )
                  ) : (
                    <span className="env-value mono" title={v.value}>
                      {v.value}
                    </span>
                  )}
                  <button className="btn btn-small btn-ghost" onClick={() => startEdit(v)}>
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
        <ModeToggle mode={newMode} onChange={setNewMode} disabled={adding} />
        {newMode === 'manual' && (
          <input
            className="input input-grow mono"
            placeholder="value"
            value={newValue}
            onChange={(e) => setNewValue(e.target.value)}
          />
        )}
        <button className="btn btn-primary" type="submit" disabled={adding || !newKey.trim()}>
          {adding ? '保存中…' : '添加'}
        </button>
      </form>
    </div>
  );
}

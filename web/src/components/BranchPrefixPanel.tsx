import { useEffect, useState } from 'react';
import { api, ApiError } from '../api';

/** 与后端 PUT 校验同源（worktree-branch-prefix spec）：原值匹配，
 *  含首尾空白即非法，不做 trim 容错。 */
const PREFIX_PATTERN = /^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$/;

const PREFIX_HINT = '小写字母或数字开头与结尾，中间可含连字符，1-50 字符（如 ocdeck、team-x）';

/**
 * 分支前缀配置面板（worktree-branch-prefix spec「设置页分支前缀入口」）：
 * 与 PaletteConfigPanel 同构的 GET→ready→编辑→保存流程，加载/保存错误分离。
 * 前缀仅影响之后创建的任务；存量任务分支名不变。
 */
export function BranchPrefixPanel() {
  const [loadState, setLoadState] = useState<'loading' | 'ready' | 'error'>('loading');
  const [loadError, setLoadError] = useState('');
  // 服务端当前生效值（保存成功/加载成功后同步）
  const [current, setCurrent] = useState('');
  const [draft, setDraft] = useState('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');

  const load = () => {
    setLoadState('loading');
    setLoadError('');
    api
      .getBranchPrefix()
      .then((c) => {
        setCurrent(c.prefix);
        setDraft(c.prefix);
        setLoadState('ready');
      })
      .catch((err) => {
        setLoadError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '加载配置失败');
        setLoadState('error');
      });
  };

  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    if (saving || loadState !== 'ready') return;
    setError('');
    setNotice('');
    // 本地校验与后端同正则（原值校验，不 trim）：非法值就地展示，不发 PUT
    if (!PREFIX_PATTERN.test(draft)) {
      setError(`前缀非法：${PREFIX_HINT}`);
      return;
    }
    setSaving(true);
    try {
      const saved = await api.putBranchPrefix(draft);
      setCurrent(saved.prefix);
      setDraft(saved.prefix);
      setNotice('保存成功，已即时生效');
    } catch (err) {
      setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '保存失败');
    } finally {
      setSaving(false);
    }
  };

  const loading = loadState === 'loading';

  return (
    <section className="od-card">
      <div className="od-card-head">
        <h2>工作空间</h2>
        <span className="muted" style={{ fontSize: '12.5px' }}>
          新任务分支名的前缀
        </span>
      </div>

      {loadState === 'error' && (
        <div className="od-alert od-alert-danger" style={{ marginBottom: 14 }}>
          <div className="od-alert-body">
            加载分支前缀配置失败{loadError ? `：${loadError}` : ''}
          </div>
        </div>
      )}
      {error && (
        <div className="od-alert od-alert-danger" style={{ marginBottom: 14 }}>
          <div className="od-alert-body">{error}</div>
        </div>
      )}
      {notice && (
        <div className="od-alert od-alert-info" style={{ marginBottom: 14 }}>
          <div className="od-alert-body">{notice}</div>
        </div>
      )}

      {/* 概念铺垫先行：用户在动字段之前先理解「前缀」指的是什么 */}
      <p style={{ fontSize: '13.5px', margin: '0 0 16px' }}>
        新建 worktree 任务时会从项目切出一个独立分支作为它的工作空间，分支名形如
        「前缀/slug」（例如 <code>ocdeck/my-feature</code>）。这里配置的就是这个前缀，默认为{' '}
        <code>ocdeck</code>。
      </p>

      <div className="od-field">
        <span className="od-label">当前生效值</span>
        <div className="mono" id="branch-prefix-current">
          {loading ? '加载中…' : loadState === 'error' ? '（未加载）' : current}
        </div>
      </div>

      <form onSubmit={(e) => void save(e)}>
        <div className="od-field">
          <label className="od-label" htmlFor="branch-prefix-input">
            分支名前缀
          </label>
          <input
            className="od-input mono"
            id="branch-prefix-input"
            spellCheck={false}
            style={{ maxWidth: 280 }}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            disabled={loadState !== 'ready'}
          />
          {PREFIX_PATTERN.test(draft) && (
            <div className="od-hint mono" data-testid="branch-prefix-preview">
              之后新任务的分支名将是 {draft}/&lt;slug&gt;
            </div>
          )}
          <div className="od-hint">{PREFIX_HINT}</div>
        </div>

        <div className="od-hint">
          仅影响之后创建的任务：已创建任务的分支名不变，修改已有任务的分支名时也沿用该任务原来的前缀。
        </div>

        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 6 }}>
          {loadState === 'error' && (
            <button className="od-btn" type="button" onClick={load}>
              重试加载
            </button>
          )}
          <button className="od-btn od-btn-primary" type="submit" disabled={saving || loading || loadState !== 'ready'}>
            {saving ? '保存中…' : '保存'}
          </button>
        </div>
      </form>
    </section>
  );
}

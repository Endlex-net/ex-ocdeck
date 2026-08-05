import { useEffect, useState } from 'react';
import { api, ApiError } from '../api';
import { GlobalEnvEditor } from '../components/GlobalEnvEditor';
import { TermAppearanceEditor } from '../components/TermAppearanceEditor';
import { navigate } from '../router';
import type { OcConfigContent, OcConfigInfo } from '../types';

/** 全局 opencode 配置编辑（design.md 2.7）：列表 → 文本编辑，mtime/hash 乐观锁，409 冲突对话。 */
export function ConfigsPage() {
  const [configs, setConfigs] = useState<OcConfigInfo[]>([]);
  const [listError, setListError] = useState('');
  const [loaded, setLoaded] = useState(false);

  const [current, setCurrent] = useState<OcConfigContent | null>(null); // 服务端已保存版本
  const [draft, setDraft] = useState(''); // 编辑器内容
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [conflict, setConflict] = useState(false);

  useEffect(() => {
    api
      .listOcConfigs()
      .then((res) => {
        setConfigs(res.configs ?? []);
        setListError('');
      })
      .catch((err) =>
        setListError(err instanceof ApiError ? err.message : '加载配置列表失败'),
      )
      .finally(() => setLoaded(true));
  }, []);

  const open = async (name: string) => {
    setError('');
    setNotice('');
    setConflict(false);
    try {
      const c = await api.getOcConfig(name);
      setCurrent(c);
      setDraft(c.content);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '加载配置失败');
    }
  };

  /** 用给定的基线版本执行 PUT；返回 true 表示成功。 */
  const doSave = async (base: OcConfigContent, content: string): Promise<boolean> => {
    setSaving(true);
    setError('');
    setNotice('');
    try {
      const res = await api.putOcConfig(base.name, content, base.mtime, base.hash);
      setCurrent({ ...base, content, mtime: res.mtime, hash: res.hash });
      setDraft(content);
      setConflict(false);
      const n = res.affectedActiveTasks?.length ?? 0;
      setNotice(
        n > 0
          ? `已保存。${n} 个活跃任务受影响，需重启任务（挂起后激活）生效。`
          : '已保存。当前没有活跃任务使用该配置。',
      );
      return true;
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        setConflict(true);
      } else {
        setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '保存失败');
      }
      return false;
    } finally {
      setSaving(false);
    }
  };

  const save = () => {
    if (current) void doSave(current, draft);
  };

  /** 409 → 覆盖保存：重新 GET 最新 mtime/hash，用编辑器内容重放 PUT。 */
  const overwrite = async () => {
    if (!current) return;
    setConflict(false);
    try {
      const latest = await api.getOcConfig(current.name);
      // doSave 内部会处理再次 409（重新置 conflict）与其他错误
      await doSave(latest, draft);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '重新加载失败');
    }
  };

  /** 409 → 放弃本地编辑，重新加载服务端版本。 */
  const discard = async () => {
    if (!current) return;
    setConflict(false);
    setError('');
    try {
      const c = await api.getOcConfig(current.name);
      setCurrent(c);
      setDraft(c.content);
      setNotice('已重新加载服务端最新内容。');
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '重新加载失败');
    }
  };

  const dirty = current !== null && draft !== current.content;

  return (
    <div className="page page-wide">
      <header className="page-header">
        <button className="btn btn-small btn-ghost" onClick={() => navigate('/')}>
          ← 项目
        </button>
        <span className="page-title">全局配置</span>
        <span className="header-meta">配置文件 / 环境变量 / 终端外观</span>
        <span className="header-spacer" />
        <button className="btn btn-small btn-ghost" onClick={() => navigate('/ai-config')}>
          AI 配置
        </button>
      </header>

      {listError && <div className="error-line">{listError}</div>}

      <section className="env-section global-env-section">
        <div className="global-env-head">
          <span className="global-env-title">终端外观</span>
          <span className="header-meta">浏览器端偏好，对本机所有任务终端生效</span>
        </div>
        <TermAppearanceEditor />
      </section>

      <section className="env-section global-env-section">
        <div className="global-env-head">
          <span className="global-env-title">全局环境变量</span>
          <span className="header-meta">跨项目生效，低于项目级 / 任务级</span>
        </div>
        <GlobalEnvEditor />
      </section>

      <div className="configs-layout">
        <div className="configs-list">
          {loaded && configs.length === 0 && <div className="env-empty">暂无配置文件。</div>}
          <ul className="row-list">
            {configs.map((c) => (
              <li
                key={c.name}
                className={`row configs-item ${current?.name === c.name ? 'configs-item-active' : ''}`}
                onClick={() => void open(c.name)}
              >
                <div className="row-main">
                  <span className="row-name mono">{c.name}</span>
                </div>
              </li>
            ))}
          </ul>
        </div>

        <div className="configs-editor">
          {!current && <div className="empty">选择左侧配置文件开始编辑。</div>}
          {current && (
            <>
              <div className="configs-editor-head">
                <span className="mono">{current.name}</span>
                <span className="header-meta mono">mtime {current.mtime}</span>
                {dirty && <span className="flag flag-notice">未保存</span>}
                <span className="header-spacer" />
                <button
                  className="btn btn-primary btn-small"
                  disabled={saving || !dirty}
                  onClick={save}
                >
                  {saving ? '保存中…' : '保存'}
                </button>
              </div>
              {notice && <div className="env-hint">{notice}</div>}
              {error && <div className="error-line">{error}</div>}
              <textarea
                className="configs-textarea mono"
                spellCheck={false}
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
              />
            </>
          )}
        </div>
      </div>

      {conflict && (
        <div className="modal-backdrop" onClick={() => setConflict(false)}>
          <div className="modal" onClick={(e) => e.stopPropagation()}>
            <div className="modal-title">保存冲突</div>
            <div className="modal-body">
              配置文件 <strong>{current?.name}</strong> 已被外部修改。
              <br />
              覆盖保存将以服务端最新版本为基线写入你的编辑；放弃则丢弃本地编辑并重新加载。
            </div>
            <div className="modal-actions">
              <button className="btn btn-ghost" onClick={() => setConflict(false)}>
                取消
              </button>
              <button className="btn" disabled={saving} onClick={() => void discard()}>
                放弃并重新加载
              </button>
              <button className="btn btn-danger" disabled={saving} onClick={() => void overwrite()}>
                覆盖保存
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

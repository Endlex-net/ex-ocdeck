import { useEffect, useState } from 'react';
import { api, ApiError } from '../api';
import { InfoIcon } from '../icons';

/** tasks.md 5.2 给定文案（勿改）。 */
const INHERIT_NOTE = '仅复制 gitignored/untracked 文件';
const INIT_NOTE = 'Re-run 或异常崩溃后脚本可能重复/并行执行，需幂等';
const PRE_DELETE_NOTE = '删除重试时会重复执行，需幂等';
const LOG_REDLINE = '脚本输出会落盘，勿打印敏感凭据';

/**
 * 项目生命周期配置编辑器（project-lifecycle-config design.md §9）：
 * Inherit patterns / Init script / Pre-delete script 三个多行编辑器 + 保存（PUT 整体替换）。
 * 非法 glob / 超长由服务端 422 + 行号拒绝，原样展示错误。
 */
export function LifecycleConfigEditor({ projectID }: { projectID: string }) {
  const [inheritPatterns, setInheritPatterns] = useState('');
  const [initScript, setInitScript] = useState('');
  const [preDeleteScript, setPreDeleteScript] = useState('');
  // ready 仅在 GET 成功后置位：加载失败时编辑与保存保持禁用，
  // 避免暂时性网络错误后把服务端已有配置整体清空（PUT 为整体替换）
  const [ready, setReady] = useState(false);
  const [saving, setSaving] = useState(false);
  // 加载错误与保存错误分离：保存失败（如非法 glob 422）不得触发重新 GET
  // （重新加载会清空编辑器，丢失待修正内容）；保存失败时用户改完直接再点保存即可
  const [loadError, setLoadError] = useState('');
  const [saveError, setSaveError] = useState('');
  const [saved, setSaved] = useState(false);
  // 重试加载：递增 nonce 重新触发 GET
  const [reloadNonce, setReloadNonce] = useState(0);

  useEffect(() => {
    let cancelled = false;
    // projectID 变化 / 重试时立即重置，防止旧项目配置跨项目提交
    setReady(false);
    setLoadError('');
    setSaved(false);
    setInheritPatterns('');
    setInitScript('');
    setPreDeleteScript('');
    (async () => {
      try {
        const cfg = await api.getLifecycleConfig(projectID);
        if (cancelled) return;
        setInheritPatterns(cfg.inherit_patterns);
        setInitScript(cfg.init_script);
        setPreDeleteScript(cfg.pre_delete_script);
        setLoadError('');
        setReady(true);
      } catch (err) {
        if (!cancelled)
          setLoadError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '加载配置失败');
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [projectID, reloadNonce]);

  const save = async () => {
    if (saving || !ready) return;
    setSaving(true);
    setSaveError('');
    setSaved(false);
    try {
      await api.putLifecycleConfig(projectID, {
        inherit_patterns: inheritPatterns,
        init_script: initScript,
        pre_delete_script: preDeleteScript,
      });
      setSaved(true);
      setSaveError('');
    } catch (err) {
      setSaveError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '保存失败');
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="lc-editor">
      {!ready && loadError && (
        <div className="error-line">
          {loadError}{' '}
          <button className="btn btn-small" onClick={() => setReloadNonce((n) => n + 1)}>
            重试
          </button>
        </div>
      )}
      {saveError && <div className="error-line">{saveError}</div>}
      {saved && !saveError && <div className="env-hint"><InfoIcon /> 已保存</div>}

      <div className="lc-field">
        <div className="lc-label">Inherit patterns</div>
        <div className="lc-note">{INHERIT_NOTE}</div>
        <div className="lc-note lc-note-warn">{LOG_REDLINE}</div>
        <textarea
          className="lc-textarea mono"
          rows={3}
          placeholder="每行一个 glob"
          value={inheritPatterns}
          onChange={(e) => {
            setInheritPatterns(e.target.value);
            setSaved(false);
          }}
          disabled={!ready}
        />
      </div>

      <div className="lc-field">
        <div className="lc-label">Init script</div>
        <div className="lc-note">{INIT_NOTE}</div>
        <div className="lc-note lc-note-warn">{LOG_REDLINE}</div>
        <textarea
          className="lc-textarea mono"
          rows={5}
          value={initScript}
          onChange={(e) => {
            setInitScript(e.target.value);
            setSaved(false);
          }}
          disabled={!ready}
          spellCheck={false}
        />
      </div>

      <div className="lc-field">
        <div className="lc-label">Pre-delete script</div>
        <div className="lc-note">{PRE_DELETE_NOTE}</div>
        <div className="lc-note lc-note-warn">{LOG_REDLINE}</div>
        <textarea
          className="lc-textarea mono"
          rows={5}
          value={preDeleteScript}
          onChange={(e) => {
            setPreDeleteScript(e.target.value);
            setSaved(false);
          }}
          disabled={!ready}
          spellCheck={false}
        />
      </div>

      <div className="lc-actions">
        <button className="btn btn-primary" disabled={saving || !ready} onClick={() => void save()}>
          {saving ? '保存中…' : '保存'}
        </button>
      </div>
    </div>
  );
}

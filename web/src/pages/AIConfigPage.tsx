import { useEffect, useState } from 'react';
import { api, ApiError } from '../api';
import { navigate } from '../router';

/**
 * 全局 AI provider 配置页（ai-worktree-naming design.md D6 / spec「AI 配置页」）：
 * provider 下拉 + api_key 密码框（掩码占位、留空保留原 key）+ base_url + model + 保存。
 * GET 返回 load_error 时显著展示并引导重新保存修复。
 */
export function AIConfigPage() {
  const [loaded, setLoaded] = useState(false);
  const [configured, setConfigured] = useState(false);
  const [loadError, setLoadError] = useState('');

  const [provider, setProvider] = useState('openai');
  const [apiKey, setApiKey] = useState(''); // 仅保存用户新输入，绝不回显明文
  const [keyMasked, setKeyMasked] = useState('');
  const [baseURL, setBaseURL] = useState('');
  const [model, setModel] = useState('');
  const [thinking, setThinking] = useState('');

  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');

  useEffect(() => {
    api
      .getAIConfig()
      .then((res) => {
        setConfigured(res.configured);
        setLoadError(res.load_error ?? '');
        setProvider(res.provider || 'openai');
        setBaseURL(res.base_url);
        setModel(res.model);
        setThinking(res.thinking ?? '');
        setKeyMasked(res.api_key_masked);
      })
      .catch((err) =>
        setError(err instanceof ApiError ? err.message : '加载配置失败'),
      )
      .finally(() => setLoaded(true));
  }, []);

  const save = async () => {
    setError('');
    setNotice('');
    if (model.trim() === '') {
      setError('模型不能为空');
      return;
    }
    if (!configured && apiKey.trim() === '') {
      setError('还没有保存过 API Key，请先填写');
      return;
    }
    setSaving(true);
    try {
      const res = await api.saveAIConfig({
        provider,
        api_key: apiKey, // 留空 = 保留原 key（服务端语义）
        base_url: baseURL.trim(),
        model: model.trim(),
        thinking,
      });
      setConfigured(res.configured);
      setLoadError(res.load_error ?? '');
      setKeyMasked(res.api_key_masked);
      setApiKey('');
      setNotice('保存成功，已即时生效');
    } catch (err) {
      // 422 invalid_input 等：message 即后端返回的原因
      setError(err instanceof ApiError ? err.message : '保存失败');
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="page">
      <header className="page-header">
        <button className="btn btn-small btn-ghost" onClick={() => navigate('/configs')}>
          ← 全局配置
        </button>
        <span className="page-title">AI 配置</span>
        <span className="header-meta">供任务分支命名等 AI 功能使用</span>
      </header>

      {loadError && (
        <div className="error-line">
          配置文件损坏或不可读：{loadError}
          <br />
          在下方重新填写并保存合法配置即可修复。
        </div>
      )}
      {error && <div className="error-line">{error}</div>}
      {notice && <div className="ai-config-saved">{notice}</div>}

      {loaded && !configured && !loadError && (
        <div className="ai-config-hint">ⓘ 尚未配置，AI 功能（如英文分支命名）当前处于降级状态。</div>
      )}

      <form
        className="ai-config-form"
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <div className="ai-config-row">
          <label className="ai-config-label">Provider</label>
          <select
            className="input"
            value={provider}
            onChange={(e) => setProvider(e.target.value)}
          >
            <option value="openai">openai</option>
            <option value="anthropic">anthropic</option>
          </select>
        </div>

        <div className="ai-config-row">
          <label className="ai-config-label">API Key</label>
          <input
            className="input input-grow mono"
            type="password"
            autoComplete="new-password"
            placeholder={
              configured
                ? keyMasked
                  ? `${keyMasked}（留空保持不变）`
                  : '留空保持不变'
                : 'sk-...'
            }
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
          />
        </div>

        <div className="ai-config-row">
          <label className="ai-config-label">Base URL</label>
          <input
            className="input input-grow mono"
            placeholder="留空使用默认端点"
            spellCheck={false}
            value={baseURL}
            onChange={(e) => setBaseURL(e.target.value)}
          />
        </div>

        <div className="ai-config-row">
          <label className="ai-config-label">模型</label>
          <input
            className="input input-grow mono"
            placeholder="如 gpt-4o-mini / claude-haiku-4-5"
            spellCheck={false}
            value={model}
            onChange={(e) => setModel(e.target.value)}
          />
        </div>

        <div className="ai-config-row">
          <label className="ai-config-label">思考强度</label>
          <select
            className="input"
            value={thinking}
            onChange={(e) => setThinking(e.target.value)}
          >
            <option value="">默认</option>
            <option value="off">关闭</option>
            <option value="low">低</option>
            <option value="medium">中</option>
            <option value="high">高</option>
          </select>
        </div>
        <div className="ai-config-row">
          <span className="ai-config-label" />
          <span className="ai-config-hint">
            {thinking === ''
              ? '默认：不下发参数，跟随模型/网关默认。'
              : thinking === 'high'
                ? '思考强度越高响应越慢，「高」强度下分支命名可能超时并回退为自动生成。'
                : '强度越高响应越慢，按需选择。'}
          </span>
        </div>

        <div className="ai-config-actions">
          <button className="btn btn-primary btn-small" type="submit" disabled={saving || !loaded}>
            {saving ? '保存中…' : '保存'}
          </button>
          {configured && <span className="header-meta">当前已配置</span>}
        </div>
      </form>
    </div>
  );
}

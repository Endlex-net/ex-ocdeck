import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../api';
import { GlobalEnvEditor } from '../components/GlobalEnvEditor';
import { NotificationConfigPanel } from '../components/NotificationConfigPanel';
import { PaletteConfigPanel, type PaletteConfigLoadState } from '../components/PaletteConfigPanel';
import { TermAppearanceEditor } from '../components/TermAppearanceEditor';
import type { PaletteConfig } from '../palette-focus';
import { navigate, type ConfigsTab } from '../router';
import { useTheme, type ThemePreference } from '../hooks';
import type { OcConfigContent, OcConfigInfo } from '../types';
import './settings.css';

/* ============================ 设置多合一（tasks.md 7.1-7.5 + task-notifications D12） ============================
 * 终端外观 / 环境变量 / opencode 配置 / AI 配置 / 通知 / 命令面板 合并为单页子标签。
 * 深链恢复：resolveRoute 已将 #/configs#<tab> 的 fragment 归一为合法 ConfigsTab（未知回 appearance）。
 * 子标签切换更新 hash（replace 模式不污染历史）。
 * 终端外观 / 环境变量复用既有编辑器组件；opencode / AI 逻辑已迁入本页（旧 ConfigsPage/AIConfigPage 已删）。 */

const TABS: { key: ConfigsTab; label: string; id: string; panel: string }[] = [
  { key: 'appearance', label: '终端外观', id: 'tab-appearance', panel: 'panel-appearance' },
  { key: 'env', label: '环境变量', id: 'tab-env', panel: 'panel-env' },
  { key: 'opencode', label: 'opencode 配置', id: 'tab-opencode', panel: 'panel-opencode' },
  { key: 'ai', label: 'AI 配置', id: 'tab-ai', panel: 'panel-ai' },
  { key: 'notifications', label: '通知', id: 'tab-notifications', panel: 'panel-notifications' },
  { key: 'palette', label: '命令面板', id: 'tab-palette', panel: 'panel-palette' },
];

const THEME_OPTIONS: { value: ThemePreference; label: string }[] = [
  { value: 'system', label: '跟随系统' },
  { value: 'light', label: '浅色' },
  { value: 'dark', label: '深色' },
];

export function SettingsPage({
  tab,
  paletteConfig,
  paletteLoadState,
  paletteLoadError,
}: {
  tab: ConfigsTab;
  paletteConfig: PaletteConfig;
  paletteLoadState: PaletteConfigLoadState;
  paletteLoadError: string;
}) {
  const tabRefs = useRef<Record<string, HTMLButtonElement | null>>({});

  const selectTab = (key: ConfigsTab) => {
    // replace 模式：不污染返回栈（design.md D3 子标签切换不新增历史项）。
    navigate(`/configs#${key}`, true);
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
    const idx = TABS.findIndex((t) => t.key === tab);
    if (idx < 0) return;
    e.preventDefault();
    const next = TABS[(idx + (e.key === 'ArrowRight' ? 1 : TABS.length - 1)) % TABS.length];
    tabRefs.current[next.key]?.focus();
    selectTab(next.key);
  };

  return (
    <>
      <header className="od-page-head">
        <div className="od-page-title">
          <h1>设置</h1>
          <p className="muted" style={{ fontSize: '13px' }}>
            终端外观 · 全局环境变量 · opencode 配置 · AI provider
          </p>
        </div>
      </header>

      <div className="set-tabs" role="tablist" onKeyDown={onKeyDown}>
        {TABS.map((t) => (
          <button
            key={t.key}
            ref={(el) => {
              tabRefs.current[t.key] = el;
            }}
            className="set-tab"
            role="tab"
            id={t.id}
            aria-controls={t.panel}
            aria-selected={tab === t.key}
            onClick={() => selectTab(t.key)}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'appearance' && (
        <div role="tabpanel" id="panel-appearance" aria-labelledby="tab-appearance">
          <AppearancePanel />
        </div>
      )}
      {tab === 'env' && (
        <div role="tabpanel" id="panel-env" aria-labelledby="tab-env">
          <section className="od-card">
            <div className="od-card-head">
              <h2>全局环境变量</h2>
            </div>
            <GlobalEnvEditor />
          </section>
        </div>
      )}
      {tab === 'opencode' && (
        <div role="tabpanel" id="panel-opencode" aria-labelledby="tab-opencode">
          <OcConfigPanel />
        </div>
      )}
      {tab === 'ai' && (
        <div role="tabpanel" id="panel-ai" aria-labelledby="tab-ai">
          <AIConfigPanel />
        </div>
      )}
      {tab === 'notifications' && (
        <div role="tabpanel" id="panel-notifications" aria-labelledby="tab-notifications">
          <NotificationConfigPanel />
        </div>
      )}
      {tab === 'palette' && (
        <div role="tabpanel" id="panel-palette" aria-labelledby="tab-palette">
          <PaletteConfigPanel
            config={paletteConfig}
            loadState={paletteLoadState}
            loadError={paletteLoadError}
          />
        </div>
      )}
    </>
  );
}

/* ============================ 终端外观子标签 ============================ */
function AppearancePanel() {
  const { preference, setPreference } = useTheme();

  return (
    <section className="od-card">
      <div className="od-card-head">
        <h2>终端外观</h2>
      </div>
      <div className="od-field">
        <span className="od-label" id="themeSegLabel">
          界面主题
        </span>
        <div
          className="seg"
          role="group"
          aria-labelledby="themeSegLabel"
        >
          {THEME_OPTIONS.map((opt) => (
            <button
              key={opt.value}
              type="button"
              className={preference === opt.value ? 'on' : ''}
              aria-pressed={preference === opt.value}
              onClick={() => setPreference(opt.value)}
            >
              {opt.label}
            </button>
          ))}
        </div>
        <div className="od-hint">默认跟随系统；终端配色随主题翻转（亮色主题 = 浅色终端）</div>
      </div>
      <TermAppearanceEditor />
    </section>
  );
}

/* ============================ opencode 配置子标签 ============================
 * 从 ConfigsPage 迁移：文件列表 + 编辑器 + mtime 显示 + 未保存标记 + 乐观锁 409 冲突模态。 */
function OcConfigPanel() {
  const [configs, setConfigs] = useState<OcConfigInfo[]>([]);
  const [listError, setListError] = useState('');
  const [loaded, setLoaded] = useState(false);

  const [current, setCurrent] = useState<OcConfigContent | null>(null);
  const [draft, setDraft] = useState('');
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
    <section className="od-card" style={{ padding: 0, overflow: 'hidden' }}>
      <div className="od-card-head" style={{ padding: '18px 20px 0' }}>
        <h2>opencode 配置文件</h2>
        {/* 保存入口收敛在编辑器头部（文件名/mtime/未保存标记旁），此处不再重复 */}
      </div>

      {listError && (
        <div className="od-alert od-alert-danger" style={{ margin: '12px 16px 0' }}>
          <div className="od-alert-body">{listError}</div>
        </div>
      )}

      <div className="cfg-split">
        <div className="cfg-files" role="listbox" aria-label="配置文件列表">
          {loaded && configs.length === 0 && (
            <div className="od-empty">暂无配置文件。</div>
          )}
          {configs.map((c) => (
            <button
              key={c.name}
              className={`cfg-file ${current?.name === c.name ? 'on' : ''}`}
              role="option"
              aria-selected={current?.name === c.name}
              onClick={() => void open(c.name)}
            >
              {c.name}
            </button>
          ))}
        </div>

        <div className="cfg-editor">
          {!current && <div className="od-empty">选择左侧配置文件开始编辑。</div>}
          {current && (
            <>
              <div className="cfg-editor-head">
                <span className="mono">{current.name}</span>
                <span className="muted mono" style={{ fontSize: '12px' }}>
                  mtime {current.mtime}
                </span>
                {dirty && <span className="cfg-dirty">未保存</span>}
                <span style={{ flex: 1 }} />
                <button
                  className="od-btn od-btn-primary od-btn-sm"
                  disabled={saving || !dirty}
                  onClick={save}
                >
                  {saving ? '保存中…' : '保存'}
                </button>
              </div>
              {notice && <div className="od-hint" style={{ marginBottom: 8 }}>{notice}</div>}
              {error && (
                <div className="od-alert od-alert-danger" style={{ marginBottom: 8 }}>
                  <div className="od-alert-body">{error}</div>
                </div>
              )}
              <textarea
                className="od-textarea mono"
                spellCheck={false}
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                rows={14}
              />
              <p className="od-hint" style={{ marginTop: 8 }}>
                保存（PUT 整体替换），乐观锁冲突时需确认覆盖或重载。改动需重启任务（挂起后激活）生效。
              </p>
            </>
          )}
        </div>
      </div>

      {conflict && (
        <div className="od-modal-mask" onClick={() => setConflict(false)}>
          <div className="od-modal" role="dialog" aria-modal="true" onClick={(e) => e.stopPropagation()}>
            <h2>保存冲突</h2>
            <div className="od-modal-body">
              配置文件 <strong>{current?.name}</strong> 已被外部修改。
              <br />
              覆盖保存将以服务端最新版本为基线写入你的编辑；放弃则丢弃本地编辑并重新加载。
            </div>
            <div className="od-modal-actions">
              <button className="od-btn od-btn-ghost" onClick={() => setConflict(false)}>
                取消
              </button>
              <button className="od-btn" disabled={saving} onClick={() => void discard()}>
                放弃并重新加载
              </button>
              <button
                className="od-btn od-btn-danger"
                disabled={saving}
                onClick={() => void overwrite()}
              >
                覆盖保存
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}

/* ============================ AI 配置子标签 ============================
 * 从 AIConfigPage 迁移：provider 下拉 + 掩码 key + base_url + model + thinking 下拉 + 保存即生效 + load_error 引导。 */
const THINKING_HINT: Record<string, string> = {
  '': '默认：不下发参数，跟随模型/网关默认。',
  off: '关闭思考，响应最快。',
  low: '低强度思考，轻度延迟。',
  medium: '中强度思考，平衡速度与质量。',
  high: '思考强度越高响应越慢，「高」强度下分支命名可能超时并回退为自动生成。',
};

function AIConfigPanel() {
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

  const reload = () => {
    setLoaded(false);
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
        setError('');
      })
      .catch((err) =>
        setError(err instanceof ApiError ? err.message : '加载配置失败'),
      )
      .finally(() => setLoaded(true));
  };

  useEffect(() => {
    reload();
  }, []);

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
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
    <>
      {loaded && !configured && !loadError && (
        <div className="od-alert od-alert-info" style={{ marginBottom: 14 }}>
          <div className="od-alert-body">尚未配置，AI 功能（如英文分支命名）当前处于降级状态。</div>
        </div>
      )}

      <section className="od-card">
        <div className="od-card-head">
          <h2>AI provider</h2>
          <span className="muted" style={{ fontSize: '12.5px' }}>
            保存即时生效
          </span>
        </div>

        {loadError && (
          <div className="od-alert od-alert-danger" style={{ marginBottom: 14 }}>
            <div className="od-alert-body">
              配置文件损坏或不可读：{loadError}
              <br />
              在下方重新填写并保存合法配置即可修复。
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

        <form onSubmit={save}>
          <div className="od-field">
            <label className="od-label" htmlFor="ai-provider">
              provider
            </label>
            <select
              className="od-select"
              id="ai-provider"
              style={{ maxWidth: 280 }}
              value={provider}
              onChange={(e) => setProvider(e.target.value)}
            >
              <option value="openai">openai</option>
              <option value="anthropic">anthropic</option>
            </select>
          </div>

          <div className="od-field">
            <label className="od-label" htmlFor="ai-apiKey">
              API Key
            </label>
            <input
              className="od-input mono"
              id="ai-apiKey"
              type="password"
              autoComplete="new-password"
              style={{ maxWidth: 420 }}
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
            {configured && keyMasked && (
              <div className="od-hint mono">当前已配置 {keyMasked}</div>
            )}
            <div className="od-hint">仅保存用户新输入，绝不回显明文；留空保持不变（服务端语义）</div>
          </div>

          <div className="od-field">
            <label className="od-label" htmlFor="ai-baseUrl">
              base URL
            </label>
            <input
              className="od-input mono"
              id="ai-baseUrl"
              spellCheck={false}
              style={{ maxWidth: 420 }}
              placeholder="留空使用默认端点"
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
            />
          </div>

          <div className="od-field">
            <label className="od-label" htmlFor="ai-model">
              模型
            </label>
            <input
              className="od-input mono"
              id="ai-model"
              spellCheck={false}
              style={{ maxWidth: 420 }}
              placeholder="如 gpt-4o-mini / claude-haiku-4-5"
              value={model}
              onChange={(e) => setModel(e.target.value)}
            />
          </div>

          <div className="od-field">
            <span className="od-label">思考强度</span>
            <select
              className="od-select"
              style={{ maxWidth: 280 }}
              value={thinking}
              onChange={(e) => setThinking(e.target.value)}
            >
              <option value="">默认</option>
              <option value="off">关闭</option>
              <option value="low">低</option>
              <option value="medium">中</option>
              <option value="high">高</option>
            </select>
            <div className="od-hint">{THINKING_HINT[thinking]}</div>
          </div>

          <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: 6 }}>
            <button className="od-btn od-btn-primary" type="submit" disabled={saving || !loaded}>
              {saving ? '保存中…' : '保存'}
            </button>
          </div>
        </form>
      </section>

      {loadError && (
        <details className="od-collapse" style={{ marginTop: 14 }}>
          <summary>
            <svg
              className="od-collapse-caret"
              width="12"
              height="12"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
            >
              <polyline points="9 5 16 12 9 19" />
            </svg>
            配置文件损坏或不可读？
          </summary>
          <div className="od-collapse-body">
            <p style={{ fontSize: '13.5px' }}>
              配置文件损坏或不可读：在上方重新填写并保存合法配置即可修复。
            </p>
            <div style={{ marginTop: 12 }}>
              <button
                className="od-btn od-btn-sm"
                disabled={!loaded}
                onClick={reload}
              >
                重试加载
              </button>
            </div>
          </div>
        </details>
      )}
    </>
  );
}
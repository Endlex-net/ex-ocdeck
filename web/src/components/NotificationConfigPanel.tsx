import { useEffect, useState } from 'react';
import { api, ApiError } from '../api';
import { emitNotificationConfigChanged } from '../notifications';
import type { NotificationCategories, NotificationTestResult } from '../types';

/* ============================ 通知配置子标签（task-notifications D12） ============================
 * 总开关 / 五类别开关 / 空闲阈值 / 四渠道参数（bark endpoint+token、wecom webhook URL、web 权限状态与申请入口）/
 * llm_summary / base_url（附 Bark 提示）/ 保存 / 测试通知 / load_error 展示。
 * token 与 wecom URL 仅保存用户新输入、绝不回显明文（GET 的 token_masked/url_masked 只作提示），
 * 留空保持不变（服务端语义）。 */

/** 五类别：key 与后端 DTO 字段一致。 */
const CATEGORY_ITEMS: { key: keyof NotificationCategories; label: string; desc: string }[] = [
  { key: 'question', label: '等待回答', desc: '任务出现待回答的提问' },
  { key: 'permission', label: '权限确认', desc: '任务等待权限批准' },
  { key: 'idle', label: '空闲超时', desc: '忙后空闲超过阈值' },
  { key: 'retry', label: '重试未恢复', desc: '重试持续 1 分钟未恢复' },
  { key: 'error', label: '运行出错', desc: '错误后 1 分钟未恢复' },
];

type PermissionState = 'granted' | 'denied' | 'default' | 'unsupported';

function notificationPermission(): PermissionState {
  if (typeof Notification === 'undefined') return 'unsupported';
  return Notification.permission;
}

export function NotificationConfigPanel() {
  const [loaded, setLoaded] = useState(false);
  const [loadError, setLoadError] = useState('');
  const [permission, setPermission] = useState<PermissionState>('unsupported');

  const [enabled, setEnabled] = useState(false);
  const [categories, setCategories] = useState<NotificationCategories>({
    question: true,
    permission: true,
    idle: true,
    retry: true,
    error: true,
  });
  const [idleTimeout, setIdleTimeout] = useState(60);
  const [webEnabled, setWebEnabled] = useState(false);
  const [barkEnabled, setBarkEnabled] = useState(false);
  const [barkEndpoint, setBarkEndpoint] = useState('https://api.day.app');
  const [barkToken, setBarkToken] = useState(''); // 仅保存用户新输入，绝不回显明文
  const [tokenMasked, setTokenMasked] = useState('');
  const [macosEnabled, setMacosEnabled] = useState(false);
  const [wecomEnabled, setWecomEnabled] = useState(false);
  const [wecomURL, setWecomURL] = useState(''); // 仅保存用户新输入，绝不回显明文
  const [wecomURLMasked, setWecomURLMasked] = useState('');
  const [llmSummary, setLlmSummary] = useState(false);
  const [baseUrl, setBaseUrl] = useState('');

  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [testResults, setTestResults] = useState<NotificationTestResult[] | null>(null);

  const reload = () => {
    setLoaded(false);
    api
      .getNotificationConfig()
      .then((res) => {
        setLoadError(res.load_error ?? '');
        setEnabled(res.enabled);
        setCategories(res.categories);
        setIdleTimeout(res.idle_timeout_seconds);
        setWebEnabled(res.channels.web.enabled);
        setBarkEnabled(res.channels.bark.enabled);
        setBarkEndpoint(res.channels.bark.endpoint);
        setTokenMasked(res.channels.bark.token_masked ?? '');
        setMacosEnabled(res.channels.macos.enabled);
        setWecomEnabled(res.channels.wecom.enabled);
        setWecomURLMasked(res.channels.wecom.url_masked ?? '');
        setLlmSummary(res.llm_summary);
        setBaseUrl(res.base_url);
        setError('');
      })
      .catch((err) => setError(err instanceof ApiError ? err.message : '加载通知配置失败'))
      .finally(() => setLoaded(true));
  };

  useEffect(() => {
    reload();
    setPermission(notificationPermission());
  }, []);

  const requestPermission = async () => {
    if (typeof Notification === 'undefined') return;
    const result = await Notification.requestPermission();
    setPermission(result);
    if (result === 'granted') emitNotificationConfigChanged();
  };

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    setNotice('');
    setTestResults(null);
    const timeout = Number(idleTimeout);
    if (!Number.isInteger(timeout) || timeout < 10 || timeout > 3600) {
      setError('空闲阈值需为 10–3600 的整数（秒）');
      return;
    }
    setSaving(true);
    try {
      const res = await api.saveNotificationConfig({
        enabled,
        categories,
        idle_timeout_seconds: timeout,
        channels: {
          web: { enabled: webEnabled },
          bark: { enabled: barkEnabled, endpoint: barkEndpoint.trim(), token: barkToken },
          macos: { enabled: macosEnabled },
          wecom: { enabled: wecomEnabled, url: wecomURL },
        },
        llm_summary: llmSummary,
        base_url: baseUrl.trim(),
      });
      setLoadError(res.load_error ?? '');
      setTokenMasked(res.channels.bark.token_masked ?? '');
      setBarkToken('');
      setWecomURLMasked(res.channels.wecom.url_masked ?? '');
      setWecomURL('');
      setNotice('保存成功，已即时生效');
      emitNotificationConfigChanged();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '保存失败');
    } finally {
      setSaving(false);
    }
  };

  const sendTest = async () => {
    setError('');
    setNotice('');
    setTestResults(null);
    setTesting(true);
    try {
      const res = await api.testNotification();
      setTestResults(res.results ?? []);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '测试通知失败');
    } finally {
      setTesting(false);
    }
  };

  return (
    <section className="od-card">
      <div className="od-card-head">
        <h2>任务通知</h2>
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
          <label className="od-label" htmlFor="ntf-master">
            <input
              type="checkbox"
              id="ntf-master"
              checked={enabled}
              onChange={(e) => setEnabled(e.target.checked)}
            />{' '}
            启用通知（总开关）
          </label>
          <div className="od-hint">总开关关闭时不触发任何通知；测试通知需要先开启</div>
        </div>

        <div className="od-field">
          <span className="od-label">通知类别</span>
          {CATEGORY_ITEMS.map((c) => (
            <label key={c.key} style={{ display: 'block', margin: '4px 0' }}>
              <input
                type="checkbox"
                checked={categories[c.key]}
                onChange={(e) => setCategories({ ...categories, [c.key]: e.target.checked })}
              />{' '}
              {c.label}
              <span className="muted" style={{ fontSize: '12px', marginLeft: 6 }}>
                {c.desc}
              </span>
            </label>
          ))}
        </div>

        <div className="od-field">
          <label className="od-label" htmlFor="ntf-idle">
            空闲阈值（秒）
          </label>
          <input
            className="od-input mono"
            id="ntf-idle"
            type="number"
            min={10}
            max={3600}
            style={{ maxWidth: 160 }}
            value={idleTimeout}
            onChange={(e) => setIdleTimeout(Number(e.target.value))}
          />
          <div className="od-hint">任务忙转闲后持续空闲超过该时长触发 idle 通知（10–3600）</div>
        </div>

        <div className="od-field">
          <span className="od-label">通知渠道</span>

          <label style={{ display: 'block', margin: '4px 0' }}>
            <input type="checkbox" checked={webEnabled} onChange={(e) => setWebEnabled(e.target.checked)} />{' '}
            网页通知（web）
          </label>
          {webEnabled && (
            <div style={{ margin: '6px 0 10px 22px' }}>
              {permission === 'granted' ? (
                <div className="od-hint">浏览器通知权限：已授权</div>
              ) : permission === 'unsupported' ? (
                <div className="od-hint">当前浏览器不支持系统通知</div>
              ) : (
                <div>
                  <span className="od-hint">
                    浏览器通知权限：{permission === 'denied' ? '已拒绝（需在浏览器设置中重新允许）' : '未授权'}
                  </span>
                  {permission !== 'denied' && (
                    <button type="button" className="od-btn od-btn-sm" style={{ marginLeft: 10 }} onClick={() => void requestPermission()}>
                      申请通知权限
                    </button>
                  )}
                </div>
              )}
            </div>
          )}

          <label style={{ display: 'block', margin: '4px 0' }}>
            <input type="checkbox" checked={barkEnabled} onChange={(e) => setBarkEnabled(e.target.checked)} />{' '}
            Bark 推送（iOS）
          </label>
          {barkEnabled && (
            <div style={{ margin: '6px 0 10px 22px' }}>
              <div style={{ marginBottom: 6 }}>
                <input
                  className="od-input mono"
                  spellCheck={false}
                  style={{ maxWidth: 420 }}
                  placeholder="https://api.day.app"
                  value={barkEndpoint}
                  onChange={(e) => setBarkEndpoint(e.target.value)}
                />
              </div>
              <div>
                <input
                  className="od-input mono"
                  type="password"
                  autoComplete="new-password"
                  style={{ maxWidth: 420 }}
                  placeholder={tokenMasked ? `${tokenMasked}（留空保持不变）` : 'device key'}
                  value={barkToken}
                  onChange={(e) => setBarkToken(e.target.value)}
                />
              </div>
              <div className="od-hint">token 仅保存新输入，绝不回显明文；留空保持不变（服务端语义）</div>
            </div>
          )}

          <label style={{ display: 'block', margin: '4px 0' }}>
            <input type="checkbox" checked={macosEnabled} onChange={(e) => setMacosEnabled(e.target.checked)} />{' '}
            macOS 本地通知
          </label>
          <div className="od-hint">
            macOS 渠道需服务端运行在 Darwin 且安装 terminal-notifier 或 osascript；启用即投递
          </div>

          <label style={{ display: 'block', margin: '4px 0' }}>
            <input type="checkbox" checked={wecomEnabled} onChange={(e) => setWecomEnabled(e.target.checked)} />{' '}
            企业微信群机器人（wecom）
          </label>
          {wecomEnabled && (
            <div style={{ margin: '6px 0 10px 22px' }}>
              <input
                className="od-input mono"
                type="password"
                autoComplete="new-password"
                spellCheck={false}
                style={{ maxWidth: 420 }}
                placeholder={wecomURLMasked ? `${wecomURLMasked}（留空保持不变）` : '粘贴完整 webhook URL'}
                value={wecomURL}
                onChange={(e) => setWecomURL(e.target.value)}
              />
              <div className="od-hint">webhook URL 仅保存新输入，绝不回显明文；留空保持不变（服务端语义）</div>
            </div>
          )}
        </div>

        <div className="od-field">
          <label className="od-label" htmlFor="ntf-baseurl">
            base URL
          </label>
          <input
            className="od-input mono"
            id="ntf-baseurl"
            spellCheck={false}
            style={{ maxWidth: 420 }}
            placeholder="留空使用服务端监听地址"
            value={baseUrl}
            onChange={(e) => setBaseUrl(e.target.value)}
          />
          <div className="od-hint">Bark 在手机上打开链接需要可达地址（默认 loopback 地址手机不可达）</div>
        </div>

        <div className="od-field">
          <label>
            <input type="checkbox" checked={llmSummary} onChange={(e) => setLlmSummary(e.target.checked)} />{' '}
            LLM 停止原因总结
          </label>
          <div className="od-hint">需要配置 AI provider；失败或超时（5 秒上界）自动降级为确定性摘要</div>
        </div>

        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 10, marginTop: 6 }}>
          <button
            type="button"
            className="od-btn"
            disabled={testing || saving || !loaded || !enabled}
            onClick={() => void sendTest()}
          >
            {testing ? '发送中…' : '发送测试通知'}
          </button>
          <button className="od-btn od-btn-primary" type="submit" disabled={saving || !loaded}>
            {saving ? '保存中…' : '保存'}
          </button>
        </div>
      </form>

      {testResults && (
        <div style={{ marginTop: 14 }}>
          {testResults.length === 0 && <div className="od-hint">没有已启用且已配置的渠道</div>}
          {testResults.map((r) => (
            <div key={r.name} className="od-hint" style={{ display: 'flex', gap: 8 }}>
              <span className="mono">{r.name}</span>
              <span
                style={{
                  color:
                    r.status === 'success' ? 'var(--ok, green)' : r.status === 'failed' ? 'var(--danger, red)' : undefined,
                }}
              >
                {r.status === 'success' ? '成功' : r.status === 'failed' ? `失败${r.error ? `：${r.error}` : ''}` : '未启用或未配置'}
              </span>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}
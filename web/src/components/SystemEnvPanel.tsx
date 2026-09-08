import { useEffect, useMemo, useRef, useState } from 'react';
import { api, ApiError } from '../api';
import type { HostEnvSource, HostEnvVar } from '../types';
import { InfoIcon } from '../icons';

/* ============================ 系统环境变量展示区（host-env-sync-and-display D5） ============================
 * 展示服务端读到的宿主环境变量合并视图（进程环境 ∪ login shell 捕获），帮用户确认
 * follow_host 能解析到什么。值默认定长掩码（不泄露长度），点击行值区域切换明文（行级状态）。
 * 「添加」一键创建 follow_host 模式全局变量；禁用原因优先级：已配置 → 系统保留 → 非法 key。
 * 刷新成功后经 onGlobalChanged 通知兄弟组件 GlobalEnvEditor 重载（resolvedValue 可能变化）。 */

const MASK = '••••••••';
const REFRESH_HINT = '刷新更新宿主解析结果；任务需挂起后激活生效。';

/** 与后端 envKeyPattern / envKeyReserved（internal/api/env.go）保持一致的前端预判。 */
const KEY_PATTERN = /^[A-Za-z_][A-Za-z0-9_]*$/;
const RESERVED_PREFIX = 'OCDECK_';
const RESERVED_EXACT = new Set(['OPENCODE_SERVER_PASSWORD']);

const SOURCE_META: Record<HostEnvSource, { label: string; cls: string }> = {
  process: { label: '进程', cls: 'badge-pending' },
  shell: { label: 'Shell', cls: 'badge-archived' },
  both: { label: '两者', cls: 'badge-active' },
};

/** 已配置 key 集加载状态：仅 ready 且集合属于当前共享信号版本时才放行添加——否则可能
 *  把已有 manual 配置 upsert 覆盖成 follow_host（后端校验只查合法性，不拒绝已有 key）。 */
type CfgState = 'unknown' | 'ready' | 'failed';

interface AddState {
  label: string;
  disabled: boolean;
  reason: string;
}

/** 按钮状态优先级：已知行级原因（已配置 → 系统保留 → 非法 key）先于读取状态门控；
 *  仅无法确定资格的普通 key 显示读取状态（K5）。eligible = 配置集加载成功且属于当前 tick。 */
function addStateOf(
  key: string,
  configured: Set<string>,
  eligible: boolean,
  cfgState: CfgState,
): AddState {
  if (configured.has(key)) {
    return { label: '已配置', disabled: true, reason: '已配置为全局环境变量' };
  }
  if (key.startsWith(RESERVED_PREFIX) || RESERVED_EXACT.has(key)) {
    return { label: '系统保留', disabled: true, reason: '系统保留变量，不可添加' };
  }
  if (!KEY_PATTERN.test(key)) {
    return { label: '非法 key', disabled: true, reason: '不符合命名规则 ^[A-Za-z_][A-Za-z0-9_]*$' };
  }
  if (!eligible) {
    return {
      label: '添加',
      disabled: true,
      reason: cfgState === 'failed' ? '读取全局配置失败，请重试后再添加' : '正在读取全局配置…',
    };
  }
  return { label: '添加', disabled: false, reason: '' };
}

export function SystemEnvPanel({
  reloadTick = 0,
  onGlobalChanged,
}: {
  /** 兄弟组件共享 reload 信号：全局列表变化（含本组件添加/刷新触发）时递增，同步已配置 key 集。 */
  reloadTick?: number;
  /** 添加/刷新成功后回调：SettingsPage 递增共享信号，驱动 GlobalEnvEditor 重载。 */
  onGlobalChanged: () => void;
}) {
  const [vars, setVars] = useState<HostEnvVar[]>([]);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState('');
  const [search, setSearch] = useState('');
  const [revealed, setRevealed] = useState<Set<string>>(new Set());
  const [configured, setConfigured] = useState<Set<string>>(new Set());
  const [cfgState, setCfgState] = useState<CfgState>('unknown');
  /** 配置集合对应的共享信号版本：仅当 eligibleTick === 当前 reloadTick 且加载成功才放行添加。
   *  tick 一变即在渲染期失效（不等 effect 启动），关闭重载在途基于旧集合放行的窗口。 */
  const [eligibleTick, setEligibleTick] = useState(-1);
  /** 请求序号：丢弃迟到的旧 getGlobalEnv 响应，防止覆盖新状态。 */
  const cfgSeq = useRef(0);
  const [addingKey, setAddingKey] = useState<string | null>(null);

  /** 已配置 key 集：随共享 reload 信号更新（全局列表增删后「已配置」标注保持准确）。
   *  重试/重载期间保留旧集合用于展示，但 eligibleTick 不匹配即失去添加资格；
   *  失败进入 failed（普通 key 禁用 + 可重试）。 */
  const loadConfigured = async (tick: number) => {
    const seq = ++cfgSeq.current;
    setCfgState((s) => (s === 'ready' ? s : 'unknown'));
    try {
      const res = await api.getGlobalEnv();
      if (seq !== cfgSeq.current) return;
      setConfigured(new Set((res.vars ?? []).map((v) => v.key)));
      setCfgState('ready');
      setEligibleTick(tick);
    } catch {
      if (seq !== cfgSeq.current) return;
      setCfgState('failed');
    }
  };

  useEffect(() => {
    void loadConfigured(reloadTick);
  }, [reloadTick]);

  /** 添加资格：配置集加载成功且属于当前 tick（重载在途期间旧集合只供展示，不可用于判定）。 */
  const eligible = cfgState === 'ready' && eligibleTick === reloadTick;

  /** 首次加载：可能触发后端 login shell 捕获（D4），需 loading 态。 */
  useEffect(() => {
    (async () => {
      setLoading(true);
      try {
        const res = await api.getHostEnv();
        setVars(res.vars ?? []);
        setError('');
      } catch (err) {
        setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '加载系统环境变量失败');
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  const filtered = useMemo(() => {
    const q = search.trim();
    if (!q) return vars;
    return vars.filter((v) => v.key.includes(q));
  }, [vars, search]);

  const toggleReveal = (key: string) => {
    setRevealed((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  const refresh = async () => {
    if (refreshing) return;
    setRefreshing(true);
    setError('');
    try {
      const res = await api.refreshHostEnv();
      // 仅成功时替换列表并联动全局列表重载；失败保留原列表。
      setVars(res.vars ?? []);
      onGlobalChanged();
    } catch (err) {
      setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '刷新失败，已保留原列表');
    } finally {
      setRefreshing(false);
    }
  };

  const add = async (key: string) => {
    if (addingKey || !eligible) return;
    setAddingKey(key);
    setError('');
    try {
      await api.putGlobalEnv(key, 'follow_host', '');
      // 立即本地标记已配置：共享信号 reload 完成前按钮即变「已配置」，防止重复点击
      setConfigured((prev) => new Set(prev).add(key));
      onGlobalChanged();
    } catch (err) {
      setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '添加失败');
    } finally {
      setAddingKey(null);
    }
  };

  return (
    <div className="env-editor">
      <div className="env-toolbar">
        <input
          className="input input-grow mono"
          placeholder="按 key 搜索"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <button
          className="btn btn-small"
          disabled={refreshing || loading}
          onClick={() => void refresh()}
        >
          {refreshing ? '刷新中…' : '刷新'}
        </button>
      </div>

      <div className="env-hint">
        <InfoIcon /> {REFRESH_HINT}
      </div>
      {error && <div className="error-line">{error}</div>}
      {cfgState === 'failed' && (
        <div className="error-line">
          读取全局环境变量配置失败，添加功能已暂时禁用。
          <button
            className="btn btn-small btn-ghost"
            style={{ marginLeft: 8 }}
            onClick={() => void loadConfigured(reloadTick)}
          >
            重试
          </button>
        </div>
      )}

      {loading ? (
        <div className="env-empty">
          <span className="spinner spinner-inline" aria-hidden /> 正在读取宿主环境变量（首次可能需要启动 login shell）…
        </div>
      ) : vars.length === 0 && !error ? (
        <div className="env-empty">未读取到宿主环境变量。</div>
      ) : filtered.length === 0 ? (
        <div className="env-empty">无匹配「{search.trim()}」的系统环境变量。</div>
      ) : (
        <ul className="env-list">
          {filtered.map((v) => {
            const shown = revealed.has(v.key);
            const st = addStateOf(v.key, configured, eligible, cfgState);
            return (
              <li key={v.key} className="env-row">
                <span className="env-key mono" title={v.key}>
                  {v.key}
                </span>
                <span className={`badge ${SOURCE_META[v.source].cls}`}>
                  {SOURCE_META[v.source].label}
                </span>
                <button
                  type="button"
                  className={`env-value env-value-btn mono${shown ? '' : ' env-value-masked'}`}
                  title={shown ? '点击隐藏' : '点击显示明文'}
                  onClick={() => toggleReveal(v.key)}
                >
                  {shown ? v.value || '（空值）' : MASK}
                </button>
                <button
                  className="btn btn-small btn-ghost"
                  disabled={st.disabled || addingKey === v.key}
                  title={st.reason || `添加为全局环境变量（跟随宿主）`}
                  onClick={() => void add(v.key)}
                >
                  {addingKey === v.key ? '添加中…' : st.label}
                </button>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}

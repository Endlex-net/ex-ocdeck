import { useRef, useState } from 'react';
import { api, ApiError } from '../api';
import { navigate } from '../router';
import { usePoll } from '../hooks';
import type { ActiveSessionItem } from '../types';
import { AgentStatusBadge } from '../components/AgentStatusBadge';

/** 相对时间（last_active_at 为 Unix 秒）。 */
function relativeTime(ts: number): string {
  const diff = Math.max(0, Math.floor(Date.now() / 1000) - ts);
  if (diff < 60) return '刚刚';
  const min = Math.floor(diff / 60);
  if (min < 60) return `${min} 分钟前`;
  const hour = Math.floor(min / 60);
  if (hour < 24) return `${hour} 小时前`;
  const day = Math.floor(hour / 24);
  return `${day} 天前`;
}

export function ActiveSessionsPage() {
  const [items, setItems] = useState<ActiveSessionItem[] | null>(null);
  const [error, setError] = useState('');
  // single-flight：上一请求未返回时跳过本次 tick；finally 释放，
  // 一次失败不会永久停止轮询（design.md D5）。
  const inflight = useRef(false);

  const load = async () => {
    if (inflight.current) return;
    inflight.current = true;
    try {
      setItems(await api.listActiveSessions());
      setError('');
    } catch (err) {
      // 失败保留上次成功数据，仅提示错误，不闪现空态
      setError(err instanceof ApiError ? err.message : '加载失败');
    } finally {
      inflight.current = false;
    }
  };
  usePoll(() => void load(), 5000);

  return (
    <div className="page">
      <header className="page-header">
        <button className="btn btn-small btn-ghost" onClick={() => navigate('/')}>
          ← 项目
        </button>
        <span className="page-title">活跃会话</span>
        <span className="header-spacer" />
      </header>

      {error && <div className="error-line">{error}</div>}

      {items === null && !error && <div className="empty">加载中…</div>}

      {items !== null && items.length === 0 && (
        <div className="empty">
          当前没有活跃会话。到
          <button className="btn btn-small btn-ghost" onClick={() => navigate('/')}>
            项目列表
          </button>
          激活一个任务后，这里会显示所有项目正在运行的任务。
        </div>
      )}

      <ul className="row-list">
        {(items ?? []).map((it) => (
          <li
            key={it.task_id}
            className="row"
            onClick={() => navigate(`/task/${it.task_id}?from=active`)}
          >
            <div className="row-main">
              <span className="row-name">{it.name}</span>
              <span className="row-sub mono">
                {it.project_name} · {it.branch || it.worktree_path}
              </span>
            </div>
            <span className="row-meta mono" title={new Date(it.last_active_at * 1000).toLocaleString()}>
              {relativeTime(it.last_active_at)}
            </span>
            <AgentStatusBadge agentStatus={it.agentStatus} />
          </li>
        ))}
      </ul>
    </div>
  );
}

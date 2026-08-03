import { useState } from 'react';
import { api, ApiError } from '../api';
import { navigate } from '../router';
import { usePoll } from '../hooks';
import { isTransitional, parseNotice, type ProjectDetail, type Task } from '../types';
import { StatusBadge } from '../components/StatusBadge';
import { AgentStatusBadge } from '../components/AgentStatusBadge';
import { TaskActions } from '../components/TaskActions';
import { DeleteTaskModal } from '../components/DeleteTaskModal';
import { EnvEditor } from '../components/EnvEditor';

export function ProjectDetailPage({ projectID }: { projectID: string }) {
  const [project, setProject] = useState<ProjectDetail | null>(null);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [error, setError] = useState('');
  const [loaded, setLoaded] = useState(false);
  const [newName, setNewName] = useState('');
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<Task | null>(null);
  const [envOpen, setEnvOpen] = useState(false);

  const load = async () => {
    try {
      const [p, ts] = await Promise.all([
        api.getProject(projectID),
        api.listTasks(projectID),
      ]);
      setProject(p);
      setTasks(ts);
      setError('');
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '加载失败');
    } finally {
      setLoaded(true);
    }
  };
  // 有过渡态任务时加快轮询以及时刷新 spinner 状态
  const hasTransitional = tasks.some((t) => isTransitional(t.status));
  usePoll(() => void load(), hasTransitional ? 2000 : 5000, [hasTransitional]);

  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    if (creating || !newName.trim()) return;
    setCreating(true);
    setError('');
    try {
      const t = await api.createTask(projectID, newName.trim());
      setNewName('');
      await load();
      navigate(`/task/${t.id}`);
    } catch (err) {
      setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '创建失败');
    } finally {
      setCreating(false);
    }
  };

  const counts = project?.tasks_by_status ?? {};

  return (
    <div className="page">
      <header className="page-header">
        <button className="btn btn-small btn-ghost" onClick={() => navigate('/')}>
          ← 项目
        </button>
        <span className="page-title">{project?.name ?? '…'}</span>
        {project && (
          <span className="header-meta mono">
            {project.path} · {project.default_branch}
          </span>
        )}
        <span className="header-spacer" />
        {(['active', 'suspended', 'archived'] as const).map(
          (s) =>
            counts[s] ? (
              <span key={s} className="header-meta mono">
                {s}:{counts[s]}
              </span>
            ) : null,
        )}
      </header>

      {error && <div className="error-line">{error}</div>}

      <form className="create-bar" onSubmit={create}>
        <input
          className="input input-grow"
          placeholder="新任务名称（回车创建并进入工作台）"
          value={newName}
          onChange={(e) => setNewName(e.target.value)}
        />
        <button className="btn btn-primary" type="submit" disabled={creating}>
          {creating ? '创建中…' : '新建任务'}
        </button>
      </form>

      <div className="env-section">
        <button
          className="btn btn-small btn-ghost env-toggle"
          onClick={() => setEnvOpen((v) => !v)}
        >
          {envOpen ? '▾ 项目环境变量' : '▸ 项目环境变量'}
        </button>
        {envOpen && <EnvEditor base={`/projects/${projectID}/env`} />}
      </div>

      {loaded && tasks.length === 0 && (
        <div className="empty">暂无任务。创建一个任务以获得独立 worktree + opencode 会话。</div>
      )}

      <ul className="row-list">
        {tasks.map((t) => {
          const notices = parseNotice(t.notice);
          return (
            <li key={t.id} className="row" onClick={() => navigate(`/task/${t.id}`)}>
              <div className="row-main">
                <span className="row-name">
                  {t.name}
                  {t.last_error && (
                    <span className="flag flag-error" title={t.last_error}>
                      ⚠ error
                    </span>
                  )}
                  {notices.length > 0 && (
                    <span className="flag flag-notice" title={notices[notices.length - 1].message}>
                      ⓘ {notices.length}
                    </span>
                  )}
                </span>
                <span className="row-sub mono">{t.branch || t.worktree_path}</span>
              </div>
              <StatusBadge status={t.status} />
              <AgentStatusBadge agentStatus={t.agentStatus} />
              <TaskActions
                task={t}
                onDone={() => void load()}
                onError={setError}
              />
              <button
                className="btn btn-small btn-ghost"
                disabled={isTransitional(t.status)}
                onClick={(e) => {
                  e.stopPropagation();
                  setDeleting(t);
                }}
              >
                删除
              </button>
            </li>
          );
        })}
      </ul>

      {deleting && (
        <DeleteTaskModal
          task={deleting}
          onClose={() => setDeleting(null)}
          onDeleted={() => {
            setDeleting(null);
            void load();
          }}
        />
      )}
    </div>
  );
}

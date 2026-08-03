import { useState } from 'react';
import { api, ApiError } from '../api';
import { navigate } from '../router';
import { usePoll } from '../hooks';
import type { Project } from '../types';

export function ProjectsPage() {
  const [projects, setProjects] = useState<Project[]>([]);
  const [error, setError] = useState('');
  const [loaded, setLoaded] = useState(false);
  const [name, setName] = useState('');
  const [path, setPath] = useState('');
  const [creating, setCreating] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);

  const load = async () => {
    try {
      setProjects(await api.listProjects());
      setError('');
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '加载失败');
    } finally {
      setLoaded(true);
    }
  };
  usePoll(() => void load(), 5000);

  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    if (creating || !name.trim() || !path.trim()) return;
    setCreating(true);
    setError('');
    try {
      await api.createProject(name.trim(), path.trim());
      setName('');
      setPath('');
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '创建失败');
    } finally {
      setCreating(false);
    }
  };

  const remove = async (id: string) => {
    try {
      await api.deleteProject(id);
      setConfirmDelete(null);
      await load();
    } catch (err) {
      setConfirmDelete(null);
      setError(
        err instanceof ApiError
          ? err.status === 409
            ? '项目下仍有任务，无法删除'
            : `[${err.code}] ${err.message}`
          : '删除失败',
      );
    }
  };

  return (
    <div className="page">
      <header className="page-header">
        <span className="brand">ocdeck</span>
        <span className="page-title">项目</span>
        <span className="header-spacer" />
        <button className="btn btn-small btn-ghost" onClick={() => navigate('/configs')}>
          全局配置
        </button>
      </header>

      {error && <div className="error-line">{error}</div>}

      <form className="create-bar" onSubmit={create}>
        <input
          className="input"
          placeholder="项目名称"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <input
          className="input input-grow"
          placeholder="仓库路径（git repo 绝对路径）"
          value={path}
          onChange={(e) => setPath(e.target.value)}
        />
        <button className="btn btn-primary" type="submit" disabled={creating}>
          {creating ? '注册中…' : '注册项目'}
        </button>
      </form>

      {loaded && projects.length === 0 && (
        <div className="empty">还没有项目。注册一个 git 仓库开始并行编排任务。</div>
      )}

      <ul className="row-list">
        {projects.map((p) => (
          <li
            key={p.id}
            className="row"
            onClick={() => navigate(`/project/${p.id}`)}
          >
            <div className="row-main">
              <span className="row-name">{p.name}</span>
              <span className="row-sub mono">{p.path}</span>
            </div>
            {/* 任务概况：与项目详情页头部一致的展示模式（字段在后端 lane 落地前可能缺省） */}
            {(p.task_count ?? 0) > 0 && (
              <span className="row-meta mono">tasks:{p.task_count}</span>
            )}
            {(['active', 'suspended', 'archived'] as const).map(
              (s) =>
                (p.tasks_by_status ?? {})[s] ? (
                  <span key={s} className="row-meta mono">
                    {s}:{p.tasks_by_status[s]}
                  </span>
                ) : null,
            )}
            <span className="row-meta mono">{p.default_branch}</span>
            {confirmDelete === p.id ? (
              <span className="task-actions" onClick={(e) => e.stopPropagation()}>
                <button className="btn btn-small btn-danger" onClick={() => void remove(p.id)}>
                  确认删除
                </button>
                <button className="btn btn-small" onClick={() => setConfirmDelete(null)}>
                  取消
                </button>
              </span>
            ) : (
              <button
                className="btn btn-small btn-ghost"
                onClick={(e) => {
                  e.stopPropagation();
                  setConfirmDelete(p.id);
                }}
              >
                删除
              </button>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}

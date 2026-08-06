import { useState } from 'react';
import { api, ApiError } from '../api';
import { navigate } from '../router';
import { usePoll } from '../hooks';
import type { Project, ProjectKind } from '../types';

export function ProjectsPage() {
  const [projects, setProjects] = useState<Project[]>([]);
  const [error, setError] = useState('');
  const [loaded, setLoaded] = useState(false);
  const [name, setName] = useState('');
  const [path, setPath] = useState('');
  // 项目类型：默认 git 仓库；dir 仅校验路径存在、无 git 功能（D1/D7）
  const [kind, setKind] = useState<ProjectKind>('repo');
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
      await api.createProject(name.trim(), path.trim(), kind);
      setName('');
      setPath('');
      setKind('repo');
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
        <button className="btn btn-small btn-ghost" onClick={() => navigate('/active')}>
          活跃会话
        </button>
        <button className="btn btn-small btn-ghost" onClick={() => navigate('/configs')}>
          全局配置
        </button>
      </header>

      {error && <div className="error-line">{error}</div>}

      <form className="create-bar" onSubmit={create}>
        <select
          className="input"
          value={kind}
          onChange={(e) => setKind(e.target.value as ProjectKind)}
          title="项目类型"
        >
          <option value="repo">git 仓库</option>
          <option value="dir">纯目录</option>
        </select>
        <input
          className="input"
          placeholder="项目名称"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <input
          className="input input-grow"
          placeholder={
            kind === 'repo'
              ? '本机项目目录的绝对路径（如 /Users/you/workplace/my-repo）'
              : '目录的绝对路径（任意普通目录）'
          }
          value={path}
          onChange={(e) => setPath(e.target.value)}
        />
        <button className="btn btn-primary" type="submit" disabled={creating}>
          {creating ? '注册中…' : '注册项目'}
        </button>
      </form>

      <div className="form-hint">
        {kind === 'repo'
          ? '填写已克隆到本机的项目目录路径，目录本身须已是 git 仓库；不支持直接填写远端仓库地址（http/ssh），如需使用请先自行 git clone 到本机。'
          : '填写任意普通目录路径，不要求 git 仓库；该目录下的多个任务将共享同一目录。'}
      </div>

      {loaded && projects.length === 0 && (
        <div className="empty">还没有项目。注册一个 git 仓库或纯目录开始编排任务。</div>
      )}

      <ul className="row-list">
        {projects.map((p) => (
          <li
            key={p.id}
            className="row"
            onClick={() => navigate(`/project/${p.id}`)}
          >
            <div className="row-main">
              <span className="row-name">
                {p.name}
                <span className="flag flag-kind">{p.kind === 'dir' ? '目录' : 'git'}</span>
              </span>
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
            {p.kind !== 'dir' && <span className="row-meta mono">{p.default_branch}</span>}
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

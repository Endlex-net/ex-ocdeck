import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../api';
import { navigate } from '../router';
import { usePoll } from '../hooks';
import { isTransitional, parseNotice, type ProjectDetail, type Task } from '../types';
import { StatusBadge } from '../components/StatusBadge';
import { AgentStatusBadge } from '../components/AgentStatusBadge';
import { TaskActions } from '../components/TaskActions';
import { DeleteTaskModal } from '../components/DeleteTaskModal';
import { EnvEditor } from '../components/EnvEditor';
import { LifecycleConfigEditor } from '../components/LifecycleConfigEditor';
import { InitStatusBadge } from '../components/InitStatusBadge';
import { LifecycleLogModal } from '../components/LifecycleLogModal';
import { BranchCombobox } from '../components/BranchCombobox';

export function ProjectDetailPage({ projectID }: { projectID: string }) {
  const [project, setProject] = useState<ProjectDetail | null>(null);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [error, setError] = useState('');
  const [loaded, setLoaded] = useState(false);
  const [newName, setNewName] = useState('');
  const [creating, setCreating] = useState(false);
  // repo 任务基线分支（D10）：候选来自 GET /projects/:id/branches，默认选中项目默认分支；
  // dir 项目不请求、不展示该控件。branchesTick 递增触发重新拉取（失败重试入口）
  const [branches, setBranches] = useState<string[]>([]);
  const [branchesError, setBranchesError] = useState('');
  const [branchesTick, setBranchesTick] = useState(0);
  const [baseRef, setBaseRef] = useState('');
  // 远端分支刷新（D10 7.6）：首次打开选择器自动一次 + 显式刷新按钮；
  // refreshing 期间禁止提交创建；失败保留本地快照并显式标注
  const [refreshing, setRefreshing] = useState(false);
  const [refreshError, setRefreshError] = useState('');
  const autoRefreshDoneRef = useRef(false);
  const refreshInFlightRef = useRef(false);
  // 初始 GET 与 refresh 共享的写入代际：任一写入 branches 的路径都必须确认
  // 自己仍是最新一代；refresh 发起时递增，使此前未完成的 GET 响应失效
  const branchesGenRef = useRef(0);
  const unmountedRef = useRef(false);
  useEffect(
    () => () => {
      unmountedRef.current = true;
    },
    [],
  );
  const [deleting, setDeleting] = useState<Task | null>(null);
  const [envOpen, setEnvOpen] = useState(false);
  const [configOpen, setConfigOpen] = useState(false);
  // init 失败任务的日志查看入口（tasks.md 5.3）
  const [initLogTask, setInitLogTask] = useState<Task | null>(null);

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
  // 有过渡态任务时加快轮询以及时刷新 spinner 状态；
  // init_status pending|running 同样视为活跃（tasks.md 5.3：轮询条件不只看 task.status）
  const hasTransitional = tasks.some(
    (t) =>
      isTransitional(t.status) || t.init_status === 'pending' || t.init_status === 'running',
  );
  usePoll(() => void load(), hasTransitional ? 2000 : 5000, [hasTransitional]);

  // 分支列表只取一次（不随任务轮询刷新）；dir 项目不调用（后端对 dir 返回错误）。
  // 页面已按 projectID key 重挂载，此处默认分支可无条件覆盖；
  // cancelled 竞态防护：effect 失效（切换项目/卸载）后旧请求结果不得写入状态。
  useEffect(() => {
    if (!project || project.kind === 'dir') return;
    setBaseRef(project.default_branch);
    setBranchesError('');
    let cancelled = false;
    // 捕获当前代际：若期间发生 refresh（代际递增），本响应属于旧快照，不得回写
    const gen = branchesGenRef.current;
    api
      .listBranches(projectID)
      .then((bs) => {
        if (!cancelled && gen === branchesGenRef.current) setBranches(bs);
      })
      .catch((err) => {
        if (!cancelled && gen === branchesGenRef.current)
          setBranchesError(
            err instanceof ApiError ? `[${err.code}] ${err.message}` : '获取分支列表失败',
          );
      });
    return () => {
      cancelled = true;
    };
  }, [projectID, project?.kind, project?.default_branch, branchesTick]); // eslint-disable-line react-hooks/exhaustive-deps

  // 保证当前选中值总在选项内（默认分支可能不在远端/本地列表中）：
  // 派生新数组，绝不原地修改 branches state
  const branchOptions = (() => {
    const base = branches.length > 0 ? branches : project ? [project.default_branch] : [];
    return baseRef && !base.includes(baseRef) ? [baseRef, ...base] : base;
  })();

  // refresh 竞态防护：共享代际 + 卸载标志，项目切换/卸载后旧响应不得写入状态；
  // 发起时递增代际，使此前未完成的初始 GET 响应失效（不得覆盖 refresh 结果）；
  // inFlight 去重（连点/自动+手动并发只跑一个）
  const refreshBranches = async () => {
    if (!project || project.kind === 'dir') return;
    if (refreshInFlightRef.current) return;
    refreshInFlightRef.current = true;
    const gen = ++branchesGenRef.current;
    setRefreshing(true);
    setRefreshError('');
    try {
      const bs = await api.refreshBranches(projectID);
      if (unmountedRef.current || gen !== branchesGenRef.current) return;
      setBranches(bs);
      setBranchesError('');
    } catch (err) {
      if (unmountedRef.current || gen !== branchesGenRef.current) return;
      setRefreshError(
        err instanceof ApiError ? `[${err.code}] ${err.message}` : '刷新远端分支失败',
      );
    } finally {
      refreshInFlightRef.current = false;
      if (!unmountedRef.current && gen === branchesGenRef.current) setRefreshing(false);
    }
  };

  // 首次打开/聚焦基线选择器时自动 refresh 一次（每个页面挂载周期内仅一次）
  const handlePickerOpen = () => {
    if (autoRefreshDoneRef.current) return;
    autoRefreshDoneRef.current = true;
    void refreshBranches();
  };

  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    // refreshing 期间禁止提交：防止基于陈旧快照误提交远端基线（D10 7.6）
    if (creating || refreshing || !newName.trim()) return;
    setCreating(true);
    setError('');
    try {
      const isDir = project?.kind === 'dir';
      const t = await api.createTask(
        projectID,
        newName.trim(),
        isDir ? undefined : baseRef || undefined,
      );
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
          <span className="flag flag-kind">{project.kind === 'dir' ? '目录' : 'git'}</span>
        )}
        {project && (
          <span className="header-meta mono">
            {project.path}
            {project.kind !== 'dir' && ` · ${project.default_branch}`}
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
        {project && project.kind !== 'dir' && (
          <>
            <BranchCombobox
              options={branchOptions}
              value={baseRef}
              defaultBranch={project.default_branch}
              onChange={setBaseRef}
              onOpen={handlePickerOpen}
              loading={refreshing}
              title="基线分支（新 worktree 从此分支创建，默认为项目默认分支）"
            />
            <button
              type="button"
              className="btn"
              disabled={refreshing}
              title="从远端拉取最新分支列表（git fetch）"
              onClick={() => void refreshBranches()}
            >
              {refreshing ? (
                <>
                  <span className="spinner" aria-hidden /> 刷新中…
                </>
              ) : (
                '⟳ 刷新远端分支'
              )}
            </button>
          </>
        )}
        <button className="btn btn-primary" type="submit" disabled={creating || refreshing}>
          {creating ? '创建中…' : '新建任务'}
        </button>
      </form>

      {/* 分支列表拉取失败：显式报错 + 重试，不静默退化为仅默认分支选项 */}
      {branchesError && (
        <div className="alert-bar alert-error">
          <span className="mono">获取分支列表失败：{branchesError}</span>
          <button className="btn btn-small" onClick={() => setBranchesTick((t) => t + 1)}>
            重试
          </button>
        </div>
      )}

      {/* refresh 失败：保留本地快照并显式标注（D10，不静默冒充最新） */}
      {refreshError && (
        <div className="alert-bar alert-error">
          <span className="mono">
            远端分支刷新失败，当前为本地快照（未刷新）：{refreshError}
          </span>
          <button className="btn btn-small" onClick={() => void refreshBranches()}>
            重试
          </button>
        </div>
      )}

      {/* dir 项目多活跃任务无文件隔离提示（D7，纯前端判断：活跃 = status active） */}
      {project?.kind === 'dir' && tasks.filter((t) => t.status === 'active').length >= 2 && (
        <div className="alert-bar alert-notice">
          <span>多个活跃任务共享同一目录、无文件隔离，并行改动可能相互影响。</span>
        </div>
      )}

      <div className="env-section">
        <button
          className="btn btn-small btn-ghost env-toggle"
          onClick={() => setEnvOpen((v) => !v)}
        >
          {envOpen ? '▾ 项目环境变量' : '▸ 项目环境变量'}
        </button>
        {envOpen && <EnvEditor base={`/projects/${projectID}/env`} />}
      </div>

      <div className="env-section">
        <button
          className="btn btn-small btn-ghost env-toggle"
          onClick={() => setConfigOpen((v) => !v)}
        >
          {configOpen ? '▾ Project Config' : '▸ Project Config'}
        </button>
        {configOpen && <LifecycleConfigEditor projectID={projectID} />}
      </div>

      {loaded && tasks.length === 0 && (
        <div className="empty">
          {project?.kind === 'dir'
            ? '暂无任务。创建一个任务以在该目录中启动 opencode 会话。'
            : '暂无任务。创建一个任务以获得独立 worktree + opencode 会话。'}
        </div>
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
                {/* dir 任务无分支概念，直接展示项目目录路径（依据 project_kind，非判空推断） */}
                <span className="row-sub mono">
                  {t.project_kind === 'dir' ? t.worktree_path : t.branch || t.worktree_path}
                </span>
              </div>
              <StatusBadge status={t.status} />
              <InitStatusBadge task={t} />
              {t.init_status === 'failed' && (
                <button
                  className="btn btn-small btn-ghost"
                  title={t.init_error || 'init 失败'}
                  onClick={(e) => {
                    e.stopPropagation();
                    setInitLogTask(t);
                  }}
                >
                  日志
                </button>
              )}
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

      {initLogTask && (
        <LifecycleLogModal
          title={`init 日志 · ${initLogTask.name}`}
          fetchLog={() => api.getInitLog(initLogTask.id)}
          onClose={() => setInitLogTask(null)}
        />
      )}

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

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api, ApiError } from '../api';
import { navigate, useHashRoute } from '../router';
import {
  useProjects,
  useProjectsRefresh,
  runProjectMutation,
  createErrorMessage,
  deleteErrorMessage,
} from '../hooks';
import {
  isTransitional,
  parseNotice,
  type Project,
  type ProjectDetail,
  type ProjectKind,
  type Task,
} from '../types';
import { StatusBadge } from '../components/StatusBadge';
import { AgentStatusBadge } from '../components/AgentStatusBadge';
import { TaskActions } from '../components/TaskActions';
import { DeleteTaskModal } from '../components/DeleteTaskModal';
import { EnvEditor } from '../components/EnvEditor';
import { LifecycleConfigEditor } from '../components/LifecycleConfigEditor';
import { InitStatusBadge } from '../components/InitStatusBadge';
import { LifecycleLogModal } from '../components/LifecycleLogModal';
import {
  PALETTE_FOCUS_EVENT,
  clearPendingPaletteFocus,
  consumePendingPaletteFocus,
  readPaletteFocusDetail,
} from '../palette-focus';
import {
  BranchIcon,
  FolderIcon,
  InfoIcon,
  WarnIcon,
} from '../icons';
import './projects-manage.css';

/** 子标签。 */
type DetailTab = 'overview' | 'automation' | 'env';

const TAB_LABEL: Record<DetailTab, string> = {
  overview: '概览',
  automation: '自动化',
  env: '环境变量',
};

/** 从 hash 路由（resolveRoute 的 fragment）解析当前选中项目 id。
 *  fragment 来自 resolveRoute（#/projects#<id> 深链），非法 id 不选中（展示空详情占位）。 */
function useSelectedProjectID(): string | undefined {
  const route = useHashRoute();
  // route 形如 "/projects#<id>"；分离 fragment（与 resolveRoute 同构，但本页只需 fragment）。
  const idx = route.indexOf('#');
  if (idx === -1) return undefined;
  const frag = route.slice(idx + 1);
  return frag || undefined;
}

/** 健康摘要：活跃/挂起/归档/失败计数（从 project.tasks_by_status 推导）。纯函数。 */
export function healthCounts(tasksByStatus: Record<string, number> | undefined): {
  active: number;
  suspended: number;
  archived: number;
  failed: number;
} {
  const s = tasksByStatus ?? {};
  return {
    active: s.active ?? 0,
    suspended: s.suspended ?? 0,
    archived: s.archived ?? 0,
    failed: (s.creation_failed ?? 0) + (s.deletion_failed ?? 0),
  };
}

/** 健康摘要可过滤状态（点击 chip 过滤任务列表，用户已确认方案）。 */
export const STATUS_FILTER_LABEL: Record<string, string> = {
  active: '活跃',
  suspended: '挂起',
  archived: '归档',
};

/** 任务列表按状态过滤（纯函数）：null/空串显示全量。 */
export function filterTasksByStatus(tasks: Task[], status: string | null): Task[] {
  if (!status) return tasks;
  return tasks.filter((t) => t.status === status);
}

/** chip 单选切换（纯函数）：点当前选中项取消（恢复全量），点其他项切换选中。 */
export function toggleStatusFilter(current: string | null, next: string): string | null {
  return current === next ? null : next;
}

/** 轨道搜索：按名称/路径/类型过滤项目列表。纯函数。 */
export function filterProjects(projects: Project[], query: string): Project[] {
  const q = query.trim().toLowerCase();
  if (!q) return projects;
  return projects.filter((p) => {
    const hay = `${p.name} ${p.path} ${p.kind === 'dir' ? '纯目录 dir' : '仓库 repo git'}`.toLowerCase();
    return hay.includes(q);
  });
}

/** 轨道项目图标：repo 用分支图标，dir 用文件夹图标（与设计稿 rail 一致）。 */
function ProjectRailIcon({ kind }: { kind: ProjectKind }) {
  return kind === 'dir' ? <FolderIcon /> : <BranchIcon />;
}

/** 详情面板头部：名称 / 类型徽章 / 路径 / 删除项目。 */
function DetailHeader({
  project,
  onDelete,
}: {
  project: ProjectDetail;
  onDelete: () => void;
}) {
  const isDir = project.kind === 'dir';
  const hasTasks = project.task_count > 0;
  return (
    <header className="pjm-detail-head">
      <div>
        <h2 className="pjm-detail-title">
          {project.name}
          <span className="od-badge od-badge-type">
            <ProjectRailIcon kind={project.kind} />
            {isDir ? '纯目录' : '仓库'}
          </span>
        </h2>
        <p className="pjm-detail-sub mono">
          {project.path}
          {!isDir && ` · 默认分支 ⎇ ${project.default_branch}`}
        </p>
      </div>
      <div className="pjm-detail-actions">
        <button
          className="od-btn od-btn-sm od-btn-danger"
          disabled={hasTasks}
          title={hasTasks ? '项目下仍有任务，清空任务后才能删除' : undefined}
          onClick={onDelete}
        >
          删除项目
        </button>
      </div>
      {hasTasks && (
        <p className="pjm-detail-blockhint">
          仍有 {project.task_count} 个任务，清空任务后才能删除项目
        </p>
      )}
    </header>
  );
}

/** 健康摘要条：活跃/挂起/归档计数为可点击 chip（单选过滤任务列表，再点取消）；
 *  失败计数保持纯展示（失败态不属于三档过滤）。 */
function HealthSummary({
  project,
  selected,
  onSelect,
}: {
  project: ProjectDetail;
  selected: string | null;
  onSelect: (status: string | null) => void;
}) {
  const counts = healthCounts(project.tasks_by_status);
  // 有活跃任务待人工（待答问题/待授权限）时活跃 chip 用蓝点，区别于正常运行绿点。
  const activeNeedsAttention = (project.tasks ?? []).some((t) => t.status === 'active' && (t.attention_count ?? 0) > 0);
  const chips: Array<{ status: string; count: number; dot: 'attention' | 'busy' | 'retry' | null }> = [
    { status: 'active', count: counts.active, dot: activeNeedsAttention ? 'attention' : 'busy' },
    { status: 'suspended', count: counts.suspended, dot: null },
    { status: 'archived', count: counts.archived, dot: null },
  ];
  return (
    <div className="pjm-health">
      {chips
        .filter((c) => c.count > 0)
        .map((c) => (
          <button
            key={c.status}
            type="button"
            className={`pjm-chip pjm-chip-${c.status}`}
            aria-pressed={selected === c.status}
            title={`只看${STATUS_FILTER_LABEL[c.status]}任务`}
            onClick={() => onSelect(toggleStatusFilter(selected, c.status))}
          >
            {c.dot && (
              <span className={`od-agent od-agent-${c.dot}`} aria-hidden>
                <span className="od-agent-dot" />
              </span>
            )}
            {STATUS_FILTER_LABEL[c.status]} {c.count}
          </button>
        ))}
      {counts.failed > 0 && (
        <span className="pjm-health-item pjm-health-fail">
          失败 {counts.failed}
        </span>
      )}
      {counts.active === 0 && counts.suspended === 0 && counts.archived === 0 && counts.failed === 0 && (
        <span className="pjm-health-item">暂无活跃或挂起任务</span>
      )}
    </div>
  );
}

/** 概览子标签：健康摘要 + 任务行（完整状态机）+ 前往指挥中心新建任务链接。
 *  MUST NOT 提供创建表单（design.md D12：新建任务入口收敛至指挥中心）。 */
function OverviewPane({
  project,
  tasks,
  statusFilter,
  onFilterChange,
  onTaskRowDone,
  onTaskRowError,
  onDeleteTask,
  onInitLog,
}: {
  project: ProjectDetail;
  tasks: Task[];
  /** 健康摘要 chip 选中的状态过滤（null = 全量）。 */
  statusFilter: string | null;
  onFilterChange: (status: string | null) => void;
  onTaskRowDone: () => void;
  onTaskRowError: (msg: string) => void;
  onDeleteTask: (task: Task) => void;
  onInitLog: (task: Task) => void;
}) {
  const isDir = project.kind === 'dir';
  const visibleTasks = filterTasksByStatus(tasks, statusFilter);
  // listTasks 行 DTO 不透出 attention；任务摘要（project.tasks）带 attention_count，
  // 同一次详情加载（getProject + listTasks 并行）取得，按 id 对齐供状态徽标「等待人工」判定。
  const attentionByTask = new Map((project.tasks ?? []).map((s) => [s.id, s.attention_count ?? 0]));
  return (
    <div className="pjm-pane" data-pane="overview">
      {isDir && tasks.filter((t) => t.status === 'active').length >= 2 && (
        <div className="od-alert od-alert-warn" style={{ marginBottom: 14 }}>
          <WarnIcon />
          <div className="od-alert-body">
            纯目录项目：无分支与 worktree 隔离，多个活跃任务共享同一目录，并行改动可能相互影响。
          </div>
        </div>
      )}
      <HealthSummary project={project} selected={statusFilter} onSelect={onFilterChange} />
      {statusFilter && (
        <div className="pjm-filter-bar" aria-live="polite">
          当前过滤：{STATUS_FILTER_LABEL[statusFilter]} · {visibleTasks.length} 个任务
          <button
            type="button"
            className="od-btn od-btn-sm od-btn-ghost"
            onClick={() => onFilterChange(null)}
          >
            清除
          </button>
        </div>
      )}
      {tasks.length === 0 ? (
        <div className="od-empty">
          还没有任务。到<a className="pjm-cc-link" href="#/">指挥中心</a>
          新建任务，或先在「自动化」里配置 init 脚本，让新任务开箱即用。
        </div>
      ) : visibleTasks.length === 0 ? (
        <div className="od-empty">无{statusFilter ? STATUS_FILTER_LABEL[statusFilter] : ''}任务</div>
      ) : (
        <>
          <div className="od-rows">
            {visibleTasks.map((t) => {
              const notices = parseNotice(t.notice);
              return (
                <div
                  key={t.id}
                  className="od-row od-row-clickable"
                  onClick={() => navigate(`/task/${t.id}?from=projects`)}
                >
                  <div className="od-row-main">
                    <div className="od-row-title">
                      {t.last_error && (
                        <span
                          className="od-badge od-badge-fail"
                          title={t.last_error}
                        >
                          <WarnIcon /> error
                        </span>
                      )}
                      {t.name}
                    </div>
                    <div className="od-row-sub">
                      {/* dir 任务无分支概念，直接展示项目目录路径（依据 project_kind，非判空推断） */}
                      <span className="mono">
                        {t.project_kind === 'dir'
                          ? t.worktree_path
                          : t.branch || t.worktree_path}
                      </span>
                      {notices.length > 0 && (
                        <span
                          className="od-badge od-badge-warn"
                          title={notices[notices.length - 1].message}
                        >
                          <InfoIcon /> {notices.length}
                        </span>
                      )}
                    </div>
                  </div>
                  <div className="pjm-task-row-side">
                    <StatusBadge status={t.status} />
                    <InitStatusBadge task={t} />
                    {t.init_status === 'failed' && (
                      <button
                        className="od-btn od-btn-sm od-btn-ghost"
                        title={t.init_error || 'init 失败'}
                        onClick={(e) => {
                          e.stopPropagation();
                          onInitLog(t);
                        }}
                      >
                        日志
                      </button>
                    )}
                    <AgentStatusBadge agentStatus={t.agentStatus} attentionCount={attentionByTask.get(t.id)} />
                    <TaskActions
                      task={t}
                      onDone={onTaskRowDone}
                      onError={onTaskRowError}
                    />
                    {/* 活跃态不出现删除按钮（design D9 / tasks 8.4）；失败/挂起/归档态提供删除入口 */}
                    {t.status !== 'active' && (
                      <button
                        className="od-btn od-btn-sm od-btn-ghost"
                        disabled={isTransitional(t.status)}
                        onClick={(e) => {
                          e.stopPropagation();
                          onDeleteTask(t);
                        }}
                      >
                        删除
                      </button>
                    )}
                  </div>
                </div>
              );
            })}
          </div>
          <p className="od-hint" style={{ marginTop: 10 }}>
            新建任务已移至
            <a
              className="pjm-cc-link"
              href="#/"
              style={{ marginLeft: 4 }}
            >
              指挥中心
            </a>
            ，此处聚焦任务状态与项目配置。
          </p>
        </>
      )}
    </div>
  );
}

/** 自动化子标签：init 脚本 + 文件继承 + pre-delete（迁移 LifecycleConfigEditor）。 */
function AutomationPane({ project }: { project: ProjectDetail }) {
  return (
    <div className="pjm-pane" data-pane="automation">
      <p className="od-hint" style={{ marginBottom: 16 }}>
        这里定义「新任务如何自动准备好」：创建后跑什么脚本、从主仓库继承哪些文件、删除前如何清理。仅对之后创建的任务生效。
      </p>
      <LifecycleConfigEditor projectID={project.id} />
    </div>
  );
}

/** 环境变量子标签（迁移 EnvEditor）。 */
function EnvPane({ project }: { project: ProjectDetail }) {
  return (
    <div className="pjm-pane" data-pane="env">
      <EnvEditor base={`/projects/${project.id}/env`} />
    </div>
  );
}

/** 详情面板：头部 + 子标签 + 内容区。
 *  项目详情数据（getProject + listTasks）走现有 api 方法（非共享 store 轮询）。
 *  错误态分层：
 *  - error: 详情加载失败（loadDetail 设置），阻断详情渲染并提示。
 *  - actionError: 任务行操作失败（TaskActions.onError 设置），不阻断详情渲染，
 *    在内容区上方行内展示，由下次操作开始时清空（不在 loadDetail 中清，避免被刷新覆盖）。
 *  注：EnvEditor / LifecycleConfigEditor 为冻结共享组件，未暴露 onSaved 回调；
 *  env/lifecycle 保存不影响 /projects 列表数据（独立存储），故不接线共享 store refresh()
 *  （tasks.md 6.6 列举的 mutation = 注册/删除项目 + 任务行操作；全量审计见 8.8）。 */
function DetailPanel({
  project,
  tasks,
  loading,
  error,
  actionError,
  tab,
  onTabChange,
  onBack,
  onDeleteProject,
  onTaskRowDone,
  onTaskRowError,
  onDeleteTask,
  onInitLog,
}: {
  project: ProjectDetail | null;
  tasks: Task[];
  loading: boolean;
  error: string;
  actionError: string;
  tab: DetailTab;
  onTabChange: (t: DetailTab) => void;
  onBack: () => void;
  onDeleteProject: () => void;
  onTaskRowDone: () => void;
  onTaskRowError: (msg: string) => void;
  onDeleteTask: (task: Task) => void;
  onInitLog: (task: Task) => void;
}) {
  // 健康摘要 chip 状态过滤（null = 全量）。Hook 必须在 early return 之前；
  // 详情面板按 projectID key 重挂载，切换项目时过滤自动复位。
  const [statusFilter, setStatusFilter] = useState<string | null>(null);

  if (loading && !project) {
    return <div className="od-empty">加载中…</div>;
  }
  if (error) {
    return <div className="error-line">{error}</div>;
  }
  if (!project) {
    return (
      <div className="od-empty">
        选择左侧项目查看详情，或<a className="pjm-cc-link" href="#/">前往指挥中心</a>。
      </div>
    );
  }

  return (
    <section className="pjm-detail">
      {/* 返回列表入口（≤1024px 钻取模式可见，CSS 控制显隐） */}
      <button
        type="button"
        className="pjm-back"
        onClick={onBack}
        aria-label="返回项目列表"
      >
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <polyline points="15 18 9 12 15 6" />
        </svg>
        全部项目
      </button>

      <DetailHeader project={project} onDelete={onDeleteProject} />

      <div className="pjm-tabs" role="tablist">
        {(Object.keys(TAB_LABEL) as DetailTab[]).map((t) => (
          <button
            key={t}
            role="tab"
            className="pjm-tab"
            aria-selected={tab === t}
            onClick={() => onTabChange(t)}
          >
            {TAB_LABEL[t]}
          </button>
        ))}
      </div>

      {actionError && <div className="error-line" style={{ marginBottom: 12 }}>{actionError}</div>}

      {tab === 'overview' && (
        <OverviewPane
          project={project}
          tasks={tasks}
          statusFilter={statusFilter}
          onFilterChange={setStatusFilter}
          onTaskRowDone={onTaskRowDone}
          onTaskRowError={onTaskRowError}
          onDeleteTask={onDeleteTask}
          onInitLog={onInitLog}
        />
      )}
      {tab === 'automation' && <AutomationPane project={project} />}
      {tab === 'env' && <EnvPane project={project} />}
    </section>
  );
}

/** 注册项目表单：repo/dir 类型选择 + 上下文提示（迁移自 ProjectsPage 注册逻辑）。 */
function RegisterForm({
  open,
  onClose,
  onRegistered,
  focusNameNonce = 0,
}: {
  open: boolean;
  onClose: () => void;
  onRegistered: () => void;
  /** 命令面板触发展开后递增，用于聚焦项目名称输入。 */
  focusNameNonce?: number;
}) {
  const refresh = useProjectsRefresh();
  const [name, setName] = useState('');
  const [path, setPath] = useState('');
  const [kind, setKind] = useState<ProjectKind>('repo');
  const [creating, setCreating] = useState(false);
  const [mutError, setMutError] = useState('');
  const nameRef = useRef<HTMLInputElement>(null);

  // 表单打开或 palette-focus 触发后聚焦名称输入
  useEffect(() => {
    if (!open) return;
    requestAnimationFrame(() => nameRef.current?.focus());
  }, [open, focusNameNonce]);

  const reset = () => {
    setName('');
    setPath('');
    setKind('repo');
    setMutError('');
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (creating || !name.trim() || !path.trim()) return;
    setCreating(true);
    setMutError('');
    await runProjectMutation({
      mutate: () => api.createProject(name.trim(), path.trim(), kind),
      refresh,
      onSuccess: () => {
        reset();
        onRegistered();
        onClose();
      },
      onError: (err) => setMutError(createErrorMessage(err)),
    });
    setCreating(false);
  };

  if (!open) return null;

  return (
    <section className="od-card" data-od-id="project-create">
      <div className="od-card-head">
        <h2>注册项目</h2>
      </div>
      <form onSubmit={submit}>
        <div className="od-field">
          <span className="od-label">项目类型</span>
          <div className="od-radio-cards" role="radiogroup">
            <div
              className="od-radio-card"
              role="radio"
              aria-checked={kind === 'repo'}
              tabIndex={0}
              onClick={() => setKind('repo')}
              onKeyDown={(e) => {
                if (e.key === 'Enter' || e.key === ' ') {
                  e.preventDefault();
                  setKind('repo');
                }
              }}
            >
              <div className="od-radio-title">git 仓库</div>
              <div className="od-radio-desc">
                任务获得独立分支与 worktree 隔离，含完整 Git 面板
              </div>
            </div>
            <div
              className="od-radio-card"
              role="radio"
              aria-checked={kind === 'dir'}
              tabIndex={0}
              onClick={() => setKind('dir')}
              onKeyDown={(e) => {
                if (e.key === 'Enter' || e.key === ' ') {
                  e.preventDefault();
                  setKind('dir');
                }
              }}
            >
              <div className="od-radio-title">纯目录</div>
              <div className="od-radio-desc">
                填写任意普通目录路径，不要求 git 仓库；该目录下的多个任务将共享同一目录。
              </div>
            </div>
          </div>
        </div>
        {kind === 'dir' && (
          <div className="od-alert od-alert-info" style={{ marginBottom: 14 }}>
            <InfoIcon />
            <div className="od-alert-body">
              多个活跃任务共享同一目录、无文件隔离，并行改动可能相互影响。
            </div>
          </div>
        )}
        <div className="od-field">
          <label className="od-label" htmlFor="pjm-reg-name">项目名称</label>
          <input
            id="pjm-reg-name"
            ref={nameRef}
            className="od-input"
            data-od-id="register-project-name"
            placeholder="my-repo"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>
        <div className="od-field">
          <label className="od-label" htmlFor="pjm-reg-path">绝对路径</label>
          <input
            id="pjm-reg-path"
            className="od-input mono"
            placeholder={
              kind === 'repo'
                ? '/Users/you/code/my-repo'
                : '/Users/you/notes/scratch'
            }
            value={path}
            onChange={(e) => setPath(e.target.value)}
          />
          <div className="od-hint">
            {kind === 'repo'
              ? '填写已克隆到本机的项目目录路径，目录本身须已是 git 仓库；不支持直接填写远端仓库地址（http/ssh），如需使用请先自行 git clone 到本机。'
              : '填写任意普通目录路径，不要求 git 仓库；该目录下的多个任务将共享同一目录。'}
          </div>
        </div>
        {mutError && <div className="error-line">{mutError}</div>}
        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
          <button
            type="button"
            className="od-btn"
            onClick={onClose}
            disabled={creating}
          >
            取消
          </button>
          <button type="submit" className="od-btn od-btn-primary" disabled={creating}>
            {creating ? '注册中…' : '注册项目'}
          </button>
        </div>
      </form>
    </section>
  );
}

/** 删除项目两步确认。 */
function DeleteProjectConfirm({
  project,
  onCancel,
  onConfirm,
}: {
  project: Project;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  return (
    <div className="pjm-del show">
      <span>
        确认删除项目「{project.name}」？只删除任务记录与会话数据，<strong>绝不触碰该目录</strong>。
      </span>
      <button
        className="od-btn od-btn-sm od-btn-danger"
        onClick={onConfirm}
      >
        确认删除
      </button>
      <button className="od-btn od-btn-sm" onClick={onCancel}>
        取消
      </button>
    </div>
  );
}

/** 项目管理单页（master-detail）：左项目轨道列 + 右详情面板。
 *  - #/projects#<id> 深链选中（fragment 来自 resolveRoute）
 *  - 非法 id 不选中、展示空详情占位
 *  - ≤1024px 钻取式导航（详情含返回列表入口）
 *  - 数据：列表走共享 store useProjects；详情走现有 api.getProject + api.listTasks
 *  - mutation（注册/删除项目、任务行操作）成功后调 refresh()（runProjectMutation 范式） */
export function ProjectsManagePage() {
  const { projects, loading: storeLoading, initialized, error: storeError } = useProjects();
  const refresh = useProjectsRefresh();

  // hash 路由 fragment（原始深链 id，未校验是否存在项目列表）。
  const rawSelectedID = useSelectedProjectID();

  // 轨道搜索
  const [query, setQuery] = useState('');
  const filteredProjects = useMemo(
    () => filterProjects(projects, query),
    [projects, query],
  );
  // 搜索框引用：全局 `/` 快捷键聚焦（对齐设计稿 projects.html 占位提示「搜索项目（/）」）
  const searchRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return;
      const el = e.target as HTMLElement | null;
      // 正在输入（输入框/文本域/下拉/可编辑区域）时不劫持 `/`
      if (
        el &&
        (el.tagName === 'INPUT' ||
          el.tagName === 'TEXTAREA' ||
          el.tagName === 'SELECT' ||
          el.isContentEditable)
      ) {
        return;
      }
      e.preventDefault();
      searchRef.current?.focus();
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, []);

  // 注册表单开关
  const [registerOpen, setRegisterOpen] = useState(false);
  // 命令面板「注册项目」：展开后聚焦名称输入（od:palette-focus detail.id=register-project-name）
  const [focusRegisterNonce, setFocusRegisterNonce] = useState(0);

  // 对齐 design 源 ocdeck-palette.js 操作语义（导航后展开并聚焦注册表单）
  useEffect(() => {
    const openAndFocus = () => {
      setRegisterOpen(true);
      setFocusRegisterNonce((n) => n + 1);
      clearPendingPaletteFocus('register-project-name');
    };
    if (consumePendingPaletteFocus('register-project-name') !== null) openAndFocus();
    const onPaletteFocus = (e: Event) => {
      const parsed = readPaletteFocusDetail((e as CustomEvent).detail);
      if (!parsed || parsed.id !== 'register-project-name') return;
      openAndFocus();
    };
    document.addEventListener(PALETTE_FOCUS_EVENT, onPaletteFocus);
    return () => document.removeEventListener(PALETTE_FOCUS_EVENT, onPaletteFocus);
  }, []);

  // 删除项目两步确认
  const [confirmDeleteProject, setConfirmDeleteProject] = useState<Project | null>(null);
  const [mutError, setMutError] = useState('');

  // 详情数据（走现有 api，非共享 store 轮询）
  const [project, setProject] = useState<ProjectDetail | null>(null);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [detailLoading, setDetailLoading] = useState(false);
  // detailError：仅详情加载失败设置（loadDetail 内部管理），阻断详情渲染并提示。
  const [detailError, setDetailError] = useState('');
  // actionError：任务行操作失败设置，不阻断详情渲染，在内容区上方行内展示；
  // 由下次操作开始（handleTaskRowDone）清空，loadDetail 不触碰它（避免被刷新覆盖）。
  const [actionError, setActionError] = useState('');
  const [tab, setTab] = useState<DetailTab>('overview');
  // 详情按 projectID key 重挂载（切换项目时重置状态）
  const [detailKey, setDetailKey] = useState(0);

  // 钻取模式（≤1024px）：详情态隐藏列表
  const [drillDetail, setDrillDetail] = useState(false);

  // 任务删除弹窗 + init 日志弹窗
  const [deletingTask, setDeletingTask] = useState<Task | null>(null);
  const [initLogTask, setInitLogTask] = useState<Task | null>(null);

  // 校验选中 id 是否存在于列表：非法/已删除 id 不选中（展示空详情占位）。
  // 本页只消费 selectedProject / selectedID（已校验），不直接消费原始 rawSelectedID——
  // 非法深链不得请求 API、不得显示错误、不得进入窄屏详情态
  // （契约：非法 id 回退为不选中 + 空详情占位）。
  const selectedProject = useMemo(
    () => (rawSelectedID ? projects.find((p) => p.id === rawSelectedID) ?? null : null),
    [projects, rawSelectedID],
  );
  // 已校验的选中项目 id：selectedProject 存在时取其 id，否则 undefined。
  const selectedID = selectedProject?.id;

  // 详情加载的 generation guard：快速切换项目时旧请求不得覆盖新项目详情。
  // 切换项目递增 gen；写回时校验仍为当前代际才生效（项目 id 比对兜底）。
  const detailGenRef = useRef(0);

  const loadDetail = useCallback(async (id: string) => {
    const gen = ++detailGenRef.current;
    setDetailLoading(true);
    setDetailError('');
    try {
      const [p, ts] = await Promise.all([api.getProject(id), api.listTasks(id)]);
      // 代际失配或项目已切换：丢弃旧响应（不得覆盖新项目详情）
      if (gen !== detailGenRef.current) return;
      setProject(p);
      setTasks(ts);
    } catch (err) {
      if (gen !== detailGenRef.current) return;
      setProject(null);
      setTasks([]);
      setDetailError(err instanceof ApiError ? err.message : '加载失败');
    } finally {
      if (gen === detailGenRef.current) setDetailLoading(false);
    }
  }, []);

  // 选中项目（已校验）变化时加载详情；非法/空 id 不请求 API、重置为空详情占位。
  // 按 projectID key 重挂载：重置 tab/错误/数据。
  useEffect(() => {
    setTab('overview');
    setDetailError('');
    setActionError('');
    setDeletingTask(null);
    setInitLogTask(null);
    setDetailKey((k) => k + 1);
    if (selectedID) {
      void loadDetail(selectedID);
    } else {
      // 非法/空 id：推进代际使任何在途旧请求失效，重置为空详情占位
      detailGenRef.current += 1;
      setProject(null);
      setTasks([]);
      setDetailLoading(false);
    }
  }, [selectedID, loadDetail]);

  // 选中项目时进入钻取模式（≤1024px）；取消选中（非法 id/空）时退出钻取。
  // 只消费已校验的 selectedID（selectedProject?.id），非法 id 不进详情态。
  useEffect(() => {
    if (selectedID) setDrillDetail(true);
    else setDrillDetail(false);
  }, [selectedID]);

  const selectProject = (id: string) => {
    navigate(`/projects#${id}`);
  };

  const backToList = () => {
    setDrillDetail(false);
    navigate('/projects');
  };

  // mutation：注册项目成功后 refresh（RegisterForm 内部已处理 onRegistered → refresh via runProjectMutation）。
  // 删除项目：两步确认 + 仍有任务禁止删除（409 文案）。
  const removeProject = async (p: Project) => {
    setMutError('');
    await runProjectMutation({
      mutate: () => api.deleteProject(p.id),
      refresh,
      onSuccess: () => {
        setConfirmDeleteProject(null);
        // 删除成功后若删除的是当前选中项目，回到列表态
        if (selectedID === p.id) navigate('/projects');
      },
      onError: (err) => {
        setMutError(deleteErrorMessage(err));
        setConfirmDeleteProject(null);
      },
    });
  };

  // 任务行操作成功后：刷新详情任务列表 + 调共享 store refresh（本 lane mutation 接线，tasks.md 6.6）。
  // 成功开始即清 actionError（loadDetail 不清，保持与 mutation 错误分层）。
  const handleTaskRowDone = () => {
    setActionError('');
    if (selectedID) void loadDetail(selectedID);
    void refresh().catch(() => {});
  };

  // 任务行操作失败：设置 actionError（行内展示，不阻断详情）；不立即 loadDetail 清错误。
  // 注：fix-1 共享组件 TaskActions/RerunInitButton onDone 语义修复后，失败不再调 onDone，
  // 此处只在 onError 路径处理；成功路径由 handleTaskRowDone 刷新详情。
  const handleTaskRowError = (msg: string) => {
    setActionError(msg);
  };

  const handleTaskDeleted = () => {
    setDeletingTask(null);
    setActionError('');
    if (selectedID) void loadDetail(selectedID);
    void refresh().catch(() => {});
  };

  const error = mutError || storeError;

  return (
    <>
      <header className="od-page-head">
        <div className="od-page-title">
          <h1>项目</h1>
          <p className="muted" style={{ fontSize: 13 }}>
            注册一次工作区，之后每个新任务自动准备好
          </p>
        </div>
        <div className="od-page-actions">
          <button
            className="od-btn od-btn-primary"
            aria-expanded={registerOpen}
            aria-controls="pjm-register-card"
            onClick={() => setRegisterOpen((v) => !v)}
          >
            {registerOpen ? '收起注册' : '注册项目'}
          </button>
        </div>
      </header>

      {error && <div className="error-line">{error}</div>}

      <RegisterForm
        open={registerOpen}
        focusNameNonce={focusRegisterNonce}
        onClose={() => setRegisterOpen(false)}
        onRegistered={() => setRegisterOpen(false)}
      />

      {storeLoading && !initialized && <div className="od-empty">加载中…</div>}

      {initialized && projects.length === 0 && !registerOpen && (
        <div className="od-empty">
          还没有项目。注册一个 git 仓库或纯目录开始编排任务。
        </div>
      )}

      <div
        className={`pjm-split${drillDetail ? ' m-detail' : ''}`}
        data-od-id="workspace-split"
      >
        {/* ============ 左栏：项目轨道 ============ */}
        <div className="pjm-rail-col" data-od-id="project-rail-col">
          <div className="pjm-search" data-od-id="project-search">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <circle cx="11" cy="11" r="7" />
              <line x1="21" y1="21" x2="16.5" y2="16.5" />
            </svg>
            <input
              ref={searchRef}
              className="od-input"
              type="search"
              autoComplete="off"
              placeholder="搜索项目（/）"
              aria-label="搜索项目，按名称、路径或类型过滤"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Escape') {
                  setQuery('');
                } else if (e.key === 'Enter') {
                  const first = filteredProjects[0];
                  if (first) selectProject(first.id);
                }
              }}
            />
          </div>
          {query.trim() && (
            <div className="pjm-search-count" aria-live="polite">
              匹配 {filteredProjects.length} / {projects.length} 个项目
            </div>
          )}
          <div className="pjm-rail" role="tablist" aria-label="项目列表" data-od-id="project-rail">
            {filteredProjects.map((p) => {
              const activeCount = (p.tasks_by_status ?? {}).active ?? 0;
              // 有活跃任务待人工时轨道行用蓝点（等待人工），区别于正常运行绿点。
              const needsAttention = (p.tasks ?? []).some((t) => t.status === 'active' && (t.attention_count ?? 0) > 0);
              return (
                <button
                  key={p.id}
                  className="pjm-rail-item"
                  role="tab"
                  aria-selected={selectedID === p.id}
                  aria-controls={`pjm-detail-${p.id}`}
                  onClick={() => selectProject(p.id)}
                >
                  <span className="pjm-rail-name">
                    <ProjectRailIcon kind={p.kind} />
                    {p.name}
                  </span>
                  <span className="pjm-rail-sub">
                    {p.kind === 'dir' ? (
                      `纯目录 · ${p.task_count} 个任务`
                    ) : (
                      <>
                        {/* 活跃 >0 时脉冲点（od-agent，对齐设计稿轨道行）：待人工蓝点优先于运行绿点 */}
                        {activeCount > 0 && (
                          <span className={`od-agent ${needsAttention ? 'od-agent-attention' : 'od-agent-busy'}`} aria-hidden>
                            <span className="od-agent-dot" />
                          </span>
                        )}
                        {activeCount > 0 ? `活跃 ${activeCount} · ` : ''}共 {p.task_count} 个任务
                      </>
                    )}
                  </span>
                </button>
              );
            })}
          </div>
          {filteredProjects.length === 0 && projects.length > 0 && (
            <div className="pjm-rail-empty">没有匹配的项目</div>
          )}
        </div>

        {/* ============ 右栏：详情面板 ============ */}
        <DetailPanel
          key={detailKey}
          project={project}
          tasks={tasks}
          loading={detailLoading}
          error={detailError}
          actionError={actionError}
          tab={tab}
          onTabChange={setTab}
          onBack={backToList}
          onDeleteProject={() => {
            if (selectedProject) setConfirmDeleteProject(selectedProject);
          }}
          onTaskRowDone={handleTaskRowDone}
          onTaskRowError={handleTaskRowError}
          onDeleteTask={setDeletingTask}
          onInitLog={setInitLogTask}
        />
      </div>

      {/* 删除项目两步确认（挂在详情面板下方，与设计稿一致） */}
      {confirmDeleteProject && (
        <DeleteProjectConfirm
          project={confirmDeleteProject}
          onCancel={() => setConfirmDeleteProject(null)}
          onConfirm={() => void removeProject(confirmDeleteProject)}
        />
      )}

      {/* 删除任务弹窗（复用现有组件，行为等价） */}
      {deletingTask && (
        <DeleteTaskModal
          task={deletingTask}
          onClose={() => setDeletingTask(null)}
          onDeleted={handleTaskDeleted}
        />
      )}

      {/* init 日志弹窗 */}
      {initLogTask && (
        <LifecycleLogModal
          title={`init 日志 · ${initLogTask.name}`}
          fetchLog={() => api.getInitLog(initLogTask.id)}
          onClose={() => setInitLogTask(null)}
        />
      )}
    </>
  );
}
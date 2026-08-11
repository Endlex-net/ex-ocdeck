import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { api, ApiError } from '../api';
import { navigate } from '../router';
import { usePoll, useProjects, useProjectsRefresh } from '../hooks';
import {
  isTransitional,
  parseNotice,
  type ActiveSessionItem,
  type Project,
} from '../types';
import { StatusBadge } from '../components/StatusBadge';
import { AgentStatusBadge } from '../components/AgentStatusBadge';
import { TaskActions } from '../components/TaskActions';
import { RerunInitButton } from '../components/RerunInitButton';
import { DeleteTaskModal } from '../components/DeleteTaskModal';
import { LifecycleLogModal } from '../components/LifecycleLogModal';
import { BranchIcon, WarnIcon, InfoIcon, PlusIcon, RetryIcon } from '../icons';
import {
  AttentionKind,
  buildCommandCenterView,
  hasPreDeleteLog,
  attentionKindLabel,
  type AttentionItem,
  type MergedTask,
} from './command-center-selector';
import { createErrorMessage } from '../hooks';
import {
  PALETTE_FOCUS_EVENT,
  clearPendingPaletteFocus,
  consumePendingPaletteFocus,
} from '../palette-focus';
import './command-center.css';

/** sessions/active 快照（本页 5s single-flight 轮询）。 */
interface SessionsSnapshot {
  sessions: ActiveSessionItem[];
  loading: boolean;
  /** 至少成功过一次（成功空响应也算）。 */
  initialized: boolean;
  /** 至少完成过一次请求（成功或失败）；首次失败后为 true，避免卡在 loading。 */
  attempted: boolean;
  error: string;
  /** 最近一次成功刷新的 Unix 毫秒；0 表示尚未成功过。 */
  lastSuccessAt: number;
}

const EMPTY: SessionsSnapshot = {
  sessions: [],
  loading: false,
  initialized: false,
  attempted: false,
  error: '',
  lastSuccessAt: 0,
};

/* 纯函数：分支刷新/加载的代际与项目 ID 校验（防陈旧写回）。
 *  异步结果仅在「代际未变 + 当前选择仍是同一项目」时才可写回。 */
export function shouldAcceptBranchResult(
  gen: number,
  currentGen: number,
  projectId: string,
  currentProjectId: string | null,
): boolean {
  return gen === currentGen && currentProjectId === projectId;
}

/** 纯函数：finally 是否应清 refreshing 标志。
 *  仅当本次刷新仍是当前刷新项目时清；否则已由项目切换 effect 重置（避免清错新项目）。 */
export function shouldClearRefreshing(
  refreshingProjectId: string | null,
  thisProjectId: string,
): boolean {
  return refreshingProjectId === thisProjectId;
}

/**
 * 指挥中心 sessions 首屏状态机（纯函数，可测）。
 * - loading：projects 未就绪或 sessions 尚未完成首次请求，且尚无数据
 * - error：sessions 首次失败（attempted && !initialized && error），不与 loading/空态并存
 * - empty：两侧均已成功初始化且三区皆空
 * - ready：其余（有数据或分区可渲染）
 */
export type SessionsBootstrapPhase = 'loading' | 'error' | 'empty' | 'ready';

export function resolveSessionsBootstrap(opts: {
  projectsInit: boolean;
  projectsLen: number;
  sessionsAttempted: boolean;
  sessionsInitialized: boolean;
  sessionsLen: number;
  sessionsError: string;
  attentionLen: number;
  activeLen: number;
  parkedLen: number;
}): SessionsBootstrapPhase {
  const hasAnyData =
    opts.projectsLen > 0 ||
    opts.sessionsLen > 0 ||
    opts.attentionLen > 0 ||
    opts.activeLen > 0 ||
    opts.parkedLen > 0;

  // 首次 sessions 失败：只报错（不 loading、不空态）
  if (opts.sessionsAttempted && !opts.sessionsInitialized && opts.sessionsError) {
    return 'error';
  }

  // 仍在等首次结果且无数据 → loading
  if ((!opts.projectsInit || !opts.sessionsAttempted) && !hasAnyData) {
    return 'loading';
  }

  // 两侧均成功初始化且无任务 → 真空态
  if (
    opts.projectsInit &&
    opts.sessionsInitialized &&
    opts.attentionLen === 0 &&
    opts.activeLen === 0 &&
    opts.parkedLen === 0
  ) {
    return 'empty';
  }

  return 'ready';
}

/**
 * 分区空态是否展示（纯函数）。
 * - loading / error：抑制「暂无…」空态占位
 * - empty / ready：允许分区空态占位
 * 注意：只门禁空态占位，非空列表 MUST 始终渲染（双快照：projects-only 任务仍须呈现）。
 */
export function shouldShowSectionEmpty(phase: SessionsBootstrapPhase): boolean {
  return phase === 'empty' || phase === 'ready';
}

/**
 * 分区主体渲染模式（纯函数）。
 * - list：有条目 → 始终渲染列表（sessions 失败/加载中不隐藏 projects 数据）
 * - empty：无条目且 phase 允许 → 「暂无…」
 * - none：无条目且 phase 为 loading/error → 不渲染占位（只留错误条/加载指示）
 */
export function sectionBodyMode(
  phase: SessionsBootstrapPhase,
  itemCount: number,
): 'list' | 'empty' | 'none' {
  if (itemCount > 0) return 'list';
  if (shouldShowSectionEmpty(phase)) return 'empty';
  return 'none';
}

export function CommandCenterPage() {
  // 共享 projects store（design.md D4：侧栏、指挥中心同一数据源，MUST NOT 自行轮询 /projects）
  const { projects, initialized: projectsInit, error: storeError } = useProjects();
  const refresh = useProjectsRefresh();

  // 本页 sessions/active 轮询（5s single-flight）
  const [snap, setSnap] = useState<SessionsSnapshot>(EMPTY);
  const inflightRef = useRef(false);
  const pollSessions = async () => {
    if (inflightRef.current) return; // single-flight
    inflightRef.current = true;
    try {
      const sessions = await api.listActiveSessions();
      setSnap({
        sessions,
        loading: false,
        initialized: true,
        attempted: true,
        error: '',
        lastSuccessAt: Date.now(),
      });
    } catch (err) {
      // 失败：attempted=true 退出 loading；initialized 仅成功置 true（空响应也算成功）。
      // 保留旧 sessions + lastSuccessAt；error 独立展示，不与 loading/空态并存。
      setSnap((s) => ({
        sessions: s.sessions,
        loading: false,
        initialized: s.initialized,
        attempted: true,
        error: err instanceof ApiError ? `[${err.code}] ${err.message}` : '加载活跃会话失败',
        lastSuccessAt: s.lastSuccessAt,
      }));
    } finally {
      inflightRef.current = false;
    }
  };
  usePoll(pollSessions, 5000, []);

  const view = useMemo(
    () => buildCommandCenterView(projects, snap.sessions),
    [projects, snap.sessions],
  );

  const activeCount = view.active.length;
  const attentionCount = view.attention.length;
  // 刷新时间指示：未成功过显示"加载中…"；成功过显示相对时间（对齐设计稿"刚刚刷新"文案）；失败时错误提示独立展示
  const lastRefresh = snap.lastSuccessAt
    ? `${relTimeMs(snap.lastSuccessAt)}刷新`
    : snap.attempted && snap.error
      ? '刷新失败'
      : '加载中…';

  const bootstrap = resolveSessionsBootstrap({
    projectsInit,
    projectsLen: projects.length,
    sessionsAttempted: snap.attempted,
    sessionsInitialized: snap.initialized,
    sessionsLen: snap.sessions.length,
    sessionsError: snap.error,
    attentionLen: view.attention.length,
    activeLen: view.active.length,
    parkedLen: view.parked.length,
  });
  const initialLoading = bootstrap === 'loading';
  const isEmpty = bootstrap === 'empty';
  const activeBody = sectionBodyMode(bootstrap, view.active.length);
  const parkedBody = sectionBodyMode(bootstrap, view.parked.length);
  // 行内操作错误（页面级，非 store 错误）
  const [opError, setOpError] = useState('');
  const [deleteTarget, setDeleteTarget] = useState<MergedTask | null>(null);
  const [logTarget, setLogTarget] = useState<{ title: string; fetchLog: () => Promise<string> } | null>(null);
  // 新建任务面板展开状态（按钮常驻页头，面板作为页头之后的同级内容展开/收起）
  const [newTaskOpen, setNewTaskOpen] = useState(false);
  // 命令面板「新建任务」：展开后聚焦任务名（od:palette-focus detail.id=new-task-name）
  const [focusTaskNameNonce, setFocusTaskNameNonce] = useState(0);

  // 对齐 design 源 ocdeck-palette.js + command-center.html:328-330
  useEffect(() => {
    const openAndFocus = () => {
      setNewTaskOpen(true);
      setFocusTaskNameNonce((n) => n + 1);
      clearPendingPaletteFocus('new-task-name');
    };
    // 跨路由：mount 时消费在途 pending（navigate 后 listener 尚未注册的竞态）
    if (consumePendingPaletteFocus('new-task-name')) openAndFocus();
    const onPaletteFocus = (e: Event) => {
      const id = (e as CustomEvent<{ id?: string }>).detail?.id;
      if (id !== 'new-task-name') return;
      openAndFocus();
    };
    document.addEventListener(PALETTE_FOCUS_EVENT, onPaletteFocus);
    return () => document.removeEventListener(PALETTE_FOCUS_EVENT, onPaletteFocus);
  }, []);

  const showOpError = (msg: string) => setOpError(msg);
  const onTaskChanged = () => {
    void refresh().catch(() => {});
  };

  return (
    <div className="cc-page">
      <header className="od-page-head">
        <div className="od-page-title">
          <h1>指挥中心</h1>
          <p className="muted cc-head-sub">
            {activeCount} 个活跃任务 · {attentionCount} 个需要关注 · 每 5 秒刷新
          </p>
        </div>
        <div className="od-page-actions">
          <span className="cc-poll-status">{lastRefresh}</span>
          <button
            className="od-btn od-btn-primary"
            onClick={() => setNewTaskOpen((v) => !v)}
            aria-expanded={newTaskOpen}
            aria-controls="cc-new-task-panel"
          >
            <PlusIcon /> 新建任务
          </button>
        </div>
      </header>

      {/* 新建任务面板：页头之后的同级内容，展开/收起（不替换页头按钮，按设计稿 command-center.html:94） */}
      {newTaskOpen && (
        <NewTaskPanel
          projects={projects}
          refresh={refresh}
          storeError={storeError}
          onClose={() => setNewTaskOpen(false)}
          focusTaskNameNonce={focusTaskNameNonce}
        />
      )}

      {(opError || storeError || snap.error) && (
        <div className="error-line">
          {opError || storeError || snap.error}
        </div>
      )}

      {/* 需要关注 */}
      {view.attention.length > 0 && (
        <section className="cc-section">
          <h2 className="od-section-title">需要关注</h2>
          <div className="od-rows cc-attention">
            {view.attention.map((item) => (
              <AttentionRow
                key={item.task.id}
                item={item}
                onError={showOpError}
                onTaskChanged={onTaskChanged}
                onDelete={setDeleteTarget}
                onShowLog={setLogTarget}
              />
            ))}
          </div>
        </section>
      )}

      {/* 其余活跃任务：非空列表始终渲染；空态占位仅 phase 允许时显示 */}
      <section className="cc-section">
        <h2 className="od-section-title">其余活跃任务</h2>
        {activeBody === 'list' ? (
          <div className="od-rows">
            {view.active.map((m) => (
              <TaskRow key={m.task.id} m={m} />
            ))}
          </div>
        ) : activeBody === 'empty' ? (
          <div className="od-empty">暂无活跃任务</div>
        ) : null}
      </section>

      {/* 挂起与归档：同上，门禁只作用于空态占位 */}
      <section className="cc-section">
        <h2 className="od-section-title">挂起与归档</h2>
        {parkedBody === 'list' ? (
          <div className="od-rows">
            {view.parked.map((m) => (
              <ParkedRow key={m.task.id} m={m} onError={showOpError} onTaskChanged={onTaskChanged} onDelete={setDeleteTarget} />
            ))}
          </div>
        ) : parkedBody === 'empty' ? (
          <div className="od-empty">暂无挂起或归档任务</div>
        ) : null}
      </section>

      {/* 真空态引导（已初始化且无任何任务） */}
      {isEmpty && (
        <div className="od-empty cc-empty-guide">
          <p>系统内暂无任务。在页头点击「新建任务」开始。</p>
        </div>
      )}

      {/* 加载占位（首次无数据时，不与空态/分区同时出现） */}
      {initialLoading && (
        <div className="od-empty">
          <span className="spinner spinner-inline" aria-hidden /> 加载中…
        </div>
      )}

      {deleteTarget && (
        <DeleteTaskModal
          task={toTask(deleteTarget)}
          onClose={() => setDeleteTarget(null)}
          onDeleted={() => {
            setDeleteTarget(null);
            onTaskChanged();
          }}
        />
      )}

      {logTarget && (
        <LifecycleLogModal
          title={logTarget.title}
          fetchLog={logTarget.fetchLog}
          onClose={() => setLogTarget(null)}
        />
      )}
    </div>
  );
}

/** MergedTask → Task 适配（DeleteTaskModal/TaskActions 需要 Task 形状）。
 *  project_kind 来自 MergedTask；init_error/delete_mode/last_port 等非指挥中心操作所需字段缺省。 */
function toTask(m: MergedTask) {
  return {
    id: m.task.id,
    project_id: m.project_id,
    project_kind: m.project_kind,
    name: m.task.name,
    branch: m.task.branch,
    status: m.task.status,
    worktree_path: m.task.worktree_path,
    last_error: m.task.last_error,
    notice: m.task.notice,
    init_status: m.task.init_status,
    created_at: 0,
    updated_at: m.task.updated_at,
    agentStatus: m.agentStatus,
    attention: m.attention,
  };
}

/** 「需要关注」行：主类别呈现 + 次要标记 + 行内操作集。 */
function AttentionRow({
  item,
  onError,
  onTaskChanged,
  onDelete,
  onShowLog,
}: {
  item: AttentionItem;
  onError: (msg: string) => void;
  onTaskChanged: () => void;
  onDelete: (m: MergedTask) => void;
  onShowLog: (t: { title: string; fetchLog: () => Promise<string> }) => void;
}) {
  const t = item.task;
  const gotoWorkbench = () => navigate(`/task/${t.id}?from=home`);
  const secondaryLabels = item.secondary.map((k) => attentionKindLabel(k));
  // 等待类提示（权限/问题）与失败态用琥珀色强调（warn 派生，对齐设计稿 cc-row-hint.urgent）
  const hintUrgent =
    item.kind === AttentionKind.Failed ||
    item.kind === AttentionKind.PermissionPending ||
    item.kind === AttentionKind.QuestionPending;

  return (
    <div
      className={`od-row od-row-clickable${item.kind === AttentionKind.Failed ? ' cc-row-failed' : ''}`}
      onClick={gotoWorkbench}
      role="button"
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === 'Enter') gotoWorkbench();
      }}
    >
      <div className="od-row-main">
        <div className="od-row-title">
          {t.name}
          {t.status === 'suspended' && <span className="badge badge-suspended">挂起</span>}
          <AgentStatusBadge agentStatus={t.agentStatus} />
        </div>
        <div className="od-row-sub mono">
          {item.project_name} · <BranchIcon /> {t.branch}
        </div>
        {/* 主类别提示 */}
        <div className={`cc-row-hint${hintUrgent ? ' urgent' : ''}`}>
          {mainHint(item)}
          {t.last_error && <div className="cc-row-error">{t.last_error}</div>}
        </div>
        {/* 次要标记 */}
        {secondaryLabels.length > 0 && (
          <div className="cc-row-secondary">{secondaryLabels.map((l) => <span key={l} className="badge badge-muted">{l}</span>)}</div>
        )}
      </div>
      <div className="od-row-side cc-row-actions" onClick={(e) => e.stopPropagation()}>
        <span className="muted cc-rel-time">{relTime(item.last_active_at)}</span>
        <AttentionActions item={item} onError={onError} onTaskChanged={onTaskChanged} onDelete={onDelete} onShowLog={onShowLog} gotoWorkbench={gotoWorkbench} />
      </div>
    </div>
  );
}

/** 主类别提示文案。 */
function mainHint(item: AttentionItem): string {
  switch (item.kind) {
    case AttentionKind.Failed:
      return item.task.status === 'creation_failed' ? '创建失败' : '删除失败';
    case AttentionKind.InitFailed:
      return 'init 脚本失败';
    case AttentionKind.PermissionPending:
      return '等待权限确认';
    case AttentionKind.QuestionPending:
      return '等待回答问题';
    case AttentionKind.Notice:
      return '有 notice 提示';
    case AttentionKind.AgentIdle:
      return 'agent 空闲，等待输入';
  }
}

/** 「需要关注」行内操作集（按 D7 / spec.md）。 */
function AttentionActions({
  item,
  onError,
  onTaskChanged,
  onDelete,
  onShowLog,
  gotoWorkbench,
}: {
  item: AttentionItem;
  onError: (msg: string) => void;
  onTaskChanged: () => void;
  onDelete: (m: MergedTask) => void;
  onShowLog: (t: { title: string; fetchLog: () => Promise<string> }) => void;
  gotoWorkbench: () => void;
}) {
  const t = item.task;
  const status = t.status;

  // 1. 失败态：creation_failed → 重试 + 普通删除（无强制删除）；deletion_failed → 重试 + 强制删除 + pre-delete 日志
  if (status === 'creation_failed') {
    return (
      <>
        <TaskActions task={toTask(itemToMerged(item))} onDone={onTaskChanged} onError={onError} />
        <button className="od-btn od-btn-sm" onClick={() => onDelete(itemToMerged(item))}>删除</button>
        <button className="od-btn od-btn-sm od-btn-ghost" onClick={gotoWorkbench}>进入工作台</button>
      </>
    );
  }
  if (status === 'deletion_failed') {
    return (
      <>
        <TaskActions task={toTask(itemToMerged(item))} onDone={onTaskChanged} onError={onError} />
        {hasPreDeleteLog(t.last_error) && (
          <button
            className="od-btn od-btn-sm od-btn-ghost"
            onClick={() => onShowLog({ title: 'pre-delete 日志', fetchLog: () => api.getPreDeleteLog(t.id) })}
          >
            pre-delete 日志
          </button>
        )}
        <button className="od-btn od-btn-sm" onClick={() => onDelete(itemToMerged(item))}>强制删除</button>
        <button className="od-btn od-btn-sm od-btn-ghost" onClick={gotoWorkbench}>进入工作台</button>
      </>
    );
  }
  // 2. init 失败：查看日志 + 重跑初始化
  if (item.kind === AttentionKind.InitFailed) {
    return (
      <>
        <button
          className="od-btn od-btn-sm od-btn-ghost"
          onClick={() => onShowLog({ title: 'init 日志', fetchLog: () => api.getInitLog(t.id) })}
        >
          查看日志
        </button>
        <RerunInitButton task={toTask(itemToMerged(item))} onDone={onTaskChanged} onError={onError} />
        <button className="od-btn od-btn-sm od-btn-ghost" onClick={gotoWorkbench}>进入工作台</button>
      </>
    );
  }
  // 3/4. 权限/问题等待：点击跳工作台（审批/回答在 TUI 内）
  if (
    item.kind === AttentionKind.PermissionPending ||
    item.kind === AttentionKind.QuestionPending
  ) {
    return (
      <button className="od-btn od-btn-sm" onClick={gotoWorkbench}>进入工作台</button>
    );
  }
  // 5/6. notice / idle：跳工作台
  return (
    <button className="od-btn od-btn-sm od-btn-ghost" onClick={gotoWorkbench}>进入工作台</button>
  );
}

/** AttentionItem → MergedTask 适配（操作组件需要 MergedTask 形状）。 */
function itemToMerged(item: AttentionItem): MergedTask {
  return {
    task: item.task,
    project_id: item.project_id,
    project_name: item.project_name,
    project_kind: item.project_kind,
    last_active_at: item.last_active_at,
    agentStatus: item.task.agentStatus,
    attention: { permissions: [], questions: [] },
    projectsOnly: false,
  };
}

/** 「其余活跃任务」行：点击跳工作台 + 过渡徽章 + agent 状态。 */
function TaskRow({ m }: { m: MergedTask }) {
  const goto = () => navigate(`/task/${m.task.id}?from=home`);
  return (
    <div
      className={`od-row od-row-clickable${isTransitional(m.task.status) ? ' cc-row-trans' : ''}`}
      onClick={goto}
      role="button"
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === 'Enter') goto();
      }}
    >
      <div className="od-row-main">
        <div className="od-row-title">
          {m.task.name}
          {isTransitional(m.task.status) ? (
            <StatusBadge status={m.task.status} />
          ) : null}
          <AgentStatusBadge agentStatus={m.agentStatus} />
        </div>
        <div className="od-row-sub mono">
          {m.project_name} · <BranchIcon /> {m.task.branch}
        </div>
        {m.task.notice && parseNotice(m.task.notice).length > 0 && (
          <div className="cc-row-hint">
            <WarnIcon /> {parseNotice(m.task.notice).map((n) => n.message).join('；')}
          </div>
        )}
      </div>
      <div className="od-row-side" onClick={(e) => e.stopPropagation()}>
        <span className="muted cc-rel-time">{relTime(m.last_active_at)}</span>
        <button className="od-btn od-btn-sm od-btn-ghost" onClick={goto}>进入工作台</button>
      </div>
    </div>
  );
}

/** 「挂起与归档」行：状态机一致操作（复用 TaskActions）。 */
function ParkedRow({
  m,
  onError,
  onTaskChanged,
  onDelete,
}: {
  m: MergedTask;
  onError: (msg: string) => void;
  onTaskChanged: () => void;
  onDelete: (m: MergedTask) => void;
}) {
  const status = m.task.status;
  const goto = () => navigate(`/task/${m.task.id}?from=home`);
  return (
    <div
      className="od-row od-row-clickable"
      onClick={goto}
      role="button"
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === 'Enter') goto();
      }}
    >
      <div className="od-row-main">
        <div className="od-row-title">
          {m.task.name}
          {status === 'suspended' ? <span className="badge badge-suspended">挂起</span> : <span className="badge badge-archived">归档</span>}
        </div>
        <div className="od-row-sub mono">
          {m.project_name} · <BranchIcon /> {m.task.branch}
        </div>
      </div>
      <div className="od-row-side" onClick={(e) => e.stopPropagation()}>
        <span className="muted cc-rel-time">{relTime(m.task.updated_at)}</span>
        <TaskActions task={toTask(m)} onDone={onTaskChanged} onError={onError} />
        {/* 归档任务无 TaskActions（restore 由 TaskActions 提供）；挂起可删除，归档可删除。
            删除用 danger 同形按钮（od-btn-sm 尺寸与相邻操作一致，危险色边框/文字区分破坏性操作） */}
        <button className="od-btn od-btn-sm od-btn-danger" onClick={() => onDelete(m)}>删除</button>
      </div>
    </div>
  );
}

/** 相对时间文案（简化：秒级时间戳 → "N 分钟前"等）。 */
function relTime(ts: number): string {
  if (!ts) return '';
  const now = Math.floor(Date.now() / 1000);
  const diff = now - ts;
  if (diff < 60) return '刚刚';
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  return `${Math.floor(diff / 86400)} 天前`;
}

/** 毫秒级时间戳 → 相对时间文案（刷新指示用，反映最后成功刷新）。 */
function relTimeMs(ms: number): string {
  return relTime(Math.floor(ms / 1000));
}

// ==================== 内联新建任务面板 ====================

/** 内联新建任务面板：项目可过滤下拉 + 任务名 + 基准分支（repo）+ 刷新远端分支 + dir 警告。
 *  提交门禁：有效项目 ID + 非空任务名才可 POST；偏离已选清除 ID；base_ref 仅 repo；在途防重复。 */
function NewTaskPanel({
  projects,
  refresh,
  storeError,
  onClose,
  focusTaskNameNonce = 0,
}: {
  projects: Project[];
  refresh: () => Promise<void>;
  storeError: string;
  onClose: () => void;
  /** 命令面板触发展开后递增，用于聚焦任务名输入（new-task-name）。 */
  focusTaskNameNonce?: number;
}) {
  const [projectQuery, setProjectQuery] = useState('');
  const [selectedProject, setSelectedProject] = useState<Project | null>(null);
  const [projListOpen, setProjListOpen] = useState(false);
  const [taskName, setTaskName] = useState('');
  const [baseRef, setBaseRef] = useState('');
  const [branches, setBranches] = useState<string[]>([]);
  const [branchesError, setBranchesError] = useState('');
  const [branchListOpen, setBranchListOpen] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [creating, setCreating] = useState(false);
  const [mutError, setMutError] = useState('');
  const projInputId = useId();
  const branchInputId = useId();
  const taskNameRef = useRef<HTMLInputElement>(null);
  // 代际防陈旧写回 + 最新选择 ref（异步闭包读最新值而非闭包捕获值）
  const projGenRef = useRef(0);
  const selectedProjectRef = useRef<Project | null>(null);
  const refreshInFlightRef = useRef(false);
  // 当前正在刷新的项目 ID：finally 仅清自身项目的 refreshing，避免清错新项目
  const refreshingProjectIdRef = useRef<string | null>(null);

  // 面板挂载或 palette-focus 触发后聚焦任务名（design: taskName / data-od-id=new-task-name）
  useEffect(() => {
    requestAnimationFrame(() => taskNameRef.current?.focus());
  }, [focusTaskNameNonce]);

  const isDir = selectedProject?.kind === 'dir';
  const filteredProjects = useMemo(() => {
    const q = projectQuery.trim().toLowerCase();
    return q ? projects.filter((p) => p.name.toLowerCase().includes(q)) : projects;
  }, [projects, projectQuery]);

  // 选中项目时加载分支（repo 项目）。
  // 任何选择变化（含切到 dir/清空）都推进代际，使此前未完成的 listBranches/refreshBranches 失效。
  useEffect(() => {
    // 推进代际（清空/切到 dir 也推进，使在途异步响应失效）
    ++projGenRef.current;
    selectedProjectRef.current = selectedProject;
    // 切换项目时重置刷新中状态（与旧项目解耦，避免 B 永久处于刷新中）
    setRefreshing(false);
    refreshInFlightRef.current = false;
    refreshingProjectIdRef.current = null;
    if (!selectedProject || selectedProject.kind === 'dir') {
      setBranches([]);
      setBaseRef('');
      return;
    }
    setBaseRef(selectedProject.default_branch);
    setBranchesError('');
    const gen = projGenRef.current;
    api
      .listBranches(selectedProject.id)
      .then((bs) => {
        if (shouldAcceptBranchResult(gen, projGenRef.current, selectedProject.id, selectedProjectRef.current?.id ?? null))
          setBranches(bs);
      })
      .catch((err) => {
        if (shouldAcceptBranchResult(gen, projGenRef.current, selectedProject.id, selectedProjectRef.current?.id ?? null))
          setBranchesError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '获取分支列表失败');
      });
  }, [selectedProject]);

  const refreshBranches = async () => {
    if (!selectedProject || isDir || refreshInFlightRef.current) return;
    refreshInFlightRef.current = true;
    // 推进代际并捕获本次刷新所属项目 ID：刷新项目 A 后切到 B，A 的结果不得覆盖 B
    const gen = ++projGenRef.current;
    const projectId = selectedProject.id;
    refreshingProjectIdRef.current = projectId;
    setRefreshing(true);
    setBranchesError('');
    try {
      const bs = await api.refreshBranches(projectId);
      // 校验代际未变且当前选择仍是同一项目（ref 读最新值，不用闭包捕获值）
      if (shouldAcceptBranchResult(gen, projGenRef.current, projectId, selectedProjectRef.current?.id ?? null))
        setBranches(bs);
    } catch (err) {
      if (gen === projGenRef.current)
        setBranchesError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '刷新远端分支失败');
    } finally {
      refreshInFlightRef.current = false;
      // 仅当本次刷新仍是当前刷新项目时清 refreshing；否则已由项目切换 effect 重置
      if (shouldClearRefreshing(refreshingProjectIdRef.current, projectId)) {
        refreshingProjectIdRef.current = null;
        setRefreshing(false);
      }
    }
  };

  // 偏离已选项目：用户继续编辑项目输入导致 query 不等于已选项目名 → 清除 ID
  const onProjectQueryChange = (v: string) => {
    setProjectQuery(v);
    if (selectedProject && v !== selectedProject.name) setSelectedProject(null);
  };

  const branchOptions = useMemo(() => {
    const base = branches.length > 0 ? branches : selectedProject ? [selectedProject.default_branch] : [];
    return baseRef && !base.includes(baseRef) ? [baseRef, ...base] : base;
  }, [branches, selectedProject, baseRef]);

  const filteredBranches = useMemo(() => {
    const q = baseRef.trim().toLowerCase();
    return q ? branchOptions.filter((b) => b.toLowerCase().includes(q)) : branchOptions;
  }, [branchOptions, baseRef]);

  const canSubmit = !!selectedProject && taskName.trim() !== '' && !creating && !refreshing;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!canSubmit || !selectedProject) return;
    setCreating(true);
    setMutError('');
    const proj = selectedProject;
    try {
      const t = await api.createTask(proj.id, taskName.trim(), isDir ? undefined : baseRef || undefined);
      setTaskName('');
      // mutation 成功：跳转工作台（from=home）+ trailing refresh（失败静默，store error 通道承担）
      navigate(`/task/${t.id}?from=home`);
      void refresh().catch(() => {});
      onClose();
    } catch (err) {
      setMutError(createErrorMessage(err));
    } finally {
      setCreating(false);
    }
  };

  return (
    <div className="od-card cc-new-task-panel" id="cc-new-task-panel" aria-label="新建任务">
      <h2 className="od-section-title">新建任务</h2>
      <form onSubmit={submit}>
        {/* 项目可过滤下拉 */}
        <div className="od-field cc-combo">
          <label className="od-label" htmlFor={projInputId}>项目</label>
          <input
            id={projInputId}
            className="od-input"
            role="combobox"
            aria-expanded={projListOpen}
            autoComplete="off"
            spellCheck={false}
            value={selectedProject ? selectedProject.name : projectQuery}
            placeholder="搜索并选择项目"
            onChange={(e) => onProjectQueryChange(e.target.value)}
            onFocus={() => setProjListOpen(true)}
            onBlur={() => setTimeout(() => setProjListOpen(false), 150)}
          />
          {projListOpen && (
            <div className="cc-combo-list open" role="listbox">
              {filteredProjects.length === 0 ? (
                <div className="cc-combo-empty">无匹配项目</div>
              ) : (
                filteredProjects.map((p) => (
                  <button
                    key={p.id}
                    type="button"
                    className="cc-combo-item"
                    onMouseDown={(e) => e.preventDefault()}
                    onClick={() => {
                      setSelectedProject(p);
                      setProjectQuery(p.name);
                      setProjListOpen(false);
                    }}
                  >
                    {p.name}
                    <span className="cc-kind">{p.kind === 'dir' ? '纯目录' : 'git 仓库'}</span>
                  </button>
                ))
              )}
            </div>
          )}
        </div>

        {/* 基准分支（仅 repo） */}
        {selectedProject && !isDir && (
          <div className="od-field cc-combo">
            <label className="od-label" htmlFor={branchInputId}>基准分支</label>
            <input
              id={branchInputId}
              className="od-input mono"
              role="combobox"
              aria-expanded={branchListOpen}
              autoComplete="off"
              spellCheck={false}
              value={baseRef}
              placeholder="选择基准分支"
              onChange={(e) => setBaseRef(e.target.value)}
              onFocus={() => setBranchListOpen(true)}
              onBlur={() => setTimeout(() => setBranchListOpen(false), 150)}
            />
            {branchListOpen && (
              <div className="cc-combo-list open" role="listbox">
                {refreshing && <div className="cc-combo-empty">刷新中…</div>}
                {filteredBranches.map((b) => (
                  <button
                    key={b}
                    type="button"
                    className={`cc-combo-item${b === baseRef ? ' hl' : ''}`}
                    onMouseDown={(e) => e.preventDefault()}
                    onClick={() => {
                      setBaseRef(b);
                      setBranchListOpen(false);
                    }}
                  >
                    {b}{selectedProject.default_branch === b ? '（默认）' : ''}
                  </button>
                ))}
                <button
                  type="button"
                  className="cc-combo-foot"
                  onMouseDown={(e) => e.preventDefault()}
                  onClick={() => void refreshBranches()}
                >
                  <RetryIcon /> 刷新远端分支
                </button>
              </div>
            )}
            {branchesError && <div className="error-line cc-field-error">{branchesError}</div>}
          </div>
        )}

        {/* 任务名（data-od-id 对齐 design new-task-name，供 palette-focus 语义） */}
        <div className="od-field">
          <label className="od-label" htmlFor="cc-task-name">任务名</label>
          <input
            id="cc-task-name"
            ref={taskNameRef}
            className="od-input"
            data-od-id="new-task-name"
            placeholder="例如：重构 agent 通信"
            value={taskName}
            onChange={(e) => setTaskName(e.target.value)}
          />
        </div>

        {/* dir 警告 */}
        {isDir && (
          <div className="od-alert od-alert-warn cc-dir-warn">
            <WarnIcon />
            <div className="od-alert-body">纯目录项目无文件隔离：多个并行任务共享同一目录。</div>
          </div>
        )}

        {mutError && <div className="error-line">{mutError}</div>}
        {storeError && <div className="cc-store-hint"><InfoIcon /> 轮询数据可能滞后：{storeError}</div>}

        <p className="od-hint">创建后自动切出独立分支与 worktree，并进入工作台；任务名将生成英文分支 slug。</p>

        <div className="cc-form-actions">
          <button type="button" className="od-btn" onClick={onClose}>取消</button>
          <button type="submit" className="od-btn od-btn-primary" disabled={!canSubmit}>
            {creating ? '创建中…' : '创建并进入工作台'}
          </button>
        </div>
      </form>
    </div>
  );
}
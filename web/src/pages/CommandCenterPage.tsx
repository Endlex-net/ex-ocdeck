import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { api, ApiError } from '../api';
import { navigate } from '../router';
import { useProjects, useProjectsRefresh } from '../hooks';
import { subscribeActiveSessions } from '../sse';
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
import { classifyMatch } from '../fuzzy-match';
import { rankBranchOptions } from './command-center-branch-rank';
import {
  PALETTE_FOCUS_EVENT,
  clearPendingPaletteFocus,
  consumePendingPaletteFocus,
  readPaletteFocusDetail,
  type PaletteFocusPayload,
  type PaletteMatchMode,
} from '../palette-focus';
import './command-center.css';

/** tasks/active 快照（本页 SSE 订阅，帧驱动整表替换）。 */
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

/** 纯函数：finally 是否应释放本次刷新所有权（单飞锁 + refreshing 指示）。
 *  仅当记录的刷新项目与代际均属本次刷新时释放；否则所有权已转移给新刷新或已被项目切换 effect 重置，
 *  旧请求的 finally 不得代清（避免释放新项目的单飞锁允许并发刷新）。 */
export function shouldClearRefreshing(
  refreshingProjectId: string | null,
  refreshingGen: number,
  thisProjectId: string,
  thisGen: number,
): boolean {
  return refreshingProjectId === thisProjectId && refreshingGen === thisGen;
}

/**
 * 指挥中心 sessions 首屏状态机（纯函数，可测，design D5：SSE 订阅版）。
 * - loading：仅 projects 自身未初始化且无数据（sessions 未首帧不升级为整页 loading）
 * - connecting：sessions 首帧未到（无错误）——独立「连接中」，抑制全局与分区空态，projects 数据照常渲染
 * - error：首次连接失败（attempted && !initialized && error），不与 connecting/loading/空态并存
 * - empty：两侧均已成功初始化且三区皆空
 * - ready：其余（有数据或分区可渲染）
 */
export type SessionsBootstrapPhase = 'loading' | 'connecting' | 'error' | 'empty' | 'ready';

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

  // 首次连接失败：只报错（不 loading、不空态、不连接中）
  if (opts.sessionsAttempted && !opts.sessionsInitialized && opts.sessionsError) {
    return 'error';
  }

  // 整页 loading 仅由 projects 未初始化驱动
  if (!opts.projectsInit && !hasAnyData) {
    return 'loading';
  }

  // sessions 首帧未到：独立连接中（不升级整页 loading，抑制全局与分区空态）
  if (!opts.sessionsInitialized) {
    return 'connecting';
  }

  // 两侧均成功初始化且无任务 → 真空态
  if (
    opts.projectsInit &&
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
 * - loading / connecting / error：抑制「暂无…」空态占位
 * - empty / ready：允许分区空态占位
 * 注意：只门禁空态占位，非空列表 MUST 始终渲染（双快照：projects-only 任务仍须呈现）。
 */
export function shouldShowSectionEmpty(phase: SessionsBootstrapPhase): boolean {
  return phase === 'empty' || phase === 'ready';
}

/**
 * 分区主体渲染模式（纯函数）。
 * - list：有条目 → 始终渲染列表（sessions 连接中/失败不隐藏 projects 数据）
 * - empty：无条目且 phase 允许 → 「暂无…」
 * - none：无条目且 phase 为 loading/connecting/error → 不渲染占位（只留错误条/连接指示）
 */
export function sectionBodyMode(
  phase: SessionsBootstrapPhase,
  itemCount: number,
): 'list' | 'empty' | 'none' {
  if (itemCount > 0) return 'list';
  if (shouldShowSectionEmpty(phase)) return 'empty';
  return 'none';
}

export type NewTaskInitResult =
  | { action: 'keep' }
  | { action: 'apply'; selected: Project | null; projectQuery: string };

/** 按信号到达时的项目快照解析初始化意图；MUST NOT 在后续加载中重试。 */
export function resolveNewTaskInit(
  payload: PaletteFocusPayload,
  snapshot: readonly Project[],
  matchMode: PaletteMatchMode,
): NewTaskInitResult {
  if (!('projectName' in payload) || payload.projectName === undefined) return { action: 'keep' };
  const projectName = payload.projectName;
  if (payload.projectID) {
    const byID = snapshot.find((p) => p.id === payload.projectID);
    if (byID) return { action: 'apply', selected: byID, projectQuery: byID.name };
  }
  const exactHits = snapshot.filter((p) => classifyMatch(p.name, projectName)?.kind === 'exact');
  if (exactHits.length === 1) {
    return { action: 'apply', selected: exactHits[0], projectQuery: exactHits[0].name };
  }
  if (matchMode === 'exact-then-substring' && exactHits.length === 0) {
    // 缩写档位（acronym）MUST NOT 参与预选推断（tasks 4.8）：命中集合只认 exact/prefix/substring
    const subHits = snapshot.filter((p) => {
      const m = classifyMatch(p.name, projectName);
      return m !== null && m.kind !== 'acronym';
    });
    if (subHits.length === 1) {
      return { action: 'apply', selected: subHits[0], projectQuery: subHits[0].name };
    }
  }
  return { action: 'apply', selected: null, projectQuery: projectName };
}

export function CommandCenterPage({ matchMode = 'exact-then-substring' }: { matchMode?: PaletteMatchMode } = {}) {
  // 共享 projects store（design.md D4：侧栏、指挥中心同一数据源，MUST NOT 自行轮询 /projects）
  const { projects, initialized: projectsInit, error: storeError } = useProjects();
  const refresh = useProjectsRefresh();
  const projectsRef = useRef(projects);
  projectsRef.current = projects;
  const matchModeRef = useRef(matchMode);
  matchModeRef.current = matchMode;

  // 本页 tasks/active SSE 订阅（design D5）：mount 订阅 / unmount close；帧驱动整表替换
  const [snap, setSnap] = useState<SessionsSnapshot>(EMPTY);
  useEffect(() => {
    const sub = subscribeActiveSessions({
      onData: (sessions) =>
        setSnap({
          sessions,
          loading: false,
          initialized: true,
          attempted: true,
          error: '',
          lastSuccessAt: Date.now(),
        }),
      // 失败：attempted=true；initialized 仅首帧到达置 true。保留旧 sessions + lastSuccessAt；
      // error 独立展示（首连失败走 bootstrap error 相位，不与连接中并存）。
      onError: (error) =>
        setSnap((s) => ({
          sessions: s.sessions,
          loading: false,
          initialized: s.initialized,
          attempted: true,
          error,
          lastSuccessAt: s.lastSuccessAt,
        })),
    });
    return () => sub.close();
  }, []);

  const view = useMemo(
    () => buildCommandCenterView(projects, snap.sessions),
    [projects, snap.sessions],
  );

  const activeCount = view.active.length;
  const attentionCount = view.attention.length;
  // 连接状态指示：首帧前显示连接中/连接失败（首连失败相位独立展示，不并存）；首帧后显示相对更新时间
  const connStatus = !snap.initialized
    ? snap.error
      ? '连接失败'
      : '连接中…'
    : `${relTimeMs(snap.lastSuccessAt)}更新`;

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
  // 命令面板初始化意图：递增 nonce；无 payload 为 keep（只展开聚焦）。
  const [newTaskInit, setNewTaskInit] = useState<{ nonce: number; result: NewTaskInitResult }>({
    nonce: 0,
    result: { action: 'keep' },
  });

  const applyPaletteNewTask = (payload: PaletteFocusPayload) => {
    // 快照必须在信号到达（listener 同步执行）时读取：functional updater 可能被 React
    // 延迟到 render 阶段，那时 ref 已是同批次更新后的值，违反「只按到达时快照判定」。
    const result = resolveNewTaskInit(payload, projectsRef.current, matchModeRef.current);
    setNewTaskOpen(true);
    setNewTaskInit((s) => ({ nonce: s.nonce + 1, result }));
    clearPendingPaletteFocus('new-task-name');
  };

  const closeNewTask = () => {
    setNewTaskOpen(false);
    setNewTaskInit({ nonce: 0, result: { action: 'keep' } });
  };

  // 对齐 design 源 ocdeck-palette.js + command-center.html:328-330
  useEffect(() => {
    const pending = consumePendingPaletteFocus('new-task-name');
    if (pending !== null) applyPaletteNewTask(pending);
    const onPaletteFocus = (e: Event) => {
      const parsed = readPaletteFocusDetail((e as CustomEvent).detail);
      if (!parsed || parsed.id !== 'new-task-name') return;
      applyPaletteNewTask(parsed.payload);
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
            {activeCount} 个活跃任务 · {attentionCount} 个需要关注 · 自动更新
          </p>
        </div>
        <div className="od-page-actions">
          <span className="cc-poll-status">{connStatus}</span>
          <button
            className="od-btn od-btn-primary"
            onClick={() => {
              if (newTaskOpen) {
                closeNewTask();
                return;
              }
              setNewTaskInit({ nonce: 0, result: { action: 'keep' } });
              setNewTaskOpen(true);
            }}
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
          onClose={closeNewTask}
          initNonce={newTaskInit.nonce}
          initResult={newTaskInit.result}
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

      {/* 加载占位（projects 自身未初始化时，不与空态/分区同时出现） */}
      {initialLoading && (
        <div className="od-empty">
          <span className="spinner spinner-inline" aria-hidden /> 加载中…
        </div>
      )}

      {/* 连接中指示（sessions 首帧未到；projects 数据照常渲染，抑制空态占位） */}
      {bootstrap === 'connecting' && (
        <div className="od-empty">
          <span className="spinner spinner-inline" aria-hidden /> 连接中…
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
  // 等待人工（权限/提问待处理）时状态徽标变蓝；计数取摘要 attention_count，
  // sessions-only 任务摘要无计数（0）时以 1 兜底保证状态呈现。
  const humanPending =
    item.kinds.has(AttentionKind.PermissionPending) || item.kinds.has(AttentionKind.QuestionPending);
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
          <AgentStatusBadge agentStatus={t.agentStatus} attentionCount={humanPending ? Math.max(1, t.attention_count ?? 0) : 0} />
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
          <AgentStatusBadge agentStatus={m.agentStatus} attention={m.attention} />
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
 *  提交门禁：有效项目 ID + 非空任务名 + repo 分支列表 ready 才可 POST；偏离已选清除 ID；
 *  base_ref 仅 repo 且取过滤排序首项；在途防重复。 */
function NewTaskPanel({
  projects,
  refresh,
  storeError,
  onClose,
  initNonce = 0,
  initResult = { action: 'keep' },
}: {
  projects: Project[];
  refresh: () => Promise<void>;
  storeError: string;
  onClose: () => void;
  /** 命令面板触发展开后递增；每个新 nonce 应用项目状态并聚焦任务名。 */
  initNonce?: number;
  initResult?: NewTaskInitResult;
}) {
  const [projectQuery, setProjectQuery] = useState('');
  const [selectedProject, setSelectedProject] = useState<Project | null>(null);
  const [projListOpen, setProjListOpen] = useState(false);
  const [taskName, setTaskName] = useState('');
  const [baseRef, setBaseRef] = useState('');
  // D9 分支列表状态机：idle|loading|ready|error，与 lastSuccessfulBranches 正交。
  // 仅 ready 计算提交候选；loading/error 禁止提交；dir 项目无此状态机（恒 idle）。
  const [branchPhase, setBranchPhase] = useState<'idle' | 'loading' | 'ready' | 'error'>('idle');
  const [lastSuccessfulBranches, setLastSuccessfulBranches] = useState<string[]>([]);
  const [branchesError, setBranchesError] = useState('');
  const [branchListOpen, setBranchListOpen] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [creating, setCreating] = useState(false);
  const [mutError, setMutError] = useState('');
  const projInputId = useId();
  const branchInputId = useId();
  const taskNameRef = useRef<HTMLInputElement>(null);
  const focusedNonceRef = useRef<number | null>(null);
  // 代际防陈旧写回 + 最新选择 ref（异步闭包读最新值而非闭包捕获值）
  const projGenRef = useRef(0);
  const selectedProjectRef = useRef<Project | null>(null);
  const refreshInFlightRef = useRef(false);
  // 刷新所有权（项目 ID + 代际）：finally 仅在项目与代际均匹配时释放单飞锁与 refreshing，
  // 避免跨项目竞态下旧请求的 finally 释放新刷新的单飞锁（允许并发刷新）
  const refreshingProjectIdRef = useRef<string | null>(null);
  const refreshingGenRef = useRef(0);

  const consumeFocus = (nonce: number) => {
    if (focusedNonceRef.current === nonce) return;
    taskNameRef.current?.focus();
    focusedNonceRef.current = nonce;
  };

  // 仅随 nonce 应用：快速新建替换项目相关状态、MUST 保留 taskName；keep 不改表单。
  // apply 分支只落状态，聚焦交由下方消费 effect；keep 与首次挂载在此直接消费——
  // 两处重复消费被 focusedNonceRef 幂等吸收。
  useLayoutEffect(() => {
    if (initNonce > 0 && initResult.action === 'apply') {
      setSelectedProject(initResult.selected);
      setProjectQuery(initResult.projectQuery);
      return;
    }
    consumeFocus(initNonce);
  }, [initNonce]);

  // 每个 nonce 恰好聚焦一次；不绑定 branches。nonce 直接驱动消费（相同 payload 的
  // apply 因同值 setState 被 React 跳过时，仍随 nonce 变化聚焦）；手动改项目时 nonce
  // 已消费，不抢焦。
  useLayoutEffect(() => {
    consumeFocus(initNonce);
  }, [initNonce, selectedProject, projectQuery]);

  const isDir = selectedProject?.kind === 'dir';
  const filteredProjects = useMemo(() => {
    const q = projectQuery.trim().toLowerCase();
    return q ? projects.filter((p) => p.name.toLowerCase().includes(q)) : projects;
  }, [projects, projectQuery]);

  // 选中项目时加载分支（repo 项目）：loading + 初次 GET；成功（含空数组）→ ready 并写入
  // lastSuccessfulBranches；失败 → error（无历史则列表为空）。
  // 任何选择变化（含切到 dir/清空）都推进代际，使此前未完成的 listBranches/refreshBranches 失效。
  useEffect(() => {
    // 推进代际（清空/切到 dir 也推进，使在途异步响应失效）
    ++projGenRef.current;
    selectedProjectRef.current = selectedProject;
    // 切换项目时重置刷新状态（与旧项目解耦，避免 B 永久处于刷新中，并释放旧刷新所有权）
    setRefreshing(false);
    refreshInFlightRef.current = false;
    refreshingProjectIdRef.current = null;
    refreshingGenRef.current = 0;
    if (!selectedProject || selectedProject.kind === 'dir') {
      // 切走 repo → idle 并清空最近成功列表
      setBranchPhase('idle');
      setLastSuccessfulBranches([]);
      setBaseRef('');
      return;
    }
    setBaseRef(selectedProject.default_branch);
    setBranchesError('');
    setBranchPhase('loading');
    setLastSuccessfulBranches([]);
    const gen = projGenRef.current;
    api
      .listBranches(selectedProject.id)
      .then((bs) => {
        if (shouldAcceptBranchResult(gen, projGenRef.current, selectedProject.id, selectedProjectRef.current?.id ?? null)) {
          setLastSuccessfulBranches(bs);
          setBranchPhase('ready');
        }
      })
      .catch((err) => {
        if (shouldAcceptBranchResult(gen, projGenRef.current, selectedProject.id, selectedProjectRef.current?.id ?? null)) {
          setBranchesError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '获取分支列表失败');
          setBranchPhase('error');
        }
      });
  }, [selectedProject]);

  const refreshBranches = async () => {
    if (!selectedProject || isDir || refreshInFlightRef.current) return;
    refreshInFlightRef.current = true;
    // 推进代际并捕获本次刷新所属项目 ID：刷新项目 A 后切到 B，A 的结果不得覆盖 B
    const gen = ++projGenRef.current;
    const projectId = selectedProject.id;
    refreshingProjectIdRef.current = projectId;
    refreshingGenRef.current = gen;
    setRefreshing(true);
    setBranchesError('');
    // refresh 在途进入 loading 禁止提交；不清空 lastSuccessfulBranches（保留 stale 展示）
    setBranchPhase('loading');
    try {
      const bs = await api.refreshBranches(projectId);
      // 校验代际未变且当前选择仍是同一项目（ref 读最新值，不用闭包捕获值）
      if (shouldAcceptBranchResult(gen, projGenRef.current, projectId, selectedProjectRef.current?.id ?? null)) {
        setLastSuccessfulBranches(bs);
        setBranchPhase('ready');
      }
    } catch (err) {
      if (shouldAcceptBranchResult(gen, projGenRef.current, projectId, selectedProjectRef.current?.id ?? null)) {
        setBranchesError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '刷新远端分支失败');
        // 保留最近一次 ready 数据作 stale 展示并标注「本地快照未刷新」；stale 不参与提交
        setBranchPhase('error');
      }
    } finally {
      // 仅当本次刷新仍持有刷新所有权（项目与代际均匹配）时释放单飞锁与刷新指示；
      // 否则所有权已转移新刷新或已被项目切换 effect 重置，不得代清
      if (shouldClearRefreshing(refreshingProjectIdRef.current, refreshingGenRef.current, projectId, gen)) {
        refreshInFlightRef.current = false;
        refreshingProjectIdRef.current = null;
        refreshingGenRef.current = 0;
        setRefreshing(false);
      }
    }
  };

  // 偏离已选项目：用户继续编辑项目输入导致 query 不等于已选项目名 → 清除 ID
  const onProjectQueryChange = (v: string) => {
    setProjectQuery(v);
    if (selectedProject && v !== selectedProject.name) setSelectedProject(null);
  };

  // stale 展示列表：loading/error 时保留最近成功列表仅供下拉展示（初次加载/失败为空），不参与提交
  const staleBranches = useMemo(() => {
    const q = baseRef.trim().toLowerCase();
    return q ? lastSuccessfulBranches.filter((b) => b.toLowerCase().includes(q)) : lastSuccessfulBranches;
  }, [lastSuccessfulBranches, baseRef]);

  // 提交候选（D2/D9）：仅 ready 计算——成功空列表回退 default_branch（loading/error 不得回退）；
  // normalizedInput 非空且不在基础候选（大小写敏感 includes）时前置 synthetic；过滤后按 D2 排序。
  // stale MUST NOT 进入本列表（提交唯一来源 base_ref = filteredBranches[0]）。
  const filteredBranches = useMemo(() => {
    if (branchPhase !== 'ready') return [];
    const normalizedInput = baseRef.trim();
    const base = lastSuccessfulBranches.length
      ? lastSuccessfulBranches
      : selectedProject?.default_branch
        ? [selectedProject.default_branch]
        : [];
    const candidates =
      normalizedInput && !base.includes(normalizedInput) ? [normalizedInput, ...base] : base;
    const q = normalizedInput.toLowerCase();
    const filtered = q ? candidates.filter((b) => b.toLowerCase().includes(q)) : candidates;
    return rankBranchOptions(filtered, q);
  }, [branchPhase, lastSuccessfulBranches, selectedProject, baseRef]);
  // 下拉展示项：ready 用提交候选；loading/error 展示 stale
  const branchListItems = branchPhase === 'ready' ? filteredBranches : staleBranches;

  // 门禁（D3/D9）：repo 另须分支列表 ready；loading/error（含 refresh 在途）禁止提交
  const canSubmit = !!selectedProject && taskName.trim() !== '' && !creating && (isDir || branchPhase === 'ready');

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!canSubmit || !selectedProject) return;
    setCreating(true);
    setMutError('');
    const proj = selectedProject;
    try {
      const t = await api.createTask(proj.id, taskName.trim(), isDir ? undefined : filteredBranches[0] || undefined);
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
                {branchListItems.map((b) => (
                  <button
                    key={b}
                    type="button"
                    // 高亮过滤排序首项（与提交值 filteredBranches[0] 一致）；列表为空时无高亮
                    className={`cc-combo-item${b === branchListItems[0] ? ' hl' : ''}`}
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
            {branchPhase === 'error' && lastSuccessfulBranches.length > 0 && (
              <div className="error-line cc-field-error">本地快照未刷新</div>
            )}
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
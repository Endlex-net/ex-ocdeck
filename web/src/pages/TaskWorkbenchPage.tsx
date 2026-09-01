import { useEffect, useMemo, useRef, useState } from 'react';
import { api, ApiError } from '../api';
import { navigate } from '../router';
import { resolveBackHref, type FromSource } from '../router';
import { useMediaQuery, useProjects, useProjectsRefresh } from '../hooks';
import { debugMark } from '../debug';
import { subscribeTask } from '../sse';
import { isTransitional, initActivateBlockReason, parseNotice, type Task } from '../types';
import { shouldCloseOverflowOnBlur } from './workbench-overflow';
import { StatusBadge } from '../components/StatusBadge';
import { TaskActions } from '../components/TaskActions';
import { DeleteTaskModal } from '../components/DeleteTaskModal';
import { GitPanel } from '../components/GitPanel';
import { EnvEditor } from '../components/EnvEditor';
import { AgentStatusBadge } from '../components/AgentStatusBadge';
import { InitStatusBadge } from '../components/InitStatusBadge';
import { LifecycleLogModal } from '../components/LifecycleLogModal';
import { RerunInitButton } from '../components/RerunInitButton';
import { TerminalView } from '../terminal/TerminalView';
import { BranchIcon, CaretDownIcon, MoreIcon, WarnIcon, InfoIcon } from '../icons';

const TUI_TAB = 'tui';
const GIT_TAB = 'git';
const SETTINGS_TAB = 'settings';

/** 页头「⋯」溢出菜单（design task-workbench.html wb-overflow）：删除等次级操作。
 *  桌面与窄屏同一入口；窄屏主操作图标化由 TaskActions compact 承担。
 *  普通 disclosure 模式（非 ARIA menu）：打开聚焦首个可用项、Escape 关闭并恢复焦点、
 *  焦点真实移出菜单即关闭（见 shouldCloseOverflowOnBlur）——菜单项少，disclosure 更轻量且满足可访问性。 */
function WorkbenchOverflow({
  task,
  onShowInitLog,
  onDelete,
}: {
  task: Task;
  onShowInitLog: () => void;
  onDelete: () => void;
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  const menuTriggerRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);

  // disclosure 模式：打开后焦点进入菜单内首个可用项
  useEffect(() => {
    if (menuOpen) {
      menuRef.current?.querySelector<HTMLButtonElement>('button:not(:disabled)')?.focus();
    }
  }, [menuOpen]);

  // 关闭溢出菜单；键盘取消（Escape）/失焦兜底后焦点恢复触发器
  const closeMenu = (restoreFocus: boolean) => {
    setMenuOpen(false);
    if (restoreFocus) menuTriggerRef.current?.focus();
  };

  return (
    <span
      className="header-overflow"
      onBlur={(e) => {
        // React onBlur 冒泡：焦点真实落到溢出区之外才关闭（触屏/Safari 的
        // relatedTarget=null 失焦不关，否则菜单项随菜单卸载导致点击丢失，
        // 由全屏 backdrop 的 click 兜底；详见 shouldCloseOverflowOnBlur）。
        const next = e.relatedTarget as Node | null;
        if (shouldCloseOverflowOnBlur(next, (n) => e.currentTarget.contains(n))) closeMenu(false);
      }}
    >
      <button
        ref={menuTriggerRef}
        className="btn btn-small btn-ghost"
        aria-label="更多操作"
        aria-expanded={menuOpen}
        aria-controls="workbench-overflow-menu"
        onClick={() => setMenuOpen((o) => !o)}
        onKeyDown={(e) => {
          // 菜单开着但焦点仍在触发器（如菜单项全 disabled 未移焦）时 Escape 也可关闭
          if (e.key === 'Escape' && menuOpen) {
            e.stopPropagation();
            closeMenu(false);
          }
        }}
      >
        <MoreIcon />
      </button>
      {menuOpen && (
        <>
          <div className="overflow-backdrop" onClick={() => closeMenu(true)} />
          <div
            ref={menuRef}
            id="workbench-overflow-menu"
            className="overflow-menu"
            onKeyDown={(e) => {
              if (e.key === 'Escape') {
                e.stopPropagation();
                closeMenu(true);
              }
            }}
          >
            {task.init_status === 'failed' && (
              <button
                className="overflow-item"
                onClick={() => {
                  setMenuOpen(false);
                  onShowInitLog();
                }}
              >
                查看 init 日志
              </button>
            )}
            {/* 活跃态不出现删除（design D9） */}
            {task.status !== 'active' && (
              <button
                className="overflow-item overflow-item-danger"
                disabled={isTransitional(task.status)}
                onClick={() => {
                  setMenuOpen(false);
                  onDelete();
                }}
              >
                删除任务
              </button>
            )}
          </div>
        </>
      )}
    </span>
  );
}

export function TaskWorkbenchPage({
  taskID,
  from = 'home',
}: {
  taskID: string;
  /** 来源感知返回链接（归一后）：home → #/，projects → #/projects#<projectID>。
   *  legacy fromActive 已废弃（P8 路由归一：active → home）。 */
  from?: FromSource;
}) {
  const [task, setTask] = useState<Task | null>(null);
  const [shells, setShells] = useState<string[]>([]);
  // 区分"无 shell 终端"与"列表获取失败"：失败时给出错误提示并可重试
  const [shellsError, setShellsError] = useState('');
  const [tab, setTab] = useState<string>(TUI_TAB);
  const [error, setError] = useState('');
  const [notFound, setNotFound] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [addingShell, setAddingShell] = useState(false);
  // 生命周期日志弹窗（init 日志入口始终可用；pre-delete 日志仅 pre-delete: 失败时）
  const [logView, setLogView] = useState<'init' | 'pre-delete' | null>(null);
  // 功能面板首次访问后才挂载，挂载后常驻以保留面板内部状态
  const [visited, setVisited] = useState<Set<string>>(new Set([TUI_TAB]));
  // 窄屏（≤1024px）结构性切换：header 主操作图标化 + 次级操作收「⋯」溢出菜单（design D3）
  const isNarrow = useMediaQuery('(max-width: 1024px)');
  // ≤767px 侧栏任务组隐藏，页头任务切换器承担任务直达（design D12 裁决 3）
  const isMobile = useMediaQuery('(max-width: 767px)');
  const [switcherOpen, setSwitcherOpen] = useState(false);
  // 共享 store refresh：任务操作成功后同步侧栏/指挥中心（tasks 8.8 refresh 审计）
  const refreshShared = useProjectsRefresh();
  // 页头任务切换器数据源：与侧栏同一共享 store（design D8/D12）
  const { projects } = useProjects();

  const switchTab = (t: string) => {
    setTab(t);
    setVisited((v) => (v.has(t) ? v : new Set(v).add(t)));
  };

  useEffect(() => {
    const sub = subscribeTask(taskID, {
      onData: (next) => {
        setTask(next);
        setError('');
      },
      onError: setError,
      onGone: () => setNotFound(true),
    });
    return () => sub.close();
  }, [taskID]);

  /** 任务操作成功后的统一回调：共享 store 同步（侧栏/指挥中心，tasks 8.8）。
   *  本任务详情由流推送承接；TaskActions/RerunInitButton 仅在成功时调 onDone。 */
  const onTaskActionDone = () => {
    void refreshShared().catch(() => {});
  };

  // shell 终端列表只需加载一次（新建/关闭由本页操作驱动）
  const loadShells = async () => {
    try {
      const ts = await api.listTerminals(taskID);
      setShells(ts.map((t) => t.terminal_id));
      setShellsError('');
    } catch (err) {
      setShellsError(
        err instanceof ApiError ? `[${err.code}] ${err.message}` : '获取终端列表失败',
      );
    }
  };
  useEffect(() => {
    void loadShells();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [taskID]);

  const addShell = async () => {
    if (addingShell) return;
    setAddingShell(true);
    try {
      const t = await api.createTerminal(taskID);
      setShells((s) => [...s, t.terminal_id]);
      setTab(t.terminal_id);
    } catch (err) {
      setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '新建终端失败');
    } finally {
      setAddingShell(false);
    }
  };

  const closeShell = async (tid: string) => {
    try {
      await api.closeTerminal(tid);
    } catch (err) {
      // 关闭失败：保留 tab（可重试），提示错误
      setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '关闭终端失败');
      return;
    }
    setError('');
    setShells((s) => s.filter((x) => x !== tid));
    if (tab === tid) setTab(TUI_TAB);
  };

  // dir 任务无 git 功能（D7）：隐藏 Git tab 与分支名，依据 project_kind 而非判空推断
  const isDir = task?.project_kind === 'dir';
  // kind 解析为 dir 时若正停在 Git tab，回退到 TUI tab
  useEffect(() => {
    if (isDir && tab === GIT_TAB) setTab(TUI_TAB);
  }, [isDir, tab]);

  // 页头任务切换器候选：与侧栏任务组同源（共享 store），仅活跃+挂起任务（归档不显示）。
  // data-od-id / wb-* class 对齐设计稿 task-workbench.html:229
  // 注意：以下 Hook 必须在任何条件 return 之前（404 分支不得改变 Hook 调用次数）。
  const switcherTasks = useMemo(() => {
    const out: Array<{ taskID: string; name: string; branch: string; projectName: string; agentStatus?: string; attentionCount: number; current: boolean }> = [];
    for (const p of projects) {
      // 与侧栏 SidebarTaskGroups 同源：仅 active+suspended（归档/失败等不显示）
      for (const t of (p.tasks ?? []).filter((x) => x.status === 'active' || x.status === 'suspended')) {
        out.push({
          taskID: t.id,
          name: t.name,
          branch: t.branch,
          projectName: p.name,
          agentStatus: t.agentStatus,
          attentionCount: t.attention_count ?? 0,
          current: t.id === taskID,
        });
      }
    }
    return out;
  }, [projects, taskID]);
  // 切换器按项目分组
  const switcherGroups = useMemo(() => {
    const m = new Map<string, typeof switcherTasks>();
    for (const t of switcherTasks) {
      const arr = m.get(t.projectName) ?? [];
      arr.push(t);
      m.set(t.projectName, arr);
    }
    return Array.from(m.entries());
  }, [switcherTasks]);
  const switcherRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!switcherOpen) return;
    const onDocDown = (e: MouseEvent) => {
      if (switcherRef.current && !switcherRef.current.contains(e.target as Node)) setSwitcherOpen(false);
    };
    document.addEventListener('mousedown', onDocDown);
    return () => document.removeEventListener('mousedown', onDocDown);
  }, [switcherOpen]);

  useEffect(() => {
    if (!task) return;
    debugMark('odterm:task-render');
  }, [task]);

  if (notFound) {
    // fullbleed 工作台路由无外层 padding；用自持边距容器（非已删 .page）
    return (
      <div className="wb-not-found">
        <div className="empty">
          任务不存在或已删除。{' '}
          <button className="btn btn-small" onClick={() => navigate('/')}>
            返回指挥中心
          </button>
        </div>
      </div>
    );
  }

  if (task === null) {
    return (
      <div className="wb-not-found">
        <div className="empty">连接中…</div>
        {error && <div className="alert-bar alert-error mono">{error}</div>}
      </div>
    );
  }

  const notices = parseNotice(task?.notice);
  const status = task?.status ?? '';
  // single-process（tasks 5.3）：原「TUI 可重开」标记语义改为「进程在不在」——
  // 任务 active 即任务进程在（TUI 与进程同体），非 active 即进程不在。
  const processReady = status === 'active';
  // init 门禁原因（空串 = 可激活）；失败展示以 init_error 为权威信息，日志仅辅助
  const initBlock = task ? initActivateBlockReason(task) : '';
  // pre-delete 失败以 last_error 的 `pre-delete:` 前缀稳定识别（tasks.md 5.3）
  const preDeleteFailed =
    status === 'deletion_failed' && (task?.last_error ?? '').startsWith('pre-delete:');

  return (
    <div className={`workbench${isNarrow ? ' workbench-narrow' : ''}`}>
      <header className="page-header">
        {/* 来源感知返回（design.md D3 归一映射）：home → #/，projects → #/projects#<projectID>。
            窄屏同样保留返回入口；任务未加载时回退 home。 */}
        <button
          className="btn btn-small btn-ghost"
          onClick={() => navigate(resolveBackHref(from, task?.project_id))}
        >
          ← {from === 'projects' ? '任务列表' : '指挥中心'}
        </button>
        {/* 页头任务切换器：≤767px 侧栏任务组隐藏时的任务直达入口（design D12 裁决 3）。
            切换执行 navigate(#/task/:id) + key={taskID} 重挂载，数据来自共享 store。 */}
        {isMobile ? (
          <div className="wb-switcher" ref={switcherRef}>
            <button
              className="wb-switcher-btn"
              aria-haspopup="true"
              aria-expanded={switcherOpen}
              title="切换任务"
              onClick={() => setSwitcherOpen((o) => !o)}
            >
              <span className="wb-switcher-title">{task?.name ?? '…'}</span>
              <CaretDownIcon />
            </button>
            {switcherOpen && (
              <div className="wb-switcher-menu" role="menu">
                {switcherGroups.map(([projName, items]) => (
                  <div key={projName}>
                    <div className="wb-sw-group">{projName}</div>
                    {items.map((t) => (
                      <button
                        key={t.taskID}
                        className={`wb-sw-item${t.current ? ' current' : ''}`}
                        role="menuitem"
                        onClick={() => {
                          setSwitcherOpen(false);
                          navigate(`/task/${t.taskID}`);
                        }}
                      >
                        {/* 有待答问题/待授权限时蓝点（等待人工 > 运行态） */}
                        <span
                          className={`od-agent od-agent-${t.attentionCount > 0 ? 'attention' : (t.agentStatus ?? '')}`}
                          title={t.attentionCount > 0 ? `等待人工处理：${t.attentionCount} 个待处理请求` : undefined}
                        ><span className="od-agent-dot" /></span>
                        <span className="wb-sw-name">{t.name}</span>
                        <span className="mono">{t.branch}</span>
                      </button>
                    ))}
                  </div>
                ))}
                <a
                  className="wb-sw-all"
                  href="#/"
                  onClick={(e) => {
                    e.preventDefault();
                    setSwitcherOpen(false);
                    navigate('/');
                  }}
                >
                  查看全部任务 →
                </a>
              </div>
            )}
          </div>
        ) : (
          <span className="page-title">{task?.name ?? '…'}</span>
        )}
        {task?.branch && !isDir && <span className="header-meta mono"><BranchIcon /> {task.branch}</span>}
        {task && <StatusBadge status={task.status} />}
        {task && <InitStatusBadge task={task} />}
        {task?.init_status === 'failed' && !isNarrow && (
          <button
            className="btn btn-small btn-ghost"
            title={task.init_error || 'init 失败'}
            onClick={() => setLogView('init')}
          >
            日志
          </button>
        )}
        {task && <AgentStatusBadge agentStatus={task.agentStatus} attention={task.attention} />}
        <span className="header-spacer" />
        {error && !isNarrow && <span className="header-error">{error}</span>}
        {task && !isNarrow && (
          <>
            {/* 主操作按状态机呈现（actionsFor：活跃=挂起；挂起=激活/归档；归档=恢复；失败=重试） */}
            <TaskActions task={task} onDone={onTaskActionDone} onError={setError} />
            {/* 「⋯」溢出菜单：删除等次级操作（对齐设计稿 task-workbench.html 页头 wb-overflow） */}
            <WorkbenchOverflow
              key="wide"
              task={task}
              onShowInitLog={() => setLogView('init')}
              onDelete={() => setDeleting(true)}
            />
          </>
        )}
        {/* 窄屏（design D3）：主操作图标化保留在 header，次级操作（init 日志/删除）收同一「⋯」溢出菜单。 */}
        {task && isNarrow && (
          <>
            <TaskActions task={task} onDone={onTaskActionDone} onError={setError} compact />
            <WorkbenchOverflow
              key="narrow"
              task={task}
              onShowInitLog={() => setLogView('init')}
              onDelete={() => setDeleting(true)}
            />
          </>
        )}
      </header>

      {/* 窄屏：操作错误从 header 移出为整宽提示条，避免挤占标题/操作空间 */}
      {isNarrow && error && <div className="alert-bar alert-error mono">{error}</div>}

      {task?.last_error && (
        <div className="alert-bar alert-error mono" title={task.last_error}>
          <WarnIcon /> {task.last_error}
          {preDeleteFailed && (
            <button className="btn btn-small" onClick={() => setLogView('pre-delete')}>
              查看 pre-delete 日志
            </button>
          )}
        </div>
      )}
      {task?.init_status === 'failed' && (
        <div className="alert-bar alert-error">
          <span className="mono" title={task.init_error}>
            <WarnIcon /> init 失败：{task.init_error || '（无错误信息）'}
          </span>
          <button className="btn btn-small" onClick={() => setLogView('init')}>
            查看日志
          </button>
          <RerunInitButton task={task} onDone={onTaskActionDone} onError={setError} />
        </div>
      )}
      {notices.length > 0 && (
        <div className="alert-bar alert-notice">
          {notices.slice(-3).map((n, i) => (
            <span key={i} className="mono" title={new Date(n.ts * 1000).toLocaleString()}>
              <InfoIcon /> [{n.code}] {n.message}
            </span>
          ))}
        </div>
      )}

      <div className="tabstrip">
        <button
          className={`tab ${tab === TUI_TAB ? 'tab-active' : ''}`}
          onClick={() => switchTab(TUI_TAB)}
        >
          终端
        </button>
        {shells.map((tid, i) => (
          <span key={tid} className={`tab ${tab === tid ? 'tab-active' : ''}`}>
            <button className="tab-label" onClick={() => switchTab(tid)}>
              shell {i + 1}
            </button>
            <button
              className="tab-close"
              title="关闭终端"
              onClick={() => void closeShell(tid)}
            >
              ×
            </button>
          </span>
        ))}
        <button
          className="tab tab-add"
          title="新建 shell 终端"
          disabled={addingShell}
          onClick={() => void addShell()}
        >
          +
        </button>
        <span className="tab-sep" />
        {!isDir && (
          <button
            className={`tab ${tab === GIT_TAB ? 'tab-active' : ''}`}
            onClick={() => switchTab(GIT_TAB)}
          >
            Git
          </button>
        )}
        <button
          className={`tab ${tab === SETTINGS_TAB ? 'tab-active' : ''}`}
          onClick={() => switchTab(SETTINGS_TAB)}
        >
          设置
        </button>
      </div>

      {shellsError && (
        <div className="alert-bar alert-error">
          <span className="mono">获取 shell 终端列表失败：{shellsError}</span>
          <button className="btn btn-small" onClick={() => void loadShells()}>
            重试
          </button>
        </div>
      )}

      <div className="pane-area">
        {/* TUI 终端：常驻挂载，隐藏不断开由 active 控制 */}
        <div className={`pane ${tab === TUI_TAB ? '' : 'pane-hidden'}`}>
          <TerminalView
            wsPath={`/ws/terminal/${taskID}`}
            active={tab === TUI_TAB && processReady}
          />
          {tab === TUI_TAB && task && !processReady && (
            <div className="terminal-overlay">
              <div className="terminal-overlay-box">
                {isTransitional(status) ? (
                  <>
                    <span className="spinner" aria-hidden />
                    {/* spec（终端重开与恢复中语义）：恢复期统一显示「进程启动中」，
                        不新增原因字段——activating 同时覆盖用户激活与自动重拉。 */}
                    <span>{status === 'activating' ? '进程启动中' : '任务状态变更中'}…</span>
                  </>
                ) : status === 'suspended' ? (
                  <>
                    <span>任务已挂起，激活后可接入任务终端</span>
                    {initBlock && <span className="terminal-overlay-reason">{initBlock}</span>}
                    <span className="terminal-overlay-actions">
                      <ActivateButton task={task} onDone={onTaskActionDone} onError={setError} />
                      <RerunInitButton
                        task={task}
                        onDone={onTaskActionDone}
                        onError={setError}
                      />
                    </span>
                  </>
                ) : (
                  <span>任务当前状态（{status}）不可用终端</span>
                )}
              </div>
            </div>
          )}
        </div>
        {shells.map((tid) => (
          <div key={tid} className={`pane ${tab === tid ? '' : 'pane-hidden'}`}>
            <TerminalView wsPath={`/ws/terminal/shell/${tid}`} active={tab === tid} />
          </div>
        ))}
        {visited.has(GIT_TAB) && !isDir && (
          <div className={`pane pane-scroll ${tab === GIT_TAB ? '' : 'pane-hidden'}`}>
            <GitPanel taskID={taskID} active={tab === GIT_TAB} />
          </div>
        )}
        {visited.has(SETTINGS_TAB) && (
          <div className={`pane pane-scroll ${tab === SETTINGS_TAB ? '' : 'pane-hidden'}`}>
            <div className="settings-pane">
              <div className="settings-section">
                <div className="settings-title">任务级环境变量</div>
                <EnvEditor base={`/tasks/${taskID}/env`} />
              </div>
              <div className="settings-section">
                <div className="settings-title">生命周期日志</div>
                <div className="lc-log-entries">
                  <button className="btn btn-small" onClick={() => setLogView('init')}>
                    查看 init 日志
                  </button>
                  {preDeleteFailed && (
                    <button className="btn btn-small" onClick={() => setLogView('pre-delete')}>
                      查看 pre-delete 日志
                    </button>
                  )}
                </div>
              </div>
            </div>
          </div>
        )}
      </div>

      {logView && task && (
        <LifecycleLogModal
          title={logView === 'init' ? 'init 日志' : 'pre-delete 日志'}
          fetchLog={() =>
            logView === 'init' ? api.getInitLog(task.id) : api.getPreDeleteLog(task.id)
          }
          onClose={() => setLogView(null)}
        />
      )}

      {deleting && task && (
        <DeleteTaskModal
          task={task}
          onClose={() => setDeleting(false)}
          onDeleted={() => {
            // 删除成功：触发共享 store refresh（侧栏/指挥中心同步）再导航离开（tasks 8.8）。
            void refreshShared().catch(() => {});
            navigate(resolveBackHref(from, task.project_id));
          }}
        />
      )}
    </div>
  );
}

function ActivateButton({
  task,
  onDone,
  onError,
}: {
  task: Task;
  onDone: () => void;
  onError: (m: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  // init 非 none|succeeded 时禁用并提示原因（tasks.md 5.3：覆盖 Workbench 内联激活入口）
  const block = initActivateBlockReason(task);
  return (
    <button
      className="btn btn-primary btn-small"
      disabled={busy || block !== ''}
      title={block || undefined}
      onClick={async () => {
        setBusy(true);
        try {
          await api.taskAction(task.id, 'activate');
          onDone();
        } catch (err) {
          // 失败仅走 onError（不调 onDone/refresh）；状态由 SSE 推送收敛（与 TaskActions 语义一致，P3 修复 7）。
          onError(err instanceof ApiError ? err.message : '激活失败');
        } finally {
          setBusy(false);
        }
      }}
    >
      {busy ? '激活中…' : '激活任务'}
    </button>
  );
}

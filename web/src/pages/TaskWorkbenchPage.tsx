import { useEffect, useState } from 'react';
import { api, ApiError } from '../api';
import { navigate } from '../router';
import { usePoll } from '../hooks';
import { isTransitional, initActivateBlockReason, parseNotice, type Task } from '../types';
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

const TUI_TAB = 'tui';
const GIT_TAB = 'git';
const SETTINGS_TAB = 'settings';

export function TaskWorkbenchPage({
  taskID,
  fromActive = false,
}: {
  taskID: string;
  /** 从活跃会话页（#/active）进入时为 true：返回链接指回活跃列表。 */
  fromActive?: boolean;
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

  const switchTab = (t: string) => {
    setTab(t);
    setVisited((v) => (v.has(t) ? v : new Set(v).add(t)));
  };

  const load = async () => {
    try {
      setTask(await api.getTask(taskID));
      setError('');
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) setNotFound(true);
      else setError(err instanceof ApiError ? err.message : '加载失败');
    }
  };
  // init_status pending|running 视为活跃（tasks.md 5.3：轮询条件不只看 task.status）
  const initActive = task?.init_status === 'pending' || task?.init_status === 'running';
  usePoll(
    () => void load(),
    (task && isTransitional(task.status)) || initActive ? 2000 : 4000,
    [task?.status, task?.init_status],
  );

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

  if (notFound) {
    return (
      <div className="page">
        <div className="empty">
          任务不存在或已删除。{' '}
          <button className="btn btn-small" onClick={() => navigate('/')}>
            返回项目列表
          </button>
        </div>
      </div>
    );
  }

  const notices = parseNotice(task?.notice);
  const status = task?.status ?? '';
  const tuiReady = status === 'active';
  // init 门禁原因（空串 = 可激活）；失败展示以 init_error 为权威信息，日志仅辅助
  const initBlock = task ? initActivateBlockReason(task) : '';
  // pre-delete 失败以 last_error 的 `pre-delete:` 前缀稳定识别（tasks.md 5.3）
  const preDeleteFailed =
    status === 'deletion_failed' && (task?.last_error ?? '').startsWith('pre-delete:');

  return (
    <div className="workbench">
      <header className="page-header">
        {/* 来源感知返回：从活跃列表进入则指回活跃列表，否则回到任务列表 */}
        {fromActive ? (
          <button className="btn btn-small btn-ghost" onClick={() => navigate('/active')}>
            ← 活跃会话
          </button>
        ) : (
          <button
            className="btn btn-small btn-ghost"
            onClick={() => task && navigate(`/project/${task.project_id}`)}
          >
            ← 任务列表
          </button>
        )}
        <span className="page-title">{task?.name ?? '…'}</span>
        {task?.branch && !isDir && <span className="header-meta mono">⎇ {task.branch}</span>}
        {task && <StatusBadge status={task.status} />}
        {task && <InitStatusBadge task={task} />}
        {task?.init_status === 'failed' && (
          <button
            className="btn btn-small btn-ghost"
            title={task.init_error || 'init 失败'}
            onClick={() => setLogView('init')}
          >
            日志
          </button>
        )}
        {task && <AgentStatusBadge agentStatus={task.agentStatus} />}
        <span className="header-spacer" />
        {error && <span className="header-error">{error}</span>}
        {task && (
          <>
            <TaskActions task={task} onDone={() => void load()} onError={setError} />
            <button
              className="btn btn-small btn-ghost"
              disabled={isTransitional(task.status)}
              onClick={() => setDeleting(true)}
            >
              删除
            </button>
          </>
        )}
      </header>

      {task?.last_error && (
        <div className="alert-bar alert-error mono" title={task.last_error}>
          ⚠ {task.last_error}
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
            ⚠ init 失败：{task.init_error || '（无错误信息）'}
          </span>
          <button className="btn btn-small" onClick={() => setLogView('init')}>
            查看日志
          </button>
          <RerunInitButton task={task} onDone={() => void load()} onError={setError} />
        </div>
      )}
      {notices.length > 0 && (
        <div className="alert-bar alert-notice">
          {notices.slice(-3).map((n, i) => (
            <span key={i} className="mono" title={new Date(n.ts * 1000).toLocaleString()}>
              ⓘ [{n.code}] {n.message}
            </span>
          ))}
        </div>
      )}

      <div className="tabstrip">
        <button
          className={`tab ${tab === TUI_TAB ? 'tab-active' : ''}`}
          onClick={() => switchTab(TUI_TAB)}
        >
          TUI
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
            active={tab === TUI_TAB && tuiReady}
          />
          {tab === TUI_TAB && task && !tuiReady && (
            <div className="terminal-overlay">
              <div className="terminal-overlay-box">
                {isTransitional(status) ? (
                  <>
                    <span className="spinner" aria-hidden />
                    <span>任务{status === 'activating' ? '激活中' : '状态变更中'}…</span>
                  </>
                ) : status === 'suspended' ? (
                  <>
                    <span>任务已挂起，激活后可接入 opencode TUI</span>
                    {initBlock && <span className="terminal-overlay-reason">{initBlock}</span>}
                    <span className="terminal-overlay-actions">
                      <ActivateButton task={task} onDone={() => void load()} onError={setError} />
                      <RerunInitButton
                        task={task}
                        onDone={() => void load()}
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
          onDeleted={() => navigate(`/project/${task.project_id}`)}
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
          onError(err instanceof ApiError ? err.message : '激活失败');
          onDone();
        } finally {
          setBusy(false);
        }
      }}
    >
      {busy ? '激活中…' : '激活任务'}
    </button>
  );
}

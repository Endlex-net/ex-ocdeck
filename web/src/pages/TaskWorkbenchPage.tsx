import { useEffect, useState } from 'react';
import { api, ApiError } from '../api';
import { navigate } from '../router';
import { usePoll } from '../hooks';
import { isTransitional, parseNotice, type Task } from '../types';
import { StatusBadge } from '../components/StatusBadge';
import { TaskActions } from '../components/TaskActions';
import { DeleteTaskModal } from '../components/DeleteTaskModal';
import { GitPanel } from '../components/GitPanel';
import { EnvEditor } from '../components/EnvEditor';
import { AgentStatusBadge } from '../components/AgentStatusBadge';
import { TerminalView } from '../terminal/TerminalView';

const TUI_TAB = 'tui';
const GIT_TAB = 'git';
const SETTINGS_TAB = 'settings';

export function TaskWorkbenchPage({ taskID }: { taskID: string }) {
  const [task, setTask] = useState<Task | null>(null);
  const [shells, setShells] = useState<string[]>([]);
  // 区分"无 shell 终端"与"列表获取失败"：失败时给出错误提示并可重试
  const [shellsError, setShellsError] = useState('');
  const [tab, setTab] = useState<string>(TUI_TAB);
  const [error, setError] = useState('');
  const [notFound, setNotFound] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [addingShell, setAddingShell] = useState(false);
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
  usePoll(() => void load(), task && isTransitional(task.status) ? 2000 : 4000, [
    task?.status,
  ]);

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

  return (
    <div className="workbench">
      <header className="page-header">
        <button
          className="btn btn-small btn-ghost"
          onClick={() => task && navigate(`/project/${task.project_id}`)}
        >
          ← 任务列表
        </button>
        <span className="page-title">{task?.name ?? '…'}</span>
        {task?.branch && <span className="header-meta mono">⎇ {task.branch}</span>}
        {task && <StatusBadge status={task.status} />}
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
        <button
          className={`tab ${tab === GIT_TAB ? 'tab-active' : ''}`}
          onClick={() => switchTab(GIT_TAB)}
        >
          Git
        </button>
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
                    <ActivateButton task={task} onDone={() => void load()} onError={setError} />
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
        {visited.has(GIT_TAB) && (
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
            </div>
          </div>
        )}
      </div>

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
  return (
    <button
      className="btn btn-primary btn-small"
      disabled={busy}
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

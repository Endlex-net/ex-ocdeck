import { useEffect, useMemo, useRef, useState } from 'react';
import { api, ApiError } from '../api';
import { navigate } from '../router';
import { resolveBackHref, type FromSource } from '../router';
import { useMediaQuery, useProjects, useProjectsRefresh } from '../hooks';
import { debugMark } from '../debug';
import { subscribeTask } from '../sse';
import { isGitlessTask, isTransitional, initActivateBlockReason, parseNotice, type Task } from '../types';
import { shouldCloseOverflowOnBlur, visibleOverflowItems } from './workbench-overflow';
import { baseRefShortName, currentBranchTooltip, sourceBranchTooltip } from './workbench-branch';
import { writeTextToClipboard } from '../clipboard';
import { StatusBadge } from '../components/StatusBadge';
import { TaskActions } from '../components/TaskActions';
import { DeleteTaskModal } from '../components/DeleteTaskModal';
import { GitPanel } from '../components/GitPanel';
import { EnvEditor } from '../components/EnvEditor';
import { AgentStatusBadge } from '../components/AgentStatusBadge';
import { InitStatusBadge } from '../components/InitStatusBadge';
import { PermissionModeBadge } from '../components/PermissionModeBadge';
import { LifecycleLogModal } from '../components/LifecycleLogModal';
import { OpenInEditorMenu } from '../components/OpenInEditorMenu';
import { RerunInitButton } from '../components/RerunInitButton';
import { TerminalView } from '../terminal/TerminalView';
import {
  cancelTerminalFocusForTask,
  isTerminalFocusExpired,
  requestTerminalFocus,
  subscribeTerminalFocus,
} from '../terminal/focus-request';
import { BranchIcon, CaretDownIcon, MoreIcon, SourceRefIcon, WarnIcon, InfoIcon } from '../icons';

const TUI_TAB = 'tui';
const GIT_TAB = 'git';
const SETTINGS_TAB = 'settings';

/** 页头分支复制反馈时长（design D3 点击复制）：复用 .od-toast 2000ms 重显模式。 */
const BRANCH_TOAST_MS = 2000;

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
  // 可见菜单项（design D4）：可见性仅由显示条件决定（visibleOverflowItems），
  // 删除项禁用态（isTransitional）不参与入口显隐；顺序"日志→删除"。
  const items = visibleOverflowItems(task.init_status, task.status);

  // disclosure 模式：打开后焦点进入菜单内首个可用项
  useEffect(() => {
    if (menuOpen) {
      menuRef.current?.querySelector<HTMLButtonElement>('button:not(:disabled)')?.focus();
    }
  }, [menuOpen]);

  // design D4：可见项由非空变为空（下方 return null）时 MUST 同时关闭展开状态；
  // 后续恢复时仅显示关闭的触发器，不自动展开、不调用任何操作回调。
  useEffect(() => {
    if (items.length === 0) setMenuOpen(false);
  }, [items.length]);

  // 关闭溢出菜单；键盘取消（Escape）/失焦兜底后焦点恢复触发器
  const closeMenu = (restoreFocus: boolean) => {
    setMenuOpen(false);
    if (restoreFocus) menuTriggerRef.current?.focus();
  };

  // 空态（design D4）：无可见菜单项时整个入口不渲染（所有 hook 之后，同 OpenInEditorMenu）
  if (items.length === 0) return null;

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
            {items.map((item) =>
              item === 'init-log' ? (
                <button
                  key="init-log"
                  className="overflow-item"
                  onClick={() => {
                    setMenuOpen(false);
                    onShowInitLog();
                  }}
                >
                  查看 init 日志
                </button>
              ) : (
                <button
                  key="delete"
                  className="overflow-item overflow-item-danger"
                  disabled={isTransitional(task.status)}
                  onClick={() => {
                    setMenuOpen(false);
                    onDelete();
                  }}
                >
                  删除任务
                </button>
              ),
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

  // 标签激活（design D3，G1 闭合同任务再点击）：目标任务收到匹配且未过期的焦点请求、
  // 当前 tab ≠ TUI（如 Git/shell/设置）时激活 TUI 标签——只激活不消费（消费在 TUI
  // TerminalView）。仅显式导航请求触发（订阅回调只在发布/挂载快照时触发），普通重连、
  // 无请求的挂载不产生调用、不切标签。
  const tabRef = useRef(tab);
  tabRef.current = tab;
  useEffect(() => {
    const unsub = subscribeTerminalFocus((req) => {
      if (req.taskID !== taskID || isTerminalFocusExpired(req)) return;
      if (tabRef.current !== TUI_TAB) switchTab(TUI_TAB);
    });
    return () => {
      unsub();
      // 路由离开目标任务（工作台卸载，含 task 未返回、TerminalView 未挂载的加载
      // 阶段）：该任务未消费的请求作废（design D3 取消规则）。按 taskID 守卫，
      // 不误删其他任务更晚发布的新请求。
      cancelTerminalFocusForTask(taskID);
    };
    // switchTab 仅包装 setTab/setVisited（稳定 setter），首帧闭包语义不变
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [taskID]);

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

  // Git 能力判定（add-local-path-task-mode 6.2/D8）：仅 dir 项目任务隐藏 Git tab 与 git 面板入口，
  // repo local-path 任务开放（git 操作作用于项目目录当前分支）。
  const isGitless = task?.project_kind === 'dir';
  // 任务分支展示判定：dir 或 repo local-path 任务无分支概念（task.branch 恒空），页头/任务行分支名隐藏
  const isBranchless = !!task && isGitlessTask(task.project_kind, task.mode);

  // local-path（repo 项目）页头当前分支（design D3）：任务详情就绪后经既有 api.gitStatus
  // 一次性获取，按任务身份隔离、随 taskID 切换重新获取、不轮询。请求条件 MUST 为
  // project_kind==='repo' && mode==='local-path'（MUST NOT 用 isGitlessTask 门控——
  // 其同时覆盖 dir 与 local-path）；非 401 失败/空串静默降级不展示（不设置页面错误、
  // 不自动重试），401 沿用共享 API 客户端的既有认证失效流程（客户端内清 token + 全局事件）。
  const isLocalPathRepo = !!task && task.project_kind === 'repo' && task.mode === 'local-path';
  const [localPathBranch, setLocalPathBranch] = useState('');
  useEffect(() => {
    setLocalPathBranch('');
    if (!isLocalPathRepo) return;
    let cancelled = false;
    api.gitStatus(taskID).then(
      (s) => {
        if (!cancelled) setLocalPathBranch(s.branch ?? '');
      },
      () => {
        /* 静默降级：本次页面停留期间不展示页头分支 */
      },
    );
    return () => {
      cancelled = true;
    };
  }, [taskID, isLocalPathRepo]);

  // 页头分支名点击复制反馈（design D3）：复用 .od-toast（2000ms、clearTimeout 重显），
  // 手动点击 MUST NOT 套用 takeToastSlot 节流；连续点击重置计时器重显，不排队不叠加。
  const [branchToast, setBranchToast] = useState('');
  const branchToastTimer = useRef<ReturnType<typeof setTimeout>>();
  // 组件存活守卫（oracle I2）：卸载后迟到的复制回调不再 setState/创建定时器；
  // 卸载时清掉在途 toast 计时器。
  const aliveRef = useRef(true);
  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
      if (branchToastTimer.current !== undefined) clearTimeout(branchToastTimer.current);
    };
  }, []);
  const showBranchToast = (msg: string) => {
    if (!aliveRef.current) return;
    setBranchToast(msg);
    if (branchToastTimer.current !== undefined) clearTimeout(branchToastTimer.current);
    branchToastTimer.current = setTimeout(() => setBranchToast(''), BRANCH_TOAST_MS);
  };
  const copyBranchName = (name: string) => {
    void writeTextToClipboard(name).then(
      () => showBranchToast(`已复制 ${name}`),
      () => showBranchToast('复制失败，完整分支名见悬浮提示'),
    );
  };
  // dir 任务无 git 能力时若正停在 Git tab，回退到 TUI tab（repo local-path 不回退）
  useEffect(() => {
    if (isGitless && tab === GIT_TAB) setTab(TUI_TAB);
  }, [isGitless, tab]);

  // 页头任务切换器候选：与侧栏任务组同源（共享 store），仅活跃+挂起任务（归档不显示）。
  // data-od-id / wb-* class 对齐设计稿 task-workbench.html:229
  // 注意：以下 Hook 必须在任何条件 return 之前（404 分支不得改变 Hook 调用次数）。
  const switcherTasks = useMemo(() => {
    const out: Array<{ taskID: string; name: string; branch: string; gitless: boolean; projectName: string; agentStatus?: string; attentionCount: number; current: boolean }> = [];
    for (const p of projects) {
      // 与侧栏 SidebarTaskGroups 同源：仅 active+suspended（归档/失败等不显示）
      for (const t of (p.tasks ?? []).filter((x) => x.status === 'active' || x.status === 'suspended')) {
        out.push({
          taskID: t.id,
          name: t.name,
          branch: t.branch,
          gitless: isGitlessTask(p.kind, t.mode),
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
  // 来源分支展示短名（design D2）：空串→空串；异常形态原样返回（baseRefShortName 契约）
  const baseShort = baseRefShortName(task?.base_ref ?? '');
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
                          // 导航焦点请求信号（design D3）：navigate 照常，信号为附加调用
                          requestTerminalFocus(t.taskID);
                          navigate(`/task/${t.taskID}`);
                        }}
                      >
                        {/* 有待答问题/待授权限时蓝点（等待人工 > 运行态） */}
                        <span
                          className={`od-agent od-agent-${t.attentionCount > 0 ? 'attention' : (t.agentStatus ?? '')}`}
                          title={t.attentionCount > 0 ? `等待人工处理：${t.attentionCount} 个待处理请求` : undefined}
                        ><span className="od-agent-dot" /></span>
                        <span className="wb-sw-name">{t.name}</span>
                        {/* gitless 任务（dir/local-path）不渲染分支名，整段移除不留占位 */}
                        {!t.gitless && <span className="mono">{t.branch}</span>}
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
        {/* 页头分支区（两行布局 v3：上行当前 / 下行来源）：dir 整段不渲染；local-path
            （repo 项目）展示文件系统当前分支（仅当前行；空串/获取失败静默降级不展示）；
            worktree 保留 branch 非空外层条件（branch 为空整段不渲染，即使 base_ref 非空），
            base_ref 非空时两行「上当前（提亮 --fg）/ 下来源（弱化 muted·0.7 + ↳ 静态图标）」，
            空串仅当前行。10px meta 级，⎇ 图标对齐首行。
            分支名 button 化点击复制（所见即所得；截断显示不影响复制完整名）；同名不去重。
            tooltip 下沉到各 button（唯一模板集），容器 span 不挂 title。 */}
        {task && isLocalPathRepo && localPathBranch !== '' && (
          <span className="header-meta mono header-meta-branches">
            <BranchIcon />
            <span className="branch-rows">
              <button
                type="button"
                className="branch-copy branch-copy-cur"
                aria-label={`复制当前分支 ${localPathBranch}`}
                title={currentBranchTooltip(localPathBranch)}
                onClick={() => copyBranchName(localPathBranch)}
              >
                {localPathBranch}
              </button>
            </span>
          </span>
        )}
        {task && !isBranchless && !!task.branch && (
          <span className="header-meta mono header-meta-branches">
            <BranchIcon />
            <span className="branch-rows">
              <button
                type="button"
                className="branch-copy branch-copy-cur"
                aria-label={`复制当前分支 ${task.branch}`}
                title={currentBranchTooltip(task.branch)}
                onClick={() => copyBranchName(task.branch)}
              >
                {task.branch}
              </button>
              {baseShort !== '' && (
                <span className="branch-src-row">
                  <SourceRefIcon />
                  <button
                    type="button"
                    className="branch-copy branch-copy-src"
                    aria-label={`复制来源分支 ${baseShort}`}
                    title={sourceBranchTooltip(baseShort, task.base_ref ?? '')}
                    onClick={() => copyBranchName(baseShort)}
                  >
                    {baseShort}
                  </button>
                </span>
              )}
            </span>
          </span>
        )}
        {task && <StatusBadge status={task.status} />}
        {task && <InitStatusBadge task={task} />}
        {/* 权限模式只读展示（task-permission-mode D7）：与状态徽标同组，创建后不可修改 */}
        {task && <PermissionModeBadge task={task} />}
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
            {/* 「在编辑器中打开」快捷入口（add-frontend-tool-quick-open）：header-spacer 之后、主操作之前 */}
            <OpenInEditorMenu worktreePath={task.worktree_path} />
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
            <OpenInEditorMenu worktreePath={task.worktree_path} />
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
        {!isGitless && (
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
        {visited.has(GIT_TAB) && !isGitless && (
          <div className={`pane pane-scroll ${tab === GIT_TAB ? '' : 'pane-hidden'}`}>
            <GitPanel
              taskID={taskID}
              active={tab === GIT_TAB}
              agentBusy={task?.agentStatus === 'busy' || task?.agentStatus === 'retry'}
            />
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

      {/* 页头分支复制反馈（design D3）：.od-toast 固定底部居中；连续点击重显不节流 */}
      {branchToast !== '' && (
        <div className="od-toast mono" role="status">
          {branchToast}
        </div>
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

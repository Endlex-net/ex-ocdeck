import { useEffect, useState } from 'react';
import { useHashRoute, resolveRoute } from '../router';
import { useProjects } from '../hooks';
import { ServerStatusBanner } from './ServerStatusBanner';
import { orderSidebarGroups, transitionalLabel } from './sidebar-order';
import type { Project } from '../types';
import {
  HomeIcon,
  FolderIcon,
  SettingsIcon,
  SearchIcon,
  SidebarCollapseIcon,
  SidebarExpandIcon,
} from '../icons';

const COLLAPSE_KEY = 'ocdeck:side-collapsed';

function readCollapsed(): boolean {
  try {
    return localStorage.getItem(COLLAPSE_KEY) === '1';
  } catch {
    return false;
  }
}

function persistCollapsed(v: boolean): void {
  try {
    localStorage.setItem(COLLAPSE_KEY, v ? '1' : '0');
  } catch {
    /* localStorage 不可用时静默 */
  }
}

/** 壳层插槽（P3/P4 页面挂载点）。本阶段只做占位接线，现有页面组件原样挂进。 */
export interface AppShellProps {
  children: React.ReactNode;
  /** ⌘K 命令面板唤出函数（由 App 注入，避免壳层直接耦合面板实现）。 */
  onOpenPalette: () => void;
  /** 主题切换按钮回调（useTheme.setPreference 循环 system→light→dark）。 */
  onToggleTheme: () => void;
  /** 当前主题偏好，决定切换按钮图标。 */
  themePref: 'system' | 'light' | 'dark';
  /** 命令面板热键展示文案（App 用 formatHotkey 下发）。 */
  paletteHotkeyLabel: string;
}

/** 全局应用壳层（design.md D1 + spec web-ui-shell）：
 *  左侧导航脊柱（品牌区/指挥中心顶层项/任务组/管理组/底栏）+ 内容区。
 *  未认证时 App 不渲染壳层（仅 TokenGate）。ServerStatusBanner 由 App 在壳层内渲染。
 *  ⌘B 折叠为 60px 图标轨，localStorage 持久化，≥768px 生效。 */
export function AppShell({
  children,
  onOpenPalette,
  onToggleTheme,
  themePref,
  paletteHotkeyLabel,
}: AppShellProps) {
  const route = useHashRoute();
  const { projects } = useProjects();

  const [collapsed, setCollapsed] = useState(() => readCollapsed());

  // 同步 body.od-side-collapsed（与 design-system.css 折叠规则、ocdeck-sidebar.js 行为一致）。
  useEffect(() => {
    document.body.classList.toggle('od-side-collapsed', collapsed);
  }, [collapsed]);

  // ⌘B / Ctrl+B 切换折叠（≥768px 生效；窄屏侧栏隐藏另有响应式处理）
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && (e.key === 'b' || e.key === 'B')) {
        e.preventDefault();
        setCollapsed((v) => {
          const next = !v;
          persistCollapsed(next);
          return next;
        });
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  const res = resolveRoute(route);
  // 重定向在 App.tsx 处理，壳层只读当前 page 用于高亮。这里 res.kind === 'redirect' 时高亮为 home。
  const currentPage = res.kind === 'page' ? res.page : 'home';
  const currentTaskID = res.kind === 'page' && res.page === 'task' ? res.taskID : undefined;
  // 工作台需确定高度链（终端 pane 绝对定位）：fullbleed 模式解除 od-content 的 max-width/padding，
  // 让 .workbench height:100% 链路经 .od-main(flex:1) → .od-content(flex:1) 到达视口高度。
  const fullbleed = res.kind === 'page' && res.page === 'task';

  return (
    <div className="od-shell">
      <aside className="od-sidebar" data-od-id="sidebar">
        <div className="od-brand">
          <span className="od-brand-mark">oc</span>
          <div className="od-brand-text">
            <div className="od-brand-name">ocdeck</div>
            <div className="od-brand-sub">opencode 编排台</div>
          </div>
        </div>

        <nav className="od-nav" data-od-id="global-nav">
          <a
            className="od-nav-item"
            href="#/"
            aria-current={currentPage === 'home' ? 'page' : undefined}
            title="指挥中心"
          >
            <HomeIcon />
            <span className="od-nav-label">指挥中心</span>
          </a>

          <div className="od-nav-group">任务</div>
          <SidebarTaskGroups projects={projects} currentTaskID={currentTaskID} />

          <div className="od-nav-group">管理</div>
          <a
            className="od-nav-item"
            href="#/projects"
            aria-current={currentPage === 'projects' ? 'page' : undefined}
            title="项目管理"
          >
            <FolderIcon />
            <span className="od-nav-label">项目管理</span>
          </a>
          <a
            className="od-nav-item"
            href="#/configs"
            aria-current={currentPage === 'configs' ? 'page' : undefined}
            title="设置"
          >
            <SettingsIcon />
            <span className="od-nav-label">设置</span>
          </a>
        </nav>

        <div className="od-sidebar-foot mono">
          <button
            className="od-sidebar-cmdk"
            type="button"
            onClick={onOpenPalette}
            title={`命令面板（${paletteHotkeyLabel}）`}
          >
            <SearchIcon />
            <span className="od-cmdk-label">命令面板</span>
            <span className="od-palette-kbd">{paletteHotkeyLabel}</span>
          </button>
          <span className="od-side-addr" title="本地服务 · 单用户模式">
            {location.host || '127.0.0.1'}
          </span>
          <button
            className="od-theme-btn"
            type="button"
            onClick={onToggleTheme}
            aria-label="切换主题"
            title="切换主题"
          >
            <ThemeIcon pref={themePref} />
          </button>
          <button
            className="od-theme-btn od-side-collapse"
            type="button"
            onClick={() => {
              const next = !collapsed;
              setCollapsed(next);
              persistCollapsed(next);
            }}
            aria-expanded={!collapsed}
            aria-label={collapsed ? '展开侧边栏（⌘B）' : '收起侧边栏（⌘B）'}
            title={collapsed ? '展开侧边栏（⌘B）' : '收起侧边栏（⌘B）'}
          >
            {collapsed ? <SidebarExpandIcon /> : <SidebarCollapseIcon />}
          </button>
        </div>
      </aside>

      <div className="od-main">
        {/* ServerStatusBanner 壳层内全页面可见（spec）：作为 .od-content 的兄弟，
            不参与内容区滚动/高度链，避免工作台 fullbleed 时 banner+100% 溢出。 */}
        <ServerStatusBanner />
        <div className={`od-content${fullbleed ? ' od-content-fullbleed' : ''}`}>{children}</div>
      </div>
    </div>
  );
}

/** 侧栏任务组：按项目分组显示 active+suspended 任务（归档不显示），agent 状态点 + 注意力标记。
 *  点击导航 #/task/:id。数据来自共享 store（projects 5s 轮询）。
 *  排序：活跃度优先（orderSidebarGroups 纯函数）——组内 注意力 > busy > idle > 过渡态 > 挂起，
 *  组间按组内最优任务键；无可见任务的项目组整体不展示（组头也不渲染）。 */
function SidebarTaskGroups({ projects, currentTaskID }: { projects: Project[]; currentTaskID?: string }) {
  const groups = orderSidebarGroups(projects);

  // 无任何可显示任务（含无项目/全空组）：保持既有空态提示
  if (groups.length === 0) {
    return <div className="od-nav-empty muted">暂无活跃或挂起任务</div>;
  }

  return (
    <>
      {groups.map(({ project, tasks }) => (
        <div className="od-nav-proj-group" key={project.id}>
          <div className="od-nav-proj">{project.name}</div>
          {tasks.map((t) => {
            const transLabel = transitionalLabel(t.status);
            return (
              <a
                key={t.id}
                className="od-nav-task"
                href={`#/task/${t.id}`}
                aria-current={currentTaskID === t.id ? 'page' : undefined}
                title={transLabel ? `${t.name}（${transLabel}）` : t.name}
              >
                {/* 过渡态（创建中/激活中/挂起中）：spinner 呈现；其余沿用 agent 状态点。
                    待人工（attention_count>0）蓝点优先于运行态：圆点表达状态语言，
                    右侧 od-nav-attention 计数丸保留计数职责。 */}
                {transLabel ? (
                  <span className="od-spinner od-nav-task-spinner" aria-hidden />
                ) : (
                  <span className={`od-agent od-agent-${agentDotClass(t.agentStatus, t.attention_count)}`}>
                    <span className="od-agent-dot"></span>
                  </span>
                )}
                <span className="od-nav-task-name">{t.name}</span>
                {(t.attention_count ?? 0) > 0 && (
                  <span className="od-nav-attention" title={`${t.attention_count} 个需要关注`}>
                    {t.attention_count}
                  </span>
                )}
              </a>
            );
          })}
        </div>
      ))}
    </>
  );
}

/** agent 状态点 class：等待人工（attention_count>0）蓝点优先于 idle/busy/retry；未知降级为 idle-off 不亮。 */
function agentDotClass(agentStatus?: string, attentionCount = 0): 'idle' | 'busy' | 'retry' | 'attention' | 'idle-off' {
  if (attentionCount > 0) return 'attention';
  if (agentStatus === 'busy') return 'busy';
  if (agentStatus === 'retry') return 'retry';
  if (agentStatus === 'idle') return 'idle';
  return 'idle-off';
}

/** 主题切换按钮图标：跟随当前有效主题循环（system 跟随时显示对应图标）。 */
function ThemeIcon({ pref }: { pref: 'system' | 'light' | 'dark' }) {
  // 简化：system 时显示太阳（跟随），dark 显示月亮，light 显示太阳。
  // 真正的循环逻辑由 App 的 onToggleTheme 控制；图标反映当前 effective 主题由父层传入 pref 近似。
  if (pref === 'dark') {
    return (
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" />
      </svg>
    );
  }
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" aria-hidden="true">
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
    </svg>
  );
}
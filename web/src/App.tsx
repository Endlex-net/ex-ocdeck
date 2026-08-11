import { useCallback, useEffect, useState } from 'react';
import { getToken, UNAUTHORIZED_EVENT } from './api';
import { navigate, useHashRoute, resolveRoute, isConfigsTab, type ConfigsTab } from './router';
import { useTheme } from './hooks';
import { TokenGate } from './components/TokenGate';
import { AppShell } from './components/AppShell';
import { CommandPalette } from './components/CommandPalette';
import { CommandCenterPage } from './pages/CommandCenterPage';
import { ProjectsManagePage } from './pages/ProjectsManagePage';
import { SettingsPage } from './pages/SettingsPage';
import { TaskWorkbenchPage } from './pages/TaskWorkbenchPage';
import { emitPaletteFocus } from './palette-focus';

export function App() {
  const [authed, setAuthed] = useState(() => getToken() !== '');
  const route = useHashRoute();
  const { preference, setPreference } = useTheme();
  const [paletteOpen, setPaletteOpen] = useState(false);

  useEffect(() => {
    const onUnauthorized = () => setAuthed(false);
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
  }, []);

  // 路由解析：重定向用 replace 模式（不污染返回栈，避免返回键死循环），非法深链由 resolveRoute 内部回退。
  // 注意 navigate(replace) 触发 hashchange → useHashRoute 重渲染，下一轮 resolveRoute 得到目标页。
  const res = resolveRoute(route);
  useEffect(() => {
    if (res.kind === 'redirect') navigate(res.target, true);
  }, [res]);

  // ⌘K / Ctrl+K 唤出命令面板（不与壳层 ⌘B 冲突）。
  useEffect(() => {
    if (!authed) return;
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && String(e.key).toLowerCase() === 'k') {
        e.preventDefault();
        setPaletteOpen((v) => !v);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [authed]);

  // 主题切换按钮：system → light → dark → system 循环。
  const toggleTheme = useCallback(() => {
    const order: Array<'system' | 'light' | 'dark'> = ['system', 'light', 'dark'];
    const next = order[(order.indexOf(preference) + 1) % order.length];
    setPreference(next);
  }, [preference, setPreference]);

  if (!authed) {
    return <TokenGate onSaved={() => setAuthed(true)} />;
  }

  // 重定向周期：渲染 null，下一帧 navigate 生效。
  if (res.kind === 'redirect') return null;

  let page: React.ReactNode;
  if (res.page === 'configs') {
    // 设置四合一（P3）：四子标签深链恢复（#/configs#appearance|#env|#opencode|#ai）。
    // resolveRoute 已对未知 tab 回退 appearance；此处二次防御（fragment 类型为 string）。
    const tab: ConfigsTab = isConfigsTab(res.fragment) ? res.fragment : 'appearance';
    page = <SettingsPage tab={tab} />;
  } else if (res.page === 'projects') {
    // 项目管理 master-detail（P3）：#/projects#<id> 深链选中。
    page = <ProjectsManagePage />;
  } else if (res.page === 'task' && res.taskID) {
    // 工作台：?from 归一（active→home、projects→projects、未知→home），key={taskID} 重挂载（D8）。
    page = <TaskWorkbenchPage key={res.taskID} taskID={res.taskID} from={res.from ?? 'home'} />;
  } else {
    // 指挥中心（home）：任务优先首页。
    page = <CommandCenterPage />;
  }

  return (
    <>
      <AppShell
        onOpenPalette={() => setPaletteOpen(true)}
        onToggleTheme={toggleTheme}
        themePref={preference}
      >
        {page}
      </AppShell>
      <CommandPalette
        open={paletteOpen}
        onClose={() => setPaletteOpen(false)}
        onNewTask={() => {
          // 导航指挥中心 + od:palette-focus：展开内联创建并聚焦任务名
          // （design: ocdeck-palette.js + command-center.html:328-330）
          navigate('/');
          emitPaletteFocus('new-task-name');
        }}
        onRegisterProject={() => {
          // 导航项目管理 + 展开注册表单并聚焦名称
          navigate('/projects');
          emitPaletteFocus('register-project-name');
        }}
      />
    </>
  );
}
import { useEffect, useState } from 'react';
import { getToken, UNAUTHORIZED_EVENT } from './api';
import { useHashRoute } from './router';
import { TokenGate } from './components/TokenGate';
import { ServerStatusBanner } from './components/ServerStatusBanner';
import { ProjectsPage } from './pages/ProjectsPage';
import { ProjectDetailPage } from './pages/ProjectDetailPage';
import { TaskWorkbenchPage } from './pages/TaskWorkbenchPage';
import { ConfigsPage } from './pages/ConfigsPage';
import { AIConfigPage } from './pages/AIConfigPage';
import { ActiveSessionsPage } from './pages/ActiveSessionsPage';

export function App() {
  const [authed, setAuthed] = useState(() => getToken() !== '');
  const route = useHashRoute();

  useEffect(() => {
    const onUnauthorized = () => setAuthed(false);
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
  }, []);

  if (!authed) {
    return <TokenGate onSaved={() => setAuthed(true)} />;
  }

  // 拆分 query：来源感知导航等参数不得混入路径匹配（如 taskID 捕获）
  const [path, query = ''] = route.split('?');
  const projectMatch = path.match(/^\/project\/([^/]+)$/);
  const taskMatch = path.match(/^\/task\/([^/]+)$/);
  const fromActive = new URLSearchParams(query).get('from') === 'active';

  let page: React.ReactNode;
  if (path === '/configs') page = <ConfigsPage />;
  else if (path === '/ai-config') page = <AIConfigPage />;
  else if (path === '/active') page = <ActiveSessionsPage />;
  // 按路由 ID key 页面组件：直接项目→项目 / 任务→任务导航时整体重挂载，
  // 避免跨项目/跨任务复用旧状态（如基线分支、分支列表）
  else if (projectMatch)
    page = <ProjectDetailPage key={projectMatch[1]} projectID={projectMatch[1]} />;
  else if (taskMatch)
    page = <TaskWorkbenchPage key={taskMatch[1]} taskID={taskMatch[1]} fromActive={fromActive} />;
  else page = <ProjectsPage />;

  // 应用级骨架：server 状态告警 banner 在所有页面顶部可见；
  // app-content 承担滚动（workbench 高度 100% 布局不受影响）。
  return (
    <div className="app-shell">
      <ServerStatusBanner />
      <div className="app-content">{page}</div>
    </div>
  );
}

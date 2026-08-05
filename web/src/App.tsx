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

  const projectMatch = route.match(/^\/project\/([^/]+)$/);
  const taskMatch = route.match(/^\/task\/([^/]+)$/);

  let page: React.ReactNode;
  if (route === '/configs') page = <ConfigsPage />;
  else if (route === '/ai-config') page = <AIConfigPage />;
  else if (projectMatch) page = <ProjectDetailPage projectID={projectMatch[1]} />;
  else if (taskMatch) page = <TaskWorkbenchPage taskID={taskMatch[1]} />;
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

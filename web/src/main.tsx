import { createRoot } from 'react-dom/client';
import { App } from './App';
// 设计系统样式（od-* token + 壳层/侧栏/命令面板）：优先级基准。
import './design-system.css';
// 保留组件样式（TaskWorkbenchPage / GitPanel / EnvEditor / modal / badge 等）。
// 迁移自旧 styles.css（tasks 8.5），token 化为 design-system 变量，死类已移除。
import './legacy-components.css';

createRoot(document.getElementById('root')!).render(<App />);

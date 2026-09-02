import { useEffect, useState } from 'react';

/** 轻量 hash 路由：#/ → 指挥中心，#/projects、#/configs、#/task/:id。
 *  hash 路由对静态托管零要求（无需 SPA fallback）。 */
export function useHashRoute(): string {
  const [route, setRoute] = useState(() => location.hash.slice(1) || '/');
  useEffect(() => {
    const onChange = () => setRoute(location.hash.slice(1) || '/');
    window.addEventListener('hashchange', onChange);
    return () => window.removeEventListener('hashchange', onChange);
  }, []);
  return route;
}

/** 导航依赖：抽 history/location 为可注入接口，便于纯函数测试（无 jsdom 环境）。
 *  生产环境用全局 history/window；测试注入 fake。 */
export interface NavigateEnv {
  replaceState: (data: unknown, unused: string, url: string) => void;
  pushHash: (hash: string) => void;
  dispatchHashChange: () => void;
}

const defaultEnv: NavigateEnv = {
  replaceState: (data, unused, url) => history.replaceState(data, unused, url),
  pushHash: (hash) => {
    location.hash = hash;
  },
  dispatchHashChange: () => {
    window.dispatchEvent(new HashChangeEvent('hashchange'));
  },
};

/** 导航到 hash 路径。replace=true 时替换当前历史项（重定向用，避免污染返回栈）；
 *  replace 走 history.replaceState + 手动 dispatch hashchange（不新增历史项）。
 *  replace=false 走 location.hash 赋值（新增历史项，正常导航）。
 *  env 参数供测试注入（不依赖全局 history/location）。 */
export function navigate(path: string, replace = false, env: NavigateEnv = defaultEnv): void {
  const hash = `#${path}`;
  if (replace) {
    env.replaceState(null, '', hash);
    // replaceState 不触发 hashchange，手动派发以让 useHashRoute 更新。
    env.dispatchHashChange();
  } else {
    env.pushHash(hash);
  }
}

/** 设置页子标签。 */
export type ConfigsTab = 'appearance' | 'env' | 'opencode' | 'ai' | 'notifications' | 'palette';

const CONFIGS_TABS: ReadonlySet<string> = new Set([
  'appearance',
  'env',
  'opencode',
  'ai',
  'notifications',
  'palette',
]);

export function isConfigsTab(v: unknown): v is ConfigsTab {
  return typeof v === 'string' && CONFIGS_TABS.has(v);
}

/** 工作台 ?from 来源感知（design.md D3 归一映射）。 */
export type FromSource = 'home' | 'projects';

/** 将原始 ?from 值归一为 FromSource：active → home（legacy 别名），未知/缺省 → home。
 *  纯函数，无副作用，可独立测试。 */
export function normalizeFrom(raw: string | null | undefined): FromSource {
  if (raw === 'projects') return 'projects';
  // active 是 legacy 别名映射到 home（旧 #/task/:id?from=active 链接不断，design.md D3）
  return 'home';
}

/** 根据归一后的 from 与 projectID 解析返回链接（design.md D3 统一函数）。
 *  home → #/，projects → #/projects#<projectID>。 */
export function resolveBackHref(from: FromSource, projectID?: string): string {
  if (from === 'projects' && projectID) return `/projects#${projectID}`;
  return '/';
}

/** 路由解析结果：区分主页面与重定向动作（纯函数，路由层不做副作用）。 */
export type RouteResolution =
  | { kind: 'page'; page: 'home' | 'projects' | 'configs' | 'task'; taskID?: string; from?: FromSource; fragment?: string }
  | { kind: 'redirect'; target: string };

/** 解析 hash 路由（含 ?query 与 #fragment，design.md D3 路由收敛+重定向+?from 归一）。
 *  - #/ → 指挥中心（home）
 *  - #/projects（可选 #<projectID> 深链选中）
 *  - #/configs（可选 #<tab> 深链子标签，未知 tab 回退 appearance）
 *  - #/task/:id（保留 ?from 归一映射）
 *  - #/active → 重定向 #/
 *  - #/ai-config → 重定向 #/configs#ai
 *  - #/project/:id → 重定向 #/projects#<id>
 *  - 其余回退 home（#/）
 *  纯函数，不读取 location、不写 hash；调用方据结果决定 navigate 或渲染。 */
export function resolveRoute(hash: string): RouteResolution {
  // hash 形如 "/path?query#fragment"；slice(1) 已由 useHashRoute 剥去 #，本函数接收 hash 路体。
  const route = hash || '/';
  // 先分离 fragment：route 可能是 "/path?q#frag"
  const [pathAndQuery, fragment] = splitFragment(route);
  const [path, query] = splitQuery(pathAndQuery);

  // 旧路由重定向。
  if (path === '/active') return { kind: 'redirect', target: '/' };
  if (path === '/ai-config') return { kind: 'redirect', target: '/configs#ai' };
  const projectMatch = path.match(/^\/project\/([^/]+)$/);
  if (projectMatch) return { kind: 'redirect', target: `/projects#${projectMatch[1]}` };

  // 收敛后的主页面。
  if (path === '/' || path === '') return { kind: 'page', page: 'home', fragment };
  if (path === '/projects') return { kind: 'page', page: 'projects', fragment };
  if (path === '/configs') {
    // 未知 tab 回退 appearance（不选中即外观）
    const tab = fragment && isConfigsTab(fragment) ? (fragment as ConfigsTab) : 'appearance';
    return { kind: 'page', page: 'configs', fragment: tab };
  }
  const taskMatch = path.match(/^\/task\/([^/]+)$/);
  if (taskMatch) {
    const fromRaw = new URLSearchParams(query).get('from');
    return { kind: 'page', page: 'task', taskID: taskMatch[1], from: normalizeFrom(fromRaw), fragment };
  }
  // 非法深链回退 home（不报错）
  return { kind: 'page', page: 'home', fragment };
}

/** 将路径段中的 fragment（首个 #）分离。route 形如 "/path?q#frag" → ["/path?q", "frag"]。 */
function splitFragment(route: string): [string, string | undefined] {
  const idx = route.indexOf('#');
  if (idx === -1) return [route, undefined];
  return [route.slice(0, idx), route.slice(idx + 1) || undefined];
}

/** 将 query（首个 ?）分离。path 形如 "/task/abc?from=home" → ["/task/abc", "from=home"]。 */
function splitQuery(path: string): [string, string] {
  const idx = path.indexOf('?');
  if (idx === -1) return [path, ''];
  return [path.slice(0, idx), path.slice(idx + 1)];
}
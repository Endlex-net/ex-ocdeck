import { useEffect, useState } from 'react';

/** 轻量 hash 路由：#/ → 项目列表，#/project/:id，#/task/:id。
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

export function navigate(path: string): void {
  location.hash = path;
}

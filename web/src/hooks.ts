import { useEffect, useRef, useState } from 'react';

/** 轮询：立即执行一次，之后每 intervalMs 执行。组件卸载自动清理。 */
export function usePoll(fn: () => void, intervalMs: number, deps: unknown[] = []): void {
  const fnRef = useRef(fn);
  fnRef.current = fn;
  useEffect(() => {
    fnRef.current();
    const id = setInterval(() => fnRef.current(), intervalMs);
    return () => clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
}

/**
 * 订阅 matchMedia 查询，返回当前是否匹配；query 变化时重新订阅。
 * SSR 或无 matchMedia 环境（如 jsdom）返回 false。
 */
export function useMediaQuery(query: string): boolean {
  // 惰性初始化：首帧即对齐真实匹配状态，避免移动端先渲染桌面布局再闪动
  const [matches, setMatches] = useState<boolean>(() =>
    typeof window !== 'undefined' && typeof window.matchMedia === 'function'
      ? window.matchMedia(query).matches
      : false,
  );

  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
      return;
    }
    const mql = window.matchMedia(query);
    // 同步当前值（订阅回调只在变化时触发，初始进入页面若已匹配需立即对齐）
    setMatches(mql.matches);
    const handleChange = (e: MediaQueryListEvent) => setMatches(e.matches);
    mql.addEventListener('change', handleChange);
    return () => mql.removeEventListener('change', handleChange);
  }, [query]);

  return matches;
}

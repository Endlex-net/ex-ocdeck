import { useEffect, useRef } from 'react';

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

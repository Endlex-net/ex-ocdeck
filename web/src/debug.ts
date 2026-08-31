/** 页面加载时读一次：localStorage `ocdeck.debug=1` 或 URL `debug=1`。 */
const debugEnabled = (() => {
  try {
    if (typeof localStorage !== 'undefined' && localStorage.getItem('ocdeck.debug') === '1') {
      return true;
    }
  } catch {
    /* private mode / 禁用 storage */
  }
  try {
    if (typeof window === 'undefined') return false;
    return new URLSearchParams(window.location.search).get('debug') === '1';
  } catch {
    return false;
  }
})();

export function isDebugEnabled(): boolean {
  return debugEnabled;
}

/** 幂等 performance.mark + console.debug；关闭时零副作用。 */
export function debugMark(name: string): void {
  if (!debugEnabled) return;
  if (performance.getEntriesByName(name).length) return;
  performance.mark(name);
  const short = name.startsWith('odterm:') ? name.slice('odterm:'.length) : name;
  console.debug('[odterm] ' + short, Math.round(performance.now()) + 'ms');
}

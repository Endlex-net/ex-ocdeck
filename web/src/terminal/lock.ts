/**
 * 锁定控制器（design D5/D8）。
 *
 * 仅负责锁定状态机 + overlay DOM；不持有 xterm 实例，不感知 React。
 * 状态唯一所有者是 TermSession，本模块由 TermSession 持有与调用；
 * TerminalView 通过 TermSession 的 onLockChange 订阅投影按钮。
 *
 * 锁定状态不持久化——每次连接建立一律 LOCKED（coarse），由 TermSession 决定何时调用 lock()。
 */
export interface LockController {
  /** 锁定 overlay 元素，手势层在 LOCKED 时将 touch 监听挂到此元素。 */
  readonly overlay: HTMLElement;
  lock(): void;
  unlock(): void;
  isLocked(): boolean;
  /** 订阅锁定状态变化，返回取消订阅函数。 */
  onChange(cb: (locked: boolean) => void): () => void;
  dispose(): void;
}

/**
 * 创建锁定控制器。
 *
 * @param host 锁定 overlay 挂载目标（TermSession 的 `.terminal-wrap`）。
 *             overlay 以 absolute inset:0 覆盖 host，pointer-events 由锁定状态切换。
 */
export function createLockController(host: HTMLElement): LockController {
  let locked = false;
  const listeners = new Set<(locked: boolean) => void>();

  const overlay = document.createElement('div');
  overlay.className = 'terminal-lock-overlay';
  // 默认隐藏；lock() 时切换为可见并拦截事件。
  overlay.style.display = 'none';
  // 防止 overlay 自身可滚动/被选中。
  overlay.setAttribute('aria-hidden', 'true');
  // 先不挂载，lock() 时再插入 DOM（避免 fine pointer 下空 div 影响 layout）。
  let mounted = false;

  function mount(): void {
    if (!mounted) {
      host.appendChild(overlay);
      mounted = true;
    }
  }

  function unmount(): void {
    if (mounted && overlay.parentNode === host) {
      host.removeChild(overlay);
    }
    mounted = false;
  }

  function notify(): void {
    for (const cb of listeners) cb(locked);
  }

  return {
    get overlay() {
      return overlay;
    },
    lock() {
      if (locked) return;
      // 先置标志（门禁依赖此状态），再操作 DOM。TermSession 在调用本函数后另行 term.blur()。
      locked = true;
      mount();
      overlay.style.display = '';
      notify();
    },
    unlock() {
      if (!locked) return;
      // 先移除 overlay（解除遮挡），TermSession 在调用本函数后另行 term.focus()。
      overlay.style.display = 'none';
      unmount();
      locked = false;
      notify();
    },
    isLocked() {
      return locked;
    },
    onChange(cb) {
      listeners.add(cb);
      return () => listeners.delete(cb);
    },
    dispose() {
      listeners.clear();
      unmount();
    },
  };
}

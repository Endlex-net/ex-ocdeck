/**
 * Session 锁定状态机协调逻辑（design D5/D8 测试缝）。
 *
 * 把 session.ts 中依赖顺序敏感的运行时协调（auth_ok 回锁顺序、pointer 动态变化、
 * lock/unlock 调用顺序）抽为独立协调器，注入依赖即可单测，不 import xterm/DOM。
 * session.ts 作为 thin adapter 持有并委托给本协调器，行为保持不变。
 *
 * 顺序契约（design D5）：
 * - lock()：先门禁置位（deps.lock）再 term.blur，防 blur 的 \x1b[O 在门禁生效前泄漏。
 * - unlock()（公共，按钮可信手势栈）：移除锁 + term.focus 唤起虚拟键盘。
 * - onAuthOk(coarse)：coarse 时 MUST 先 lock 再暴露 authed/connected（onAuthed 回调）。
 * - onPointerChange：转 coarse → lock + attach 手势层；转 fine → detach 手势层 +
 *   unlockSilently（MUST NOT focus，主动 focus 会注入 \x1b[I focus-in 序列）。
 */

export interface LockOrchestratorDeps {
  /** 锁定控制器置位（门禁生效 + overlay 挂载）。 */
  lock(): void;
  /** term.blur，lock 后调用防 focus-loss 序列泄漏。 */
  blur(): void;
  /** 系统级解锁：仅移除锁，不 focus。 */
  unlockSilently(): void;
  /** term.focus，仅公共 unlock（按钮可信手势栈）调用。 */
  focus(): void;
  /** attach 触控手势层（仅 coarse pointer 启用）。 */
  attachGestures(): void;
  /** dispose 触控手势层（fine pointer 无触控，无需接管）。 */
  detachGestures(): void;
}

export interface LockOrchestrator {
  /**
   * 锁定。顺序敏感：先门禁置位再 blur。
   * 防 blur 发出的 \x1b[O focus-loss 序列在门禁生效前泄漏。
   */
  lock(): void;
  /**
   * 解锁（公共，按钮可信手势栈）：移除锁 + focus 唤起虚拟键盘。
   * 必须在可信用户手势同步调用栈内调用（iOS Safari 限制）。
   */
  unlock(): void;
  /**
   * auth_ok 顺序：coarse 时先 lock，再暴露 authed/connected（onAuthed 回调）。
   * 非 coarse 时直接暴露。任何 WS 连接建立/auth_ok（含重连、Tab 切换）→ coarse 强制 LOCKED。
   */
  onAuthOk(coarse: boolean, onAuthed: () => void): void;
  /**
   * pointer 类型动态变化重评估（design D5）：
   * - 转 coarse：lock + attach 手势层。
   * - 转 fine：detach 手势层 + unlockSilently（MUST NOT focus）。
   * 仅外接键盘不改变 pointer，不触发本回调。
   */
  onPointerChange(matchesCoarse: boolean): void;
  dispose(): void;
}

export function createLockOrchestrator(deps: LockOrchestratorDeps): LockOrchestrator {
  let disposed = false;
  return {
    lock() {
      if (disposed) return;
      // 先门禁置位再 blur：blur 的 focus-loss 序列在门禁生效后被拦截。
      deps.lock();
      deps.blur();
    },
    unlock() {
      if (disposed) return;
      // 移除锁 + focus（可信手势栈内唤起虚拟键盘）。
      deps.unlockSilently();
      deps.focus();
    },
    onAuthOk(coarse, onAuthed) {
      if (disposed) return;
      // coarse 强制 LOCKED，必须先于 authed/connected 暴露与回调，防门禁未就绪窗口泄漏。
      if (coarse) {
        deps.lock();
        deps.blur();
      }
      onAuthed();
    },
    onPointerChange(matchesCoarse) {
      if (disposed) return;
      if (matchesCoarse) {
        deps.lock();
        deps.blur();
        deps.attachGestures();
      } else {
        deps.detachGestures();
        // 系统级解锁：仅移除锁，MUST NOT focus（防 \x1b[I 注入）。
        deps.unlockSilently();
      }
    },
    dispose() {
      disposed = true;
    },
  };
}
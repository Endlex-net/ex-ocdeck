// @vitest-environment jsdom
import { describe, expect, it } from 'vitest';
import { Terminal } from '@xterm/xterm';

/* ============================ IME 切换键双发射门槛（design D2 / tasks 3.3） ============================
 * jsdom 挂载真实 Terminal，合成 compositionstart/update + F7（keyCode 118，不在 6.0 排除集
 * {16,17,18,20,229} 内）keydown + compositionend 序列驱动真实 CompositionHelper。
 *
 * 红绿举证：同一 @xterm/xterm@6.0.0，唯一变量是 patches/@xterm__xterm@6.0.0.patch 是否应用——
 * 未打补丁时 keydown 立即 finalize 分支不写 _dataAlreadySent，随后 compositionend 的
 * deferred 分支按 0 偏移重复发射 commit 文本（上游 issue #5778，修复 PR #6140）；
 * 打补丁后 deferred 分支按已发送长度偏移跳过，onData 恰好收到 commit 文本一次。
 * 期望序列与上游 PR #6140 浏览器测试一致：[COMPOSITION, F7]。 */

/* jsdom 缺 xterm 6.0 open() 依赖的最小浏览器 API，装最小桩（同 cm-test-env 模式） */
if (typeof globalThis.ResizeObserver === 'undefined') {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver;
}
if (typeof globalThis.requestAnimationFrame === 'undefined') {
  globalThis.requestAnimationFrame = (cb: (t: number) => void) =>
    setTimeout(() => cb(Date.now()), 0) as unknown as number;
  globalThis.cancelAnimationFrame = (id: number) =>
    clearTimeout(id as unknown as ReturnType<typeof setTimeout>);
}
if (typeof window !== 'undefined' && typeof window.matchMedia !== 'function') {
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    writable: true,
    value: () => ({
      matches: false,
      media: '',
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }),
  });
}

/** 合成事件工厂：composition 事件（jsdom 无 CompositionEvent 构造器，用 Event + data 属性）。 */
function compositionEvent(type: string, data?: string): Event {
  const ev = new Event(type, { bubbles: true });
  if (data !== undefined) Object.defineProperty(ev, 'data', { value: data });
  return ev;
}

/** keydown 事件工厂：jsdom 构造器不支持 keyCode，用实例 getter 覆盖。 */
function keydownEvent(keyCode: number, key: string): KeyboardEvent {
  const ev = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
  Object.defineProperty(ev, 'keyCode', { get: () => keyCode });
  return ev;
}

/** 排空一个宏任务轮次（compositionupdate 内部 setTimeout(0) 写 _compositionPosition.end、
 *  compositionend 的 deferred finalize 依赖）。 */
function tick(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

/** 挂载真实 Terminal 并收集 onData。 */
function setup() {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const term = new Terminal({ cols: 80, rows: 24 });
  term.open(container);
  const data: string[] = [];
  term.onData((d) => data.push(d));
  return {
    term,
    textarea: term.textarea!,
    data,
    dispose() {
      term.dispose();
      container.remove();
    },
  };
}

describe('F7 中途结束 composition（#5778 补丁红绿门槛）', () => {
  it('onData 恰好收到 commit 文本一次，随后是 F7 序列（[COMPOSITION, F7]）', async () => {
    const f = setup();
    try {
      f.textarea.dispatchEvent(compositionEvent('compositionstart'));
      f.textarea.value = 'nihao';
      f.textarea.dispatchEvent(compositionEvent('compositionupdate', 'nihao'));
      await tick(); // compositionupdate 的 setTimeout(0) 记录 _compositionPosition.end
      // 非排除键 keydown（F7）→ 立即 finalize 分支发射 commit 文本（第 1 次），随后 F7 序列同步发出
      f.textarea.dispatchEvent(keydownEvent(118, 'F7'));
      expect(f.data).toEqual(['nihao', '\x1b[18~']);
      // 浏览器随后派发 compositionend → deferred finalize 分支
      f.textarea.dispatchEvent(compositionEvent('compositionend'));
      await tick();
      // 补丁后 deferred 分支按 _dataAlreadySent 偏移跳过已发送文本 → 不再重复；
      // 未打补丁时此处追加第二个 'nihao'（双发射）→ 断言失败
      expect(f.data).toEqual(['nihao', '\x1b[18~']);
    } finally {
      f.dispose();
    }
  });
});

describe('CapsLock 切换路径（keyCode 20，6.0 原生排除集，不承担补丁红绿举证）', () => {
  it('CapsLock 不触发 finalize，composition 正常结束恰好发送一次', async () => {
    const f = setup();
    try {
      f.textarea.dispatchEvent(compositionEvent('compositionstart'));
      f.textarea.value = 'nihao';
      f.textarea.dispatchEvent(compositionEvent('compositionupdate', 'nihao'));
      await tick();
      // CapsLock 在 6.0 keydown 排除集内 → 不 finalize、不产生终端输入
      f.textarea.dispatchEvent(keydownEvent(20, 'CapsLock'));
      expect(f.data).toEqual([]);
      // composition 正常结束 → deferred finalize 发射一次 commit 文本
      f.textarea.dispatchEvent(compositionEvent('compositionend'));
      await tick();
      expect(f.data).toEqual(['nihao']);
    } finally {
      f.dispose();
    }
  });
});

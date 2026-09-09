// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { TerminalView } from '../terminal/TerminalView';
import { FONT_FAMILY_KEY, TERM_PREFS_CHANGED, resolveFontFamily } from '../terminal/preferences';
import { mount, stubMatchMedia } from './cm-test-env';
import { stubStorage } from './dom-test-utils';

/**
 * ora-15 P3：真实 TermSession + 真实 preferences + mock xterm/WS 的偏好通道组件路径
 * （补 terminal-fonts-view.test.tsx 整体 mock TermSession 造成的契约缺口）：
 * - 派发 TERM_PREFS_CHANGED 后断言 `term.options.fontFamily` 为 resolveFontFamily 完整结果
 *   （emoji 三族 + Symbols 绝对末尾），存储零写入；
 * - 实际触发所声称的重连入口（TerminalView overlay「重新连接」按钮 → session.reconnect()，
 *   真实新建 WS），终端实例身份保持（不重建）。
 */

const harness = vi.hoisted(() => {
  const terms: FakeTerm[] = [];
  const wsInstances: FakeWS[] = [];
  class FakeTerm {
    loadAddon = vi.fn();
    open = vi.fn();
    onData = vi.fn();
    onBinary = vi.fn();
    write = vi.fn();
    dispose = vi.fn();
    blur = vi.fn();
    focus = vi.fn();
    input = vi.fn();
    refresh = vi.fn();
    cols = 80;
    rows = 24;
    element = null;
    textarea = { addEventListener: vi.fn(), removeEventListener: vi.fn() } as unknown as HTMLTextAreaElement;
    options: Record<string, unknown> = {};
    modes = { mouseTrackingMode: 'none' as const };
    buffer = { active: { type: 'normal' as const } };
    parser = { registerOscHandler: vi.fn(() => ({ dispose: vi.fn() })) };
    attachCustomKeyEventHandler = vi.fn();
    constructor() {
      terms.push(this);
    }
  }
  class FakeWS {
    static instances = wsInstances;
    onopen: (() => void) | null = null;
    onmessage: ((ev: MessageEvent) => void) | null = null;
    onclose: ((ev: CloseEvent) => void) | null = null;
    onerror: (() => void) | null = null;
    readyState = 0; // CONNECTING
    binaryType = 'arraybuffer';
    send = vi.fn();
    close = vi.fn();
    constructor() {
      wsInstances.push(this);
    }
  }
  return { terms, wsInstances, FakeTerm, FakeWS };
});

vi.mock('@xterm/xterm', () => ({ Terminal: harness.FakeTerm }));
// webgl 在 jsdom 不可用：构造即抛 → session 捕获置 null（canvas→DOM renderer 回退路径）
vi.mock('@xterm/addon-webgl', () => ({
  WebglAddon: class {
    constructor() {
      throw new Error('WebGL unavailable');
    }
  },
}));

const { store, writes } = stubStorage();

beforeEach(() => {
  stubMatchMedia(false);
  store.clear();
  writes.count = 0;
  harness.terms.length = 0;
  harness.wsInstances.length = 0;
  vi.stubGlobal('WebSocket', harness.FakeWS);
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('偏好通道（真实 TermSession + 真实 preferences，mock xterm/WS）', () => {
  it('TERM_PREFS_CHANGED → term.options.fontFamily 为完整有效栈；存储零写入；重连按钮走真实 reconnect 且实例不重建', () => {
    store.set(FONT_FAMILY_KEY, 'Fira Code');
    const view = mount(<TerminalView wsPath="/ws/terminal/t1" active />);
    // active → 真实 connect：建立第一根 WS
    expect(harness.terms).toHaveLength(1);
    expect(harness.wsInstances).toHaveLength(1);

    // 服务端正常关闭（1000）→ 状态 closed → overlay 出现「重新连接」按钮
    act(() => {
      harness.wsInstances[0].onclose?.(new CloseEvent('close', { code: 1000 }));
    });
    const reconnectBtn = [...view.container.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '重新连接',
    );
    expect(reconnectBtn).toBeTruthy();

    // 实际触发所声称的重连入口：真实 session.reconnect() → 新建 WS（同一终端实例）
    act(() => reconnectBtn!.click());
    expect(harness.terms).toHaveLength(1); // 实例身份保持（不重建）
    expect(harness.wsInstances).toHaveLength(2); // 重连 = 真实新建 WS

    // 派发偏好事件：真实 applyPreferences → resolveFontFamily 完整结果写入 term.options
    act(() => {
      window.dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED));
    });
    expect(harness.terms[0].options.fontFamily).toBe(
      resolveFontFamily({ fontFamily: 'Fira Code' }),
    );
    expect(harness.terms[0].options.fontFamily).toBe(
      'Fira Code, "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
    // 有效栈变换不写回偏好存储
    expect(writes.count).toBe(0);
    expect(store.get(FONT_FAMILY_KEY)).toBe('Fira Code');

    view.unmount();
    expect(harness.terms[0].dispose).toHaveBeenCalledTimes(1);
  });
});

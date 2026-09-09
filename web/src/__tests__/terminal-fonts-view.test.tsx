// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { TerminalView } from '../terminal/TerminalView';
import { FONT_FAMILY_KEY, TERM_PREFS_CHANGED } from '../terminal/preferences';
import { mount, rerender, stubMatchMedia } from './cm-test-env';
import { stubStorage } from './dom-test-utils';

/**
 * 偏好即时更新通道组件验收（terminal-links-emoji-icons 4.4，design 测试策略 N7 末行）：
 * `TERM_PREFS_CHANGED → TerminalView apply 回调 → session.applyPreferences(loadTermPrefs())`：
 * - 同页 CustomEvent 与跨标签页 storage 事件均触发 applyPreferences，prefs 为新鲜读取；
 * - 有效栈更新通道不写回偏好存储（store 内容与写入次数不变）；
 * - 隐藏后激活（active 翻转）与重连场景终端实例身份保持（TermSession 构造恰一次，不重建）。
 *
 * 策略：clipboard-terminal-view.test.tsx 同款 FakeTermSession mock；preferences/clipboard 真实实现。
 */

const sessionMock = vi.hoisted(() => {
  const instances: FakeTermSession[] = [];
  class FakeTermSession {
    static instances = instances;
    connect = vi.fn();
    disconnect = vi.fn();
    dispose = vi.fn();
    applyPreferences = vi.fn();
    focus = vi.fn();
    lock = vi.fn();
    unlock = vi.fn();
    constructor(
      _host: HTMLElement,
      _wrap: HTMLElement,
      _wsPath: string,
      _onState: (s: string) => void,
      _onClipboardWrite?: (text: string) => void,
    ) {
      instances.push(this);
    }
    isLocked(): boolean {
      return false;
    }
    onLockChange(): () => boolean {
      return () => false;
    }
  }
  return FakeTermSession;
});

vi.mock('../terminal/session', () => ({ TermSession: sessionMock }));

const { store, writes } = stubStorage();

function mountView(active: boolean) {
  return mount(<TerminalView wsPath="/ws/terminal/t1" active={active} />);
}

beforeEach(() => {
  stubMatchMedia(false);
  store.clear();
  writes.count = 0;
  sessionMock.instances.length = 0; // 清跨测试实例累积
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('偏好即时更新通道（TERM_PREFS_CHANGED → applyPreferences → resolveFontFamily）', () => {
  it('同页 CustomEvent：已挂载终端按新偏好 applyPreferences（fontFamily 为存储值），不写回偏好存储', async () => {
    store.set(FONT_FAMILY_KEY, '"Fira Code"');
    const view = mountView(true);
    expect(sessionMock.instances).toHaveLength(1);
    const session = sessionMock.instances[0];

    act(() => {
      window.dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED));
    });
    expect(session.applyPreferences).toHaveBeenCalledTimes(1);
    expect(session.applyPreferences).toHaveBeenCalledWith({ fontFamily: '"Fira Code"' });
    expect(writes.count).toBe(0); // 变换只作用于运行时有效栈，MUST NOT 写回存储
    expect(store.get(FONT_FAMILY_KEY)).toBe('"Fira Code"');
    view.unmount();
  });

  it('跨标签页 storage 事件同样触发 applyPreferences（新鲜读取，非缓存）', () => {
    const view = mountView(true);
    const session = sessionMock.instances[0];
    store.set(FONT_FAMILY_KEY, 'Fira Code');

    act(() => {
      window.dispatchEvent(new StorageEvent('storage'));
    });
    expect(session.applyPreferences).toHaveBeenCalledWith({ fontFamily: 'Fira Code' });
    expect(writes.count).toBe(0);
    view.unmount();
  });

  it('隐藏后激活与重连场景终端实例身份保持（不重建）：prefs change 前后构造恰一次', () => {
    const view = mountView(true);
    expect(sessionMock.instances).toHaveLength(1);
    const session = sessionMock.instances[0];

    // 隐藏 → 激活（active 翻转：disconnect → connect，同一实例）
    rerender(view.root, <TerminalView wsPath="/ws/terminal/t1" active={false} />);
    rerender(view.root, <TerminalView wsPath="/ws/terminal/t1" active={true} />);
    expect(sessionMock.instances).toHaveLength(1);
    expect(session.connect).toHaveBeenCalledTimes(2); // 初次 + 重新激活
    expect(session.disconnect).toHaveBeenCalledTimes(1);

    // 重连语义（reconnect 由设置 overlay 按钮驱动，连接层不重建）+ prefs change
    store.set(FONT_FAMILY_KEY, 'Fira Code');
    act(() => {
      window.dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED));
    });
    expect(sessionMock.instances).toHaveLength(1); // 实例身份保持（不重建）
    expect(session.applyPreferences).toHaveBeenCalled();
    expect(session.dispose).not.toHaveBeenCalled();
    view.unmount();
    expect(session.dispose).toHaveBeenCalledTimes(1); // 卸载时恰好释放一次
  });
});

// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { TerminalView } from '../terminal/TerminalView';
import { CLIPBOARD_POLICY_KEY } from '../terminal/clipboard';
import { TERM_PREFS_CHANGED } from '../terminal/preferences';
import { mount, stubMatchMedia } from './cm-test-env';

/* ============================ TerminalView 远程剪贴板 UI（OSC 52 lane） ============================
 * session mock 成假 TermSession（捕获第 5 参 onClipboardWrite，emitCopy 驱动）；
 * clipboard.ts 为真实实现（localStorage 桩）。覆盖：ask/auto/off 策略路径、Esc 关闭、
 * React 转义预览、Clipboard API 拒绝 → 回退 popover、execCommand 降级、设置联动关 popover。 */

const sessionMock = vi.hoisted(() => {
  const instances: FakeTermSession[] = [];
  class FakeTermSession {
    static instances = instances;
    connect = vi.fn();
    disconnect = vi.fn();
    dispose = vi.fn();
    applyPreferences = vi.fn();
    lock = vi.fn();
    unlock = vi.fn();
    onClipboardWrite: ((text: string) => void) | undefined;
    constructor(
      _host: HTMLElement,
      _wrap: HTMLElement,
      _wsPath: string,
      _onState: (s: string) => void,
      onClipboardWrite?: (text: string) => void,
    ) {
      this.onClipboardWrite = onClipboardWrite;
      instances.push(this);
    }
    isLocked(): boolean {
      return false;
    }
    onLockChange(): () => boolean {
      return () => false;
    }
    emitCopy(text: string): void {
      this.onClipboardWrite?.(text);
    }
  }
  return FakeTermSession;
});

vi.mock('../terminal/session', () => ({ TermSession: sessionMock }));

const store = new Map<string, string>();
let savedStorage: Storage | undefined;
let savedExecCommand: PropertyDescriptor | undefined;

function setClipboardApi(writeText?: (t: string) => Promise<void>): void {
  if (writeText) {
    Object.defineProperty(window.navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    });
  } else {
    // jsdom navigator 无 clipboard；确保「无 API」路径干净
    delete (window.navigator as { clipboard?: unknown }).clipboard;
  }
}

function stubExecCommand(impl: () => boolean): ReturnType<typeof vi.fn> {
  savedExecCommand = Object.getOwnPropertyDescriptor(document, 'execCommand');
  const spy = vi.fn(impl);
  Object.defineProperty(document, 'execCommand', { value: spy, configurable: true });
  return spy;
}

function mountTerminal() {
  return mount(<TerminalView wsPath="/ws/x" active={false} />);
}

function popover(container: HTMLElement): HTMLElement | null {
  return container.querySelector<HTMLElement>('.terminal-clipboard-popover');
}

function toast(container: HTMLElement): HTMLElement | null {
  return container.querySelector<HTMLElement>('.od-toast');
}

async function flushUI() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

beforeEach(() => {
  store.clear();
  sessionMock.instances.length = 0;
  savedStorage = globalThis.localStorage;
  (globalThis as { localStorage: Storage }).localStorage = {
    getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
    setItem: (k: string, v: string) => {
      store.set(k, String(v));
    },
    removeItem: (k: string) => {
      store.delete(k);
    },
    clear: () => {
      store.clear();
    },
    key: (i: number) => Array.from(store.keys())[i] ?? null,
    get length() {
      return store.size;
    },
  } as Storage;
  stubMatchMedia(false);
  setClipboardApi(() => Promise.resolve());
});

afterEach(() => {
  (globalThis as { localStorage: Storage }).localStorage = savedStorage!;
  if (savedExecCommand) Object.defineProperty(document, 'execCommand', savedExecCommand);
  else Reflect.deleteProperty(document, 'execCommand');
  vi.restoreAllMocks();
});

describe('TerminalView 远程剪贴板策略路径', () => {
  it('ask（默认）：弹确认 popover，预览经 React 转义（<img> 为纯文本），不自动写入', async () => {
    const { container, unmount } = mountTerminal();
    const session = sessionMock.instances[0];
    await act(async () => {
      session.emitCopy('<img src=x onerror=alert(1)>');
    });

    const pop = popover(container);
    expect(pop).not.toBeNull();
    expect(pop!.querySelector('.terminal-clipboard-text')!.textContent).toBe(
      '<img src=x onerror=alert(1)>',
    );
    expect(container.querySelector('img')).toBeNull(); // 无 HTML 注入
    expect(toast(container)).toBeNull();
    unmount();
  });

  it('ask：点复制 → Clipboard API 写入 + toast + popover 关闭', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    setClipboardApi(writeText);
    const { container, unmount } = mountTerminal();
    await act(async () => {
      sessionMock.instances[0].emitCopy('hello');
    });
    await act(async () => {
      popover(container)!.querySelector<HTMLButtonElement>('.terminal-clipboard-actions button')!.click();
    });
    await flushUI();

    expect(writeText).toHaveBeenCalledWith('hello');
    expect(popover(container)).toBeNull();
    expect(toast(container)).not.toBeNull();
    expect(toast(container)!.textContent).toContain('已复制');
    unmount();
  });

  it('Clipboard API 拒绝 → popover 保留（文本仍可手动选中），无 toast', async () => {
    setClipboardApi(() => Promise.reject(new Error('denied')));
    const { container, unmount } = mountTerminal();
    await act(async () => {
      sessionMock.instances[0].emitCopy('secret');
    });
    await act(async () => {
      popover(container)!.querySelector<HTMLButtonElement>('.terminal-clipboard-actions button')!.click();
    });
    await flushUI();

    expect(popover(container)).not.toBeNull();
    expect(toast(container)).toBeNull();
    unmount();
  });

  it('auto：直接写入 + toast，不弹 popover', async () => {
    store.set(CLIPBOARD_POLICY_KEY, 'auto');
    const writeText = vi.fn(() => Promise.resolve());
    setClipboardApi(writeText);
    const { container, unmount } = mountTerminal();
    await act(async () => {
      sessionMock.instances[0].emitCopy('auto-copy');
    });
    await flushUI();

    expect(writeText).toHaveBeenCalledWith('auto-copy');
    expect(popover(container)).toBeNull();
    expect(toast(container)).not.toBeNull();
    unmount();
  });

  it('auto + 写入被拒 → 回退 popover', async () => {
    store.set(CLIPBOARD_POLICY_KEY, 'auto');
    setClipboardApi(() => Promise.reject(new Error('denied')));
    const { container, unmount } = mountTerminal();
    await act(async () => {
      sessionMock.instances[0].emitCopy('auto-denied');
    });
    await flushUI();

    expect(popover(container)).not.toBeNull();
    expect(toast(container)).toBeNull();
    unmount();
  });

  it('off：不写入、不弹任何剪贴板 UI', async () => {
    store.set(CLIPBOARD_POLICY_KEY, 'off');
    const writeText = vi.fn(() => Promise.resolve());
    setClipboardApi(writeText);
    const { container, unmount } = mountTerminal();
    await act(async () => {
      sessionMock.instances[0].emitCopy('ignored');
    });
    await flushUI();

    expect(writeText).not.toHaveBeenCalled();
    expect(popover(container)).toBeNull();
    expect(toast(container)).toBeNull();
    unmount();
  });

  it('Esc 关闭确认 popover（不改策略）', async () => {
    const { container, unmount } = mountTerminal();
    await act(async () => {
      sessionMock.instances[0].emitCopy('esc-me');
    });
    expect(popover(container)).not.toBeNull();
    await act(async () => {
      window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    });
    expect(popover(container)).toBeNull();
    expect(store.get(CLIPBOARD_POLICY_KEY)).toBeUndefined();
    unmount();
  });

  it('无 Clipboard API（非安全上下文）→ 复制走 execCommand 降级', async () => {
    setClipboardApi(undefined);
    const execSpy = stubExecCommand(() => true);
    const { container, unmount } = mountTerminal();
    await act(async () => {
      sessionMock.instances[0].emitCopy('legacy');
    });
    await act(async () => {
      popover(container)!.querySelector<HTMLButtonElement>('.terminal-clipboard-actions button')!.click();
    });
    await flushUI();

    expect(execSpy).toHaveBeenCalledWith('copy');
    expect(popover(container)).toBeNull();
    expect(toast(container)).not.toBeNull();
    unmount();
  });

  it('始终允许：持久化 auto + 立即写入 + 关闭 popover', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    setClipboardApi(writeText);
    const { container, unmount } = mountTerminal();
    await act(async () => {
      sessionMock.instances[0].emitCopy('grant');
    });
    const buttons = popover(container)!.querySelectorAll<HTMLButtonElement>('.terminal-clipboard-actions button');
    await act(async () => {
      buttons[1].click(); // 复制 / 始终允许 / 关闭
    });
    await flushUI();

    expect(store.get(CLIPBOARD_POLICY_KEY)).toBe('auto');
    expect(writeText).toHaveBeenCalledWith('grant');
    expect(popover(container)).toBeNull();
    unmount();
  });

  it('off（TERM_PREFS_CHANGED）：在途确认 popover 立即关闭，新的不同内容也不再弹/不再写', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    setClipboardApi(writeText);
    const { container, unmount } = mountTerminal();
    await act(async () => {
      sessionMock.instances[0].emitCopy('first');
    });
    expect(popover(container)).not.toBeNull();

    store.set(CLIPBOARD_POLICY_KEY, 'off');
    await act(async () => {
      window.dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED));
    });
    expect(popover(container)).toBeNull();

    // 换不同内容再请求：必须仍被 off 挡住（同内容会被去重挡住，不能作为反证）
    await act(async () => {
      sessionMock.instances[0].emitCopy('second-distinct');
    });
    expect(popover(container)).toBeNull();
    expect(writeText).not.toHaveBeenCalled();
    unmount();
  });

  it('in-flight + 排队期间切 off：排队项不启动，in-flight 迟到失败不重开 popover', async () => {
    store.set(CLIPBOARD_POLICY_KEY, 'auto');
    let releaseA!: (ok: boolean) => void;
    const gateA = new Promise<boolean>((resolve) => {
      releaseA = resolve;
    });
    const writeText = vi.fn((t: string) => {
      if (t === 'A1') return gateA.then((ok) => (ok ? undefined : Promise.reject(new Error('denied'))));
      return Promise.resolve();
    });
    setClipboardApi(writeText);
    const { container, unmount } = mountTerminal();

    await act(async () => {
      sessionMock.instances[0].emitCopy('A1'); // 启动 in-flight
      sessionMock.instances[0].emitCopy('B2'); // 排队
    });
    expect(writeText).toHaveBeenCalledTimes(1); // 仅 A 启动，B 排队未启动

    store.set(CLIPBOARD_POLICY_KEY, 'off');
    await act(async () => {
      window.dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED));
    });
    await flushUI();

    releaseA(false); // A 迟到失败（off 之后才结算）
    await flushUI();

    expect(writeText).toHaveBeenCalledTimes(1); // B 从未启动
    expect(popover(container)).toBeNull(); // A 迟到失败不得重开 popover
    expect(toast(container)).toBeNull();
    unmount();
  });
});

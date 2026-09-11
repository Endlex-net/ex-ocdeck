// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { TerminalView } from '../terminal/TerminalView';
import { mount, stubMatchMedia } from './cm-test-env';

/* ==================== terminal-file-paste-drop 5.3：TerminalView 文件投递 UI 骨架 ====================
 * session mock 成假 TermSession（扩展文件投递端口），api.uploadAttachment mock。
 * 覆盖：仅 TUI 挂载入口（shell 不挂）、paste capture 先于目标阶段监听器 + 纯文本不拦截、
 * drop 防导航 + dragenter/leave 防闪烁 + 目录分项拒绝、能力降级（无 connId）提示不上传、
 * 失败项重试（重新上传）。 */

const UPLOAD_A = 'a'.repeat(32);
const CONN_ID = '11111111-1111-4111-8111-111111111111';

interface DeliverResultLike {
  uploadId: string;
  ok: boolean;
  error?: string;
}

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
    // ---- 文件投递端口（fake）----
    connId: string | null = '11111111-1111-4111-8111-111111111111';
    gateOpen = true;
    sentDelivers: string[] = [];
    private deliverResultCb: ((r: { uploadId: string; ok: boolean; error?: string }) => void) | null = null;
    private connCb: ((ev: { type: string; connId?: string | null }) => void) | null = null;
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
    getConnId(): string | null {
      return this.connId;
    }
    fileGateOpen(): boolean {
      return this.gateOpen;
    }
    fileGateMessage(): string {
      return '终端未连接，请连接后重试';
    }
    sendDeliver(uploadId: string): boolean {
      if (!this.gateOpen) return false;
      this.sentDelivers.push(uploadId);
      return true;
    }
    onConnEvent(cb: (ev: { type: string; connId?: string | null }) => void): () => void {
      this.connCb = cb;
      return () => {
        this.connCb = null;
      };
    }
    /** 测试驱动：模拟连接断开（真实由 WS close 驱动）。 */
    emitClosed(): void {
      this.connCb?.({ type: 'closed' });
    }
    onDeliverResult(cb: (r: { uploadId: string; ok: boolean; error?: string }) => void): () => void {
      this.deliverResultCb = cb;
      return () => {
        this.deliverResultCb = null;
      };
    }
    emitResult(r: DeliverResultLike): void {
      this.deliverResultCb?.(r);
    }
  }
  return FakeTermSession;
});

vi.mock('../terminal/session', () => ({ TermSession: sessionMock }));

const apiMock = vi.hoisted(() => ({
  uploadAttachment: vi.fn(),
}));
vi.mock('../api', async () => {
  const actual = await vi.importActual<typeof import('../api')>('../api');
  return { ...actual, api: { ...actual.api, uploadAttachment: apiMock.uploadAttachment } };
});

const store = new Map<string, string>();
let savedStorage: Storage | undefined;

function mountTerminal(wsPath: string) {
  return mount(<TerminalView wsPath={wsPath} active={false} />);
}

function host(container: HTMLElement): HTMLElement {
  return container.querySelector<HTMLElement>('.terminal-host')!;
}

function wrap(container: HTMLElement): HTMLElement {
  return container.querySelector<HTMLElement>('.terminal-wrap')!;
}

function filePanel(container: HTMLElement): HTMLElement | null {
  return container.querySelector<HTMLElement>('.terminal-file-panel');
}

function fileStatuses(container: HTMLElement): string[] {
  return [...container.querySelectorAll('.terminal-file-item .terminal-file-status')].map(
    (el) => el.textContent ?? '',
  );
}

async function flushUI() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

function pasteEvent(files: File[]): Event {
  const ev = new Event('paste', { bubbles: true, cancelable: true });
  Object.defineProperty(ev, 'clipboardData', {
    value: { items: files.map((f) => ({ kind: 'file', getAsFile: () => f })), files: [] },
  });
  return ev;
}

function dropEvent(dt: unknown): Event {
  const ev = new Event('drop', { bubbles: true, cancelable: true });
  Object.defineProperty(ev, 'dataTransfer', { value: dt });
  return ev;
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
  apiMock.uploadAttachment.mockReset();
  apiMock.uploadAttachment.mockResolvedValue({ uploadId: UPLOAD_A });
});

afterEach(() => {
  (globalThis as { localStorage: Storage }).localStorage = savedStorage!;
  vi.restoreAllMocks();
});

describe('TerminalView 文件投递入口挂载（仅 TUI）', () => {
  it('TUI 实例：挂载「选择文件」按钮与隐藏 file input', () => {
    const { container, unmount } = mountTerminal('/ws/terminal/task-1');
    expect(container.querySelector('.terminal-file-bar button')!.textContent).toBe('选择文件');
    expect(container.querySelector('input[type="file"]')).not.toBeNull();
    unmount();
  });

  it('shell 实例：不挂任何文件投递入口', () => {
    const { container, unmount } = mountTerminal('/ws/terminal/shell/s1');
    expect(container.querySelector('.terminal-file-bar')).toBeNull();
    expect(container.querySelector('input[type="file"]')).toBeNull();
    unmount();
  });
});

describe('paste 捕获（capture 先于 textarea 阶段、纯文本不拦截）', () => {
  it('paste 文件：capture 阶段拦截（stopPropagation 后目标阶段不再收到）→ 上传 + deliver + 已发送到终端', async () => {
    const { container, unmount } = mountTerminal('/ws/terminal/task-1');
    const session = sessionMock.instances[0];
    const h = host(container);
    const child = document.createElement('div'); // 模拟 xterm textarea 所在的目标层级
    h.appendChild(child);
    let textareaPhaseSeen = false;
    child.addEventListener('paste', () => {
      textareaPhaseSeen = true;
    });

    const file = new File(['x'], 'shot.png', { type: 'image/png' });
    const ev = pasteEvent([file]);
    await act(async () => {
      child.dispatchEvent(ev);
    });
    // capture listener 先拦截并 stopPropagation：目标阶段监听器（xterm textarea 同理）不再收到事件
    expect(textareaPhaseSeen).toBe(false);
    expect(ev.defaultPrevented).toBe(true);
    expect(apiMock.uploadAttachment).toHaveBeenCalledWith('task-1', file, CONN_ID);
    expect(filePanel(container)).not.toBeNull();

    await flushUI();
    expect(session.sentDelivers).toEqual([UPLOAD_A]);
    expect(fileStatuses(container)).toEqual(['等待回执']);

    await act(async () => {
      session.emitResult({ uploadId: UPLOAD_A, ok: true });
    });
    expect(fileStatuses(container)).toEqual(['已发送到终端']);
    unmount();
  });

  it('paste 纯文本：不拦截（defaultPrevented false）、不上传、无浮层', async () => {
    const { container, unmount } = mountTerminal('/ws/terminal/task-1');
    const ev = new Event('paste', { bubbles: true, cancelable: true });
    Object.defineProperty(ev, 'clipboardData', {
      value: { items: [{ kind: 'string', getAsFile: () => null }], files: [] },
    });
    await act(async () => {
      host(container).dispatchEvent(ev);
    });
    expect(ev.defaultPrevented).toBe(false);
    expect(apiMock.uploadAttachment).not.toHaveBeenCalled();
    expect(filePanel(container)).toBeNull();
    unmount();
  });
});

describe('drop 捕获（防导航、防闪烁、目录分项拒绝）', () => {
  it('dragenter/leave 计数驱动悬停态；dragover 允许投放并阻止浏览器导航', async () => {
    const { container, unmount } = mountTerminal('/ws/terminal/task-1');
    const h = host(container);
    const dragover = new Event('dragover', { bubbles: true, cancelable: true });
    Object.defineProperty(dragover, 'dataTransfer', { value: { dropEffect: 'none' } });

    await act(async () => {
      h.dispatchEvent(new Event('dragenter', { bubbles: true, cancelable: true }));
    });
    expect(wrap(container).className).toContain('terminal-drop-hover');

    await act(async () => {
      h.dispatchEvent(dragover);
    });
    expect(dragover.defaultPrevented).toBe(true);
    expect(
      (dragover as unknown as { dataTransfer: { dropEffect: string } }).dataTransfer.dropEffect,
    ).toBe('copy');

    await act(async () => {
      h.dispatchEvent(new Event('dragleave'));
    });
    expect(wrap(container).className).not.toContain('terminal-drop-hover'); // 计数归零，无闪烁残留

    // 嵌套子元素 dragenter/leave（计数 2→1）不提前熄灭悬停态
    const child = document.createElement('div');
    h.appendChild(child);
    await act(async () => {
      h.dispatchEvent(new Event('dragenter', { bubbles: true, cancelable: true }));
      child.dispatchEvent(new Event('dragenter', { bubbles: true, cancelable: true }));
    });
    await act(async () => {
      child.dispatchEvent(new Event('dragleave', { bubbles: true }));
    });
    expect(wrap(container).className).toContain('terminal-drop-hover');
    unmount();
  });

  it('drop：目录项分项拒绝（不支持目录）、普通文件继续上传；drop 阻止浏览器导航', async () => {
    const { container, unmount } = mountTerminal('/ws/terminal/task-1');
    const file = new File(['x'], 'ok.png', { type: 'image/png' });
    const ev = dropEvent({
      items: [
        {
          kind: 'file',
          getAsFile: () => null,
          webkitGetAsEntry: () => ({ isDirectory: true, name: 'mydir' }),
        },
        {
          kind: 'file',
          getAsFile: () => file,
          webkitGetAsEntry: () => ({ isDirectory: false, name: 'ok.png' }),
        },
      ],
      files: [],
    });
    await act(async () => {
      host(container).dispatchEvent(ev);
    });
    expect(ev.defaultPrevented).toBe(true); // 阻止浏览器导航
    expect(apiMock.uploadAttachment).toHaveBeenCalledTimes(1); // 不遍历目录，仅普通文件

    const names = [...container.querySelectorAll('.terminal-file-item .terminal-file-name')].map(
      (el) => el.textContent ?? '',
    );
    expect(names).toEqual(['mydir', 'ok.png']);
    // 目录项明确失败；普通文件在 act 内已完成上传+deliver（mock 即时 resolve）
    expect(fileStatuses(container)).toEqual(['明确失败', '等待回执']);
    const detail = container.querySelector('.terminal-file-item .terminal-file-detail');
    expect(detail!.textContent).toBe('不支持目录');
    unmount();
  });
});

describe('能力降级（无 connId）与重试', () => {
  it('auth_ok 无 connId：paste 文件 → 提示「当前 server 版本不支持文件投递」，不上传', async () => {
    const { container, unmount } = mountTerminal('/ws/terminal/task-1');
    sessionMock.instances[0].connId = null;
    const file = new File(['x'], 'shot.png', { type: 'image/png' });
    await act(async () => {
      host(container).dispatchEvent(pasteEvent([file]));
    });
    expect(apiMock.uploadAttachment).not.toHaveBeenCalled();
    expect(container.querySelector('.terminal-file-notice')!.textContent).toBe(
      '当前 server 版本不支持文件投递',
    );
    expect(fileStatuses(container)).toEqual([]);
    unmount();
  });

  it('上传失败项点「重试」→ 重新上传（新 uploadId）→ deliver', async () => {
    const { container, unmount } = mountTerminal('/ws/terminal/task-1');
    const session = sessionMock.instances[0];
    const file = new File(['x'], 'shot.png', { type: 'image/png' });
    apiMock.uploadAttachment.mockRejectedValueOnce(new Error('boom'));
    await act(async () => {
      host(container).dispatchEvent(pasteEvent([file]));
    });
    await flushUI();
    expect(fileStatuses(container)).toEqual(['明确失败']);

    const retryBtn = container.querySelector<HTMLButtonElement>('.terminal-file-item button')!;
    expect(retryBtn.textContent).toBe('重试');
    await act(async () => {
      retryBtn.click();
    });
    await flushUI();
    expect(apiMock.uploadAttachment).toHaveBeenCalledTimes(2); // 重试 = 重新上传
    expect(session.sentDelivers).toEqual([UPLOAD_A]);
    expect(fileStatuses(container)).toEqual(['等待回执']);
    unmount();
  });
});

describe('shell 实例零文件入口（W1）', () => {
  it('shell：paste/dragover/drop 全部不被拦截、不改写 dropEffect、无任何文件行为', async () => {
    const { container, unmount } = mountTerminal('/ws/terminal/shell/s1');
    const h = host(container);
    const file = new File(['x'], 's.png', { type: 'image/png' });
    const paste = pasteEvent([file]);
    const dragover = new Event('dragover', { bubbles: true, cancelable: true });
    Object.defineProperty(dragover, 'dataTransfer', { value: { dropEffect: 'none' } });
    const drop = dropEvent({ items: [{ kind: 'file', getAsFile: () => file }], files: [] });

    await act(async () => {
      h.dispatchEvent(paste);
      h.dispatchEvent(dragover);
      h.dispatchEvent(new Event('dragenter', { bubbles: true, cancelable: true }));
      h.dispatchEvent(drop);
    });
    expect(paste.defaultPrevented).toBe(false);
    expect(dragover.defaultPrevented).toBe(false);
    // dropEffect 未被改写为 copy（dragover listener 未注册）
    expect(
      (dragover as unknown as { dataTransfer: { dropEffect: string } }).dataTransfer.dropEffect,
    ).toBe('none');
    expect(drop.defaultPrevented).toBe(false);
    expect(apiMock.uploadAttachment).not.toHaveBeenCalled();
    expect(filePanel(container)).toBeNull();
    expect(wrap(container).className).not.toContain('terminal-drop-hover');
    unmount();
  });
});

describe('文件选择器取消路径（W4）', () => {
  beforeEach(() => {
    // 避免 jsdom 对 file input click 的 not-implemented 噪音（openFilePicker 经 input.click()）
    vi.spyOn(HTMLInputElement.prototype, 'click').mockImplementation(() => {});
  });

  it('failed 项（file 不可用）重试 → input cancel → 项保持失败、队列暂停不变、绑定解除；卸载后再 cancel 无作用', async () => {
    const { container, unmount } = mountTerminal('/ws/terminal/task-1');
    const session = sessionMock.instances[0];

    // ① 制造队列暂停：文件 → 等待回执 → 断线 → unknown + paused
    const f = new File(['x'], 'a.png', { type: 'image/png' });
    await act(async () => {
      host(container).dispatchEvent(pasteEvent([f]));
    });
    await flushUI();
    await act(async () => {
      session.emitClosed();
    });
    expect(fileStatuses(container)).toEqual(['结果未知']);
    expect(container.querySelector('.terminal-file-panel-title')!.textContent).toContain('队列已暂停');

    // ② 目录拒绝项（file=null 的 failed 项）→ 重试 → 选择器在途，其他项重试按钮禁用
    await act(async () => {
      host(container).dispatchEvent(
        dropEvent({
          items: [
            {
              kind: 'file',
              getAsFile: () => null,
              webkitGetAsEntry: () => ({ isDirectory: true, name: 'mydir' }),
            },
          ],
          files: [],
        }),
      );
    });
    const retryBtns = () => [...container.querySelectorAll<HTMLButtonElement>('.terminal-file-item button')];
    expect(retryBtns()).toHaveLength(2); // [a.png(unknown), mydir(failed)]
    await act(async () => {
      retryBtns()[1].click();
    });
    expect(retryBtns()[0].disabled).toBe(true); // 在途绑定 mydir：a.png 的重试禁用
    expect(retryBtns()[1].disabled).toBe(false);

    // ③ input cancel：mydir 保持失败、暂停保持、绑定解除（按钮恢复可用）
    const input = container.querySelector<HTMLInputElement>('input[type="file"]')!;
    await act(async () => {
      input.dispatchEvent(new Event('cancel'));
    });
    expect(fileStatuses(container)).toEqual(['结果未知', '明确失败']);
    expect(container.querySelector('.terminal-file-panel-title')!.textContent).toContain('队列已暂停');
    expect(retryBtns()[0].disabled).toBe(false);
    expect(retryBtns()[1].disabled).toBe(false);

    // ④ 卸载后派发 cancel：监听已清理，不更新已销毁组件、不抛错
    unmount();
    expect(() => input.dispatchEvent(new Event('cancel'))).not.toThrow();
  });

  it('普通文件选择取消路径：change 时文件列表为空 → 视为取消（绑定解除、状态不变）', async () => {
    const { container, unmount } = mountTerminal('/ws/terminal/task-1');
    await act(async () => {
      host(container).dispatchEvent(
        dropEvent({
          items: [
            {
              kind: 'file',
              getAsFile: () => null,
              webkitGetAsEntry: () => ({ isDirectory: true, name: 'mydir' }),
            },
          ],
          files: [],
        }),
      );
    });
    await act(async () => {
      container.querySelector<HTMLButtonElement>('.terminal-file-item button')!.click();
    });
    const input = container.querySelector<HTMLInputElement>('input[type="file"]')!;
    // jsdom input.files 默认空 FileList → onChange 取 files.length===0 → handlePickerResult(null)
    await act(async () => {
      input.dispatchEvent(new Event('change'));
    });
    expect(fileStatuses(container)).toEqual(['明确失败']);
    expect(container.querySelector('.terminal-file-item')!.textContent).toContain('不支持目录');
    unmount();
  });
});

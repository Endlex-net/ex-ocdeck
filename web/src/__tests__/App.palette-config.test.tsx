// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import type { Root } from 'react-dom/client';
import { App } from '../App';
import { api, UNAUTHORIZED_EVENT } from '../api';
import { PALETTE_CONFIG_CHANGED_EVENT, type PaletteConfig } from '../palette-focus';
import { DEFAULT_PALETTE_CONFIG } from '../components/PaletteConfigPanel';
import { formatHotkey } from '../hotkey';
import { mount, flushUI, stubMatchMedia } from './cm-test-env';

let token = 'fake-token';

vi.mock('../api', () => ({
  api: {
    getNotificationConfig: vi.fn(() =>
      Promise.resolve({
        enabled: false,
        categories: { question: true, permission: true, idle: true, retry: true, error: true },
        idle_timeout_seconds: 60,
        channels: {
          web: { enabled: false },
          bark: { enabled: false, endpoint: '', token_masked: '' },
          macos: { enabled: false },
        },
        llm_summary: false,
        base_url: '',
      }),
    ),
    getPaletteConfig: vi.fn(),
    listProjects: vi.fn(() => Promise.resolve({ projects: [] })),
  },
  getToken: vi.fn(() => token),
  setToken: vi.fn((t: string) => {
    token = t;
  }),
  UNAUTHORIZED_EVENT: 'ocdeck:unauthorized',
  ApiError: class ApiError extends Error {
    constructor(
      public readonly status: number,
      public readonly code: string,
      message: string,
    ) {
      super(message);
      this.name = 'ApiError';
    }
  },
}));

vi.mock('../hooks', () => ({
  useTheme: () => ({ preference: 'system' as const, setPreference: () => {} }),
  useProjects: () => ({ projects: [], initialized: true, error: '' }),
}));

vi.mock('../pages/CommandCenterPage', () => ({
  CommandCenterPage: ({ matchMode }: { matchMode?: string }) => (
    <div data-testid="cc-match">{matchMode ?? ''}</div>
  ),
}));
vi.mock('../pages/ProjectsManagePage', () => ({ ProjectsManagePage: () => null }));
vi.mock('../pages/TaskWorkbenchPage', () => ({ TaskWorkbenchPage: () => null }));
vi.mock('../components/CommandPalette', () => ({
  CommandPalette: ({
    triggerWord,
    matchMode,
    open,
  }: {
    triggerWord?: string;
    matchMode?: string;
    open: boolean;
  }) => (
    <div data-testid="palette-props" data-open={open ? '1' : '0'} data-trigger={triggerWord} data-mode={matchMode} />
  ),
}));
vi.mock('../components/ServerStatusBanner', () => ({ ServerStatusBanner: () => null }));

const getPaletteMock = vi.mocked(api.getPaletteConfig);

function cfg(over: Partial<PaletteConfig> = {}): PaletteConfig {
  return { ...DEFAULT_PALETTE_CONFIG, ...over };
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const roots: Root[] = [];
let savedLocalStorage: Storage | undefined;
const store = new Map<string, string>();

beforeEach(() => {
  vi.clearAllMocks();
  stubMatchMedia(false);
  token = 'fake-token';
  window.location.hash = '#/';
  getPaletteMock.mockResolvedValue(cfg());
  savedLocalStorage = globalThis.localStorage;
  store.clear();
  (globalThis as { localStorage: Storage }).localStorage = {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
    key: (i: number) => Array.from(store.keys())[i] ?? null,
    get length() {
      return store.size;
    },
  } as Storage;
});

afterEach(async () => {
  while (roots.length) {
    const root = roots.pop()!;
    await act(async () => {
      root.unmount();
    });
  }
  if (savedLocalStorage === undefined) {
    delete (globalThis as { localStorage?: Storage }).localStorage;
  } else {
    (globalThis as { localStorage: Storage }).localStorage = savedLocalStorage;
  }
});

async function renderApp() {
  const utils = mount(<App />);
  roots.push(utils.root);
  await flushUI();
  return utils;
}

describe('App 命令面板配置代际', () => {
  it('authed=false 不发起 GET，使用默认快照', async () => {
    token = '';
    const { container } = await renderApp();
    expect(getPaletteMock).not.toHaveBeenCalled();
    expect(container.textContent).toContain('ocdeck');
  });

  it('同一 act 内 unauthorized 后配置事件不 GET、不应用', async () => {
    getPaletteMock.mockResolvedValue(cfg());
    const { container } = await renderApp();
    expect(getPaletteMock).toHaveBeenCalledTimes(1);
    const stolen = cfg({ hotkey: 'alt+k', triggerWord: 'stolen', matchMode: 'exact' });
    act(() => {
      window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
      window.dispatchEvent(new CustomEvent(PALETTE_CONFIG_CHANGED_EVENT, { detail: stolen }));
    });
    await flushUI();
    expect(getPaletteMock).toHaveBeenCalledTimes(1);
    expect(container.querySelector('[data-testid="palette-props"]')).toBeNull();
    expect(container.querySelector('input[type="password"]')).not.toBeNull();
  });

  it('TokenGate 期间派发合法配置事件不 GET、不应用', async () => {
    token = '';
    const { container } = await renderApp();
    expect(getPaletteMock).not.toHaveBeenCalled();
    act(() => {
      window.dispatchEvent(
        new CustomEvent(PALETTE_CONFIG_CHANGED_EVENT, {
          detail: cfg({ hotkey: 'alt+k', triggerWord: 'stolen', matchMode: 'exact' }),
        }),
      );
    });
    await flushUI();
    expect(getPaletteMock).not.toHaveBeenCalled();
    expect(container.querySelector('[data-testid="palette-props"]')).toBeNull();
    expect(container.textContent).toContain('ocdeck');
    expect(container.querySelector('input[type="password"]')).not.toBeNull();
  });

  it('非法事件 detail 不应用、不重拉', async () => {
    getPaletteMock.mockResolvedValue(cfg());
    const { container } = await renderApp();
    expect(getPaletteMock).toHaveBeenCalledTimes(1);
    act(() => {
      window.dispatchEvent(
        new CustomEvent(PALETTE_CONFIG_CHANGED_EVENT, {
          detail: { hotkey: 'alt+k', triggerWord: 'x', matchMode: 'prefix' },
        }),
      );
    });
    await flushUI();
    expect(getPaletteMock).toHaveBeenCalledTimes(1);
    expect(container.querySelector('[data-testid="palette-props"]')?.getAttribute('data-trigger')).toBe('new');
  });

  it('首次挂载 authed=true 开新代际并发起 GET', async () => {
    getPaletteMock.mockResolvedValue(cfg({ triggerWord: 'newtask', matchMode: 'exact' }));
    const { container } = await renderApp();
    expect(getPaletteMock).toHaveBeenCalledTimes(1);
    expect(container.querySelector('[data-testid="cc-match"]')?.textContent).toBe('exact');
    expect(container.querySelector('[data-testid="palette-props"]')?.getAttribute('data-trigger')).toBe('newtask');
  });

  it('TokenGate 保存后 false→true 开新代际 GET', async () => {
    token = '';
    const { container } = await renderApp();
    expect(getPaletteMock).not.toHaveBeenCalled();

    token = 'saved-token';
    getPaletteMock.mockResolvedValue(cfg({ triggerWord: 'create' }));
    const input = container.querySelector<HTMLInputElement>('input[type="password"]')!;
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
      setter.call(input, 'saved-token');
      input.dispatchEvent(new Event('input', { bubbles: true }));
    });
    await act(async () => {
      container.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    });
    await flushUI();
    expect(getPaletteMock).toHaveBeenCalledTimes(1);
    expect(container.querySelector('[data-testid="palette-props"]')?.getAttribute('data-trigger')).toBe('create');
  });

  it('401 后 true→false 使在途 GET 失效并重置默认；再认证开新代际', async () => {
    const first = deferred<PaletteConfig>();
    getPaletteMock.mockReturnValueOnce(first.promise);
    const { container } = await renderApp();
    expect(getPaletteMock).toHaveBeenCalledTimes(1);

    act(() => {
      window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
    });
    await flushUI();
    first.resolve(cfg({ triggerWord: 'stale' }));
    await flushUI();
    expect(container.querySelector('[data-testid="palette-props"]')).toBeNull();

    token = 'reauthed';
    getPaletteMock.mockResolvedValue(cfg({ triggerWord: 'fresh' }));
    const input = container.querySelector<HTMLInputElement>('input[type="password"]')!;
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
      setter.call(input, 'reauthed');
      input.dispatchEvent(new Event('input', { bubbles: true }));
    });
    await act(async () => {
      container.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    });
    await flushUI();
    expect(getPaletteMock).toHaveBeenCalledTimes(2);
    expect(container.querySelector('[data-testid="palette-props"]')?.getAttribute('data-trigger')).toBe('fresh');
  });

  it('deferred Promise 乱序只写最新代际', async () => {
    const first = deferred<PaletteConfig>();
    const second = deferred<PaletteConfig>();
    getPaletteMock.mockReturnValueOnce(first.promise);
    const { container } = await renderApp();

    act(() => {
      window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
    });
    await flushUI();

    token = 'gen2';
    getPaletteMock.mockReturnValueOnce(second.promise);
    const input = container.querySelector<HTMLInputElement>('input[type="password"]')!;
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
      setter.call(input, 'gen2');
      input.dispatchEvent(new Event('input', { bubbles: true }));
    });
    await act(async () => {
      container.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    });
    await flushUI();

    first.resolve(cfg({ triggerWord: 'old-gen' }));
    await flushUI();
    expect(container.querySelector('[data-testid="palette-props"]')?.getAttribute('data-trigger')).not.toBe('old-gen');

    second.resolve(cfg({ triggerWord: 'new-gen' }));
    await flushUI();
    expect(container.querySelector('[data-testid="palette-props"]')?.getAttribute('data-trigger')).toBe('new-gen');
  });

  it('PUT 保存事件使在途初始 GET 失效，stale 响应不得覆盖 PUT 值', async () => {
    const initial = deferred<PaletteConfig>();
    getPaletteMock.mockReturnValueOnce(initial.promise);
    const { container } = await renderApp();
    expect(getPaletteMock).toHaveBeenCalledTimes(1);

    const saved = cfg({ hotkey: 'alt+k', triggerWord: 'saved', matchMode: 'exact' });
    const refetch = deferred<PaletteConfig>();
    getPaletteMock.mockReturnValueOnce(refetch.promise);
    act(() => {
      window.dispatchEvent(new CustomEvent(PALETTE_CONFIG_CHANGED_EVENT, { detail: saved }));
    });
    await flushUI();
    expect(getPaletteMock).toHaveBeenCalledTimes(2);
    const props = () => container.querySelector('[data-testid="palette-props"]');

    // 保存前的初始 GET 迟到返回 stale：代际已推进，不得覆盖 PUT 值与 ready 状态
    initial.resolve(cfg({ hotkey: 'mod+shift+k', triggerWord: 'stale', matchMode: 'exact-then-substring' }));
    await flushUI();
    expect(props()?.getAttribute('data-trigger')).toBe('saved');
    expect(props()?.getAttribute('data-mode')).toBe('exact');
    expect(container.querySelector('.od-sidebar-cmdk')?.getAttribute('title')).toBe(
      `命令面板（${formatHotkey('alt+k')}）`,
    );

    // 后台重拉失败后 PUT 值仍保留
    refetch.reject(new Error('refetch failed'));
    await flushUI();
    expect(props()?.getAttribute('data-trigger')).toBe('saved');
  });

  it('保存成功事件原子应用 detail、清 loadError，重拉失败保留 PUT 值', async () => {
    getPaletteMock.mockRejectedValueOnce(new Error('load failed'));
    window.location.hash = '#/configs#palette';
    const { container } = await renderApp();
    expect(container.textContent).toContain('加载命令面板配置失败');

    const saved = cfg({ hotkey: 'alt+k', triggerWord: 'newtask', matchMode: 'exact' });
    const refetch = deferred<PaletteConfig>();
    getPaletteMock.mockReturnValueOnce(refetch.promise);
    act(() => {
      window.dispatchEvent(new CustomEvent(PALETTE_CONFIG_CHANGED_EVENT, { detail: saved }));
    });
    await flushUI();
    expect(container.textContent).not.toContain('加载命令面板配置失败');
    expect(container.querySelector<HTMLInputElement>('#palette-hotkey')?.value).toBe('alt+k');
    expect(container.querySelector<HTMLInputElement>('#palette-trigger')?.value).toBe('newtask');

    refetch.reject(new Error('refetch failed'));
    await flushUI();
    expect(container.querySelector<HTMLInputElement>('#palette-hotkey')?.value).toBe('alt+k');
    expect(container.textContent).not.toContain('加载命令面板配置失败');
  });

  it('GET 失败提示后 PUT 成功事件清除提示并应用 canonical', async () => {
    getPaletteMock.mockRejectedValueOnce(new Error('boom'));
    window.location.hash = '#/configs#palette';
    const { container } = await renderApp();
    expect(container.textContent).toContain('加载命令面板配置失败');
    expect(container.querySelector<HTMLInputElement>('#palette-hotkey')?.value).toBe('mod+k');

    getPaletteMock.mockRejectedValueOnce(new Error('refetch skipped'));
    act(() => {
      window.dispatchEvent(
        new CustomEvent(PALETTE_CONFIG_CHANGED_EVENT, {
          detail: cfg({ hotkey: 'mod+shift+k', triggerWord: 'go' }),
        }),
      );
    });
    await flushUI();
    expect(container.textContent).not.toContain('加载命令面板配置失败');
    expect(container.querySelector<HTMLInputElement>('#palette-hotkey')?.value).toBe('mod+shift+k');
    expect(container.querySelector<HTMLInputElement>('#palette-trigger')?.value).toBe('go');
  });

  it('热键监听读配置：改配置后新组合生效', async () => {
    const { container } = await renderApp();
    expect(container.querySelector('[data-testid="palette-props"]')?.getAttribute('data-open')).toBe('0');

    act(() => {
      window.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', metaKey: true, bubbles: true }));
    });
    expect(container.querySelector('[data-testid="palette-props"]')?.getAttribute('data-open')).toBe('1');
    act(() => {
      window.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', metaKey: true, bubbles: true }));
    });
    expect(container.querySelector('[data-testid="palette-props"]')?.getAttribute('data-open')).toBe('0');

    getPaletteMock.mockRejectedValueOnce(new Error('refetch skipped'));
    act(() => {
      window.dispatchEvent(
        new CustomEvent(PALETTE_CONFIG_CHANGED_EVENT, { detail: cfg({ hotkey: 'alt+x' }) }),
      );
    });
    await flushUI();

    act(() => {
      window.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', metaKey: true, bubbles: true }));
    });
    expect(container.querySelector('[data-testid="palette-props"]')?.getAttribute('data-open')).toBe('0');

    act(() => {
      window.dispatchEvent(new KeyboardEvent('keydown', { key: 'x', altKey: true, bubbles: true }));
    });
    expect(container.querySelector('[data-testid="palette-props"]')?.getAttribute('data-open')).toBe('1');
  });

  it('AppShell 两处文案随配置变化', async () => {
    const { container } = await renderApp();
    const label = formatHotkey('mod+k');
    const btn = container.querySelector<HTMLButtonElement>('.od-sidebar-cmdk')!;
    expect(btn.title).toBe(`命令面板（${label}）`);
    expect(btn.querySelector('.od-palette-kbd')?.textContent).toBe(label);

    getPaletteMock.mockRejectedValueOnce(new Error('refetch skipped'));
    act(() => {
      window.dispatchEvent(
        new CustomEvent(PALETTE_CONFIG_CHANGED_EVENT, { detail: cfg({ hotkey: 'alt+k' }) }),
      );
    });
    await flushUI();
    const next = formatHotkey('alt+k');
    expect(btn.title).toBe(`命令面板（${next}）`);
    expect(btn.querySelector('.od-palette-kbd')?.textContent).toBe(next);
  });
});

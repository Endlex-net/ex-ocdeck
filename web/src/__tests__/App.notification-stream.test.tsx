// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import type { Root } from 'react-dom/client';
import { App } from '../App';
import { api } from '../api';
import {
  emitNotificationConfigChanged,
  NOTIFICATION_CONFIG_SYNC_KEY,
  subscribeNotifications,
} from '../notifications';

// mock 模块导出的监听器 unsubscribe 记录（见 vi.mock 工厂）；React 18 unmount
// 不 flush passive cleanup，afterEach 手动调用移除跨用例累积的 window 监听器。
const notificationsMod = await import('../notifications');
const testUnsubs = (notificationsMod as unknown as { __testUnsubs: Array<() => void> }).__testUnsubs;
import type { NotificationConfig } from '../types';
import { mount, flushUI, stubMatchMedia } from './cm-test-env';

vi.mock('../api', () => ({
  api: {
    getNotificationConfig: vi.fn(),
    getPaletteConfig: vi.fn(() =>
      Promise.resolve({ hotkey: 'mod+k', triggerWord: 'new', matchMode: 'exact-then-substring' }),
    ),
  },
  getToken: vi.fn(() => 'fake-token'),
  UNAUTHORIZED_EVENT: 'od:unauthorized',
}));

vi.mock('../notifications', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../notifications')>();
  // React 18 unmount 不 flush passive effect cleanup，window 监听器会跨用例累积。
  // mock subscribeNotificationConfigChanged 记录每次返回的 unsubscribe，afterEach
  // 手动调用移除监听，避免后续 emit 触发已卸载组件的 setConfigEpoch。
  const unsubs: Array<() => void> = [];
  const subscribeConfigChanged = vi.fn((cb: () => void) => {
    const onLocal = () => cb();
    const onStorage = (e: StorageEvent) => {
      if (e.key === orig.NOTIFICATION_CONFIG_SYNC_KEY) cb();
    };
    window.addEventListener(orig.NOTIFICATION_CONFIG_CHANGED_EVENT, onLocal);
    window.addEventListener('storage', onStorage);
    const unsub = () => {
      window.removeEventListener(orig.NOTIFICATION_CONFIG_CHANGED_EVENT, onLocal);
      window.removeEventListener('storage', onStorage);
    };
    unsubs.push(unsub);
    return unsub;
  });
  return {
    ...orig,
    subscribeNotifications: vi.fn(() => ({ close: vi.fn() })),
    showNotification: vi.fn(),
    subscribeNotificationConfigChanged: subscribeConfigChanged,
    __testUnsubs: unsubs,
  };
});

vi.mock('../hooks', () => ({
  useTheme: () => ({ preference: 'system' as const, setPreference: () => {} }),
}));

vi.mock('../components/TokenGate', () => ({ TokenGate: () => null }));
vi.mock('../components/AppShell', () => ({
  AppShell: ({ children }: { children: unknown }) => <div>{children as never}</div>,
}));
vi.mock('../components/CommandPalette', () => ({ CommandPalette: () => null }));
vi.mock('../pages/CommandCenterPage', () => ({ CommandCenterPage: () => <div>home</div> }));
vi.mock('../pages/ProjectsManagePage', () => ({ ProjectsManagePage: () => null }));
vi.mock('../pages/SettingsPage', () => ({ SettingsPage: () => null }));
vi.mock('../pages/TaskWorkbenchPage', () => ({ TaskWorkbenchPage: () => null }));

const getConfigMock = vi.mocked(api.getNotificationConfig);
const subscribeMock = vi.mocked(subscribeNotifications);

function cfg(over: Partial<NotificationConfig> = {}): NotificationConfig {
  return {
    enabled: true,
    categories: { question: true, permission: true, idle: true, retry: true, error: true },
    idle_timeout_seconds: 60,
    channels: {
      web: { enabled: true },
      bark: { enabled: false, endpoint: '', token_masked: '' },
      macos: { enabled: false },
      wecom: { enabled: false, url_masked: '' },
    },
    llm_summary: false,
    base_url: '',
    ...over,
  };
}

function stubPermission(state: NotificationPermission | 'unsupported') {
  if (state === 'unsupported') {
    delete (globalThis as Record<string, unknown>).Notification;
    return;
  }
  (globalThis as Record<string, unknown>).Notification = { permission: state, requestPermission: vi.fn() };
}

const roots: Root[] = [];

// localStorage stub：jsdom 在 vitest + Node 26 实验性 localStorage 下可能不暴露
// globalThis.localStorage（见运行时警告 "localStorage is not available"）。
// 用 Map 后端提供 getItem/setItem/removeItem/clear/key/length，沿
// session-adapter.test.ts:310 模式；afterEach 恢复原值避免跨文件污染。
let savedLocalStorage: Storage | undefined;
const store = new Map<string, string>();

beforeEach(() => {
  vi.clearAllMocks();
  stubMatchMedia(false);
  stubPermission('granted');
  window.location.hash = '#/';
  getConfigMock.mockResolvedValue(cfg());
  subscribeMock.mockReturnValue({ close: vi.fn() });
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
  localStorage.removeItem(NOTIFICATION_CONFIG_SYNC_KEY);
});

afterEach(async () => {
  // React 18 unmount 不 flush passive effect cleanup，window 监听器跨用例累积。
  // 手动调用 mock 记录的 unsubscribe 移除监听，避免后续 emit 触发已卸载组件的
  // setConfigEpoch（configEpoch 跳多级导致 subscribe 次数异常）。
  while (testUnsubs.length) testUnsubs.pop()?.();
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

describe('App 通知 SSE 订阅条件（D1）', () => {
  it('启用 web + 已授权时建立订阅', async () => {
    roots.push(mount(<App />).root);
    await flushUI();
    expect(getConfigMock).toHaveBeenCalled();
    expect(subscribeMock).toHaveBeenCalledTimes(1);
  });

  it('保存关闭总开关后关闭订阅（notification-config-changed）', async () => {
    const close = vi.fn();
    subscribeMock.mockReturnValue({ close });
    roots.push(mount(<App />).root);
    await flushUI();
    expect(subscribeMock).toHaveBeenCalledTimes(1);

    getConfigMock.mockResolvedValue(cfg({ enabled: false }));
    act(() => emitNotificationConfigChanged());
    await flushUI();
    expect(close).toHaveBeenCalled();
    expect(subscribeMock).toHaveBeenCalledTimes(1);
  });

  it('授权成功后重建订阅', async () => {
    stubPermission('default');
    roots.push(mount(<App />).root);
    await flushUI();
    expect(subscribeMock).not.toHaveBeenCalled();

    stubPermission('granted');
    act(() => emitNotificationConfigChanged());
    await flushUI();
    expect(subscribeMock).toHaveBeenCalledTimes(1);
  });

  it('另一标签页 storage 事件触发本页重判并关闭订阅', async () => {
    const close = vi.fn();
    subscribeMock.mockReturnValue({ close });
    roots.push(mount(<App />).root);
    await flushUI();
    expect(subscribeMock).toHaveBeenCalledTimes(1);

    getConfigMock.mockResolvedValue(cfg({ enabled: false }));
    act(() => {
      window.dispatchEvent(
        new StorageEvent('storage', { key: NOTIFICATION_CONFIG_SYNC_KEY, newValue: String(Date.now()) }),
      );
    });
    await flushUI();
    expect(close).toHaveBeenCalled();
    expect(subscribeMock).toHaveBeenCalledTimes(1);
  });

  it('emit 写入跨标签 nonce（本页 setItem 不触发 storage，异页靠该 key）', () => {
    emitNotificationConfigChanged();
    expect(localStorage.getItem(NOTIFICATION_CONFIG_SYNC_KEY)).toBeTruthy();
  });
});

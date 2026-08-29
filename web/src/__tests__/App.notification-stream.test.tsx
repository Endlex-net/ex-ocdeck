// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { App } from '../App';
import { api } from '../api';
import {
  emitNotificationConfigChanged,
  NOTIFICATION_CONFIG_SYNC_KEY,
  subscribeNotifications,
} from '../notifications';
import type { NotificationConfig } from '../types';
import { mount, flushUI, stubMatchMedia } from './cm-test-env';

vi.mock('../api', () => ({
  api: {
    getNotificationConfig: vi.fn(),
  },
  getToken: vi.fn(() => 'fake-token'),
  UNAUTHORIZED_EVENT: 'od:unauthorized',
}));

vi.mock('../notifications', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../notifications')>();
  return {
    ...orig,
    subscribeNotifications: vi.fn(() => ({ close: vi.fn() })),
    showNotification: vi.fn(),
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

const unmounts: Array<() => void> = [];

beforeEach(() => {
  vi.clearAllMocks();
  stubMatchMedia(false);
  stubPermission('granted');
  window.location.hash = '#/';
  getConfigMock.mockResolvedValue(cfg());
  subscribeMock.mockReturnValue({ close: vi.fn() });
  localStorage.removeItem(NOTIFICATION_CONFIG_SYNC_KEY);
});

afterEach(() => {
  while (unmounts.length) unmounts.pop()?.();
});

describe('App 通知 SSE 订阅条件（D1）', () => {
  it('启用 web + 已授权时建立订阅', async () => {
    unmounts.push(mount(<App />).unmount);
    await flushUI();
    expect(getConfigMock).toHaveBeenCalled();
    expect(subscribeMock).toHaveBeenCalledTimes(1);
  });

  it('保存关闭总开关后关闭订阅（notification-config-changed）', async () => {
    const close = vi.fn();
    subscribeMock.mockReturnValue({ close });
    unmounts.push(mount(<App />).unmount);
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
    unmounts.push(mount(<App />).unmount);
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
    unmounts.push(mount(<App />).unmount);
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

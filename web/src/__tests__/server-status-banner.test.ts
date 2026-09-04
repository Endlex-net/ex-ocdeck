// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act, createElement } from 'react';
import { api } from '../api';
import { ServerStatusBanner } from '../components/ServerStatusBanner';
import type { ServerStatus } from '../types';
import { mount, flushUI } from './cm-test-env';

/* ============================ ServerStatusBanner「版本未验证提示可关闭」（mobile-terminal-mode-settings 7.1） ============================
 * localStorage 整体 mock（含 getItem/setItem 抛异常路径）；api 层 mock。
 * 组件挂载即惰性读取一次 dismissed 状态；点击「不再提示」写 key 并立即隐藏。 */

vi.mock('../api', () => ({
  api: { serverStatus: vi.fn() },
  UNAUTHORIZED_EVENT: 'ocdeck:unauthorized-test',
}));

const VERSION_NOTICE_KEY = 'ocdeck.versionNotice.dismissed';

const serverStatusMock = vi.mocked(api.serverStatus);

function status(over: Partial<ServerStatus> = {}): ServerStatus {
  return {
    opencodeVersion: '0.15.0',
    tmuxVersion: '3.4',
    shutdownPolicy: 'kill_immediate',
    watchdogState: 'running',
    contractMinVersion: '0.14.0',
    contractBaseline: '0.16.0',
    versionVerified: false,
    ...over,
  };
}

/** 用受控 fake 替换全局 localStorage，可指定读取/写入抛异常（SecurityError / quota）。 */
function stubStorage(values: Record<string, string> = {}, opts: { getItemThrows?: Error; setItemThrows?: Error } = {}) {
  const store = new Map(Object.entries(values));
  const storage = {
    getItem: vi.fn((key: string) => {
      if (opts.getItemThrows) throw opts.getItemThrows;
      return store.has(key) ? store.get(key)! : null;
    }),
    setItem: vi.fn((key: string, value: string) => {
      if (opts.setItemThrows) throw opts.setItemThrows;
      store.set(key, value);
    }),
    removeItem: vi.fn((key: string) => {
      store.delete(key);
    }),
    clear: vi.fn(() => {
      store.clear();
    }),
  };
  vi.stubGlobal('localStorage', storage);
  return storage;
}

async function mountWithStatus(over: Partial<ServerStatus> = {}) {
  serverStatusMock.mockResolvedValue(status(over));
  // .ts 测试文件不走 JSX 转换，用 createElement 渲染
  const mounted = mount(createElement(ServerStatusBanner));
  await flushUI();
  return mounted;
}

function findDismissButton(container: HTMLElement): HTMLButtonElement {
  const btn = [...container.querySelectorAll('button')].find((b) => b.textContent === '不再提示');
  expect(btn, '「不再提示」按钮应存在于版本未验证告警内').toBeDefined();
  return btn!;
}

describe('ServerStatusBanner 版本未验证提示可关闭', () => {
  beforeEach(() => {
    serverStatusMock.mockReset();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('未关闭：展示版本未验证告警与「不再提示」按钮，且读取 dismissed key', async () => {
    const storage = stubStorage();
    const { container, unmount } = await mountWithStatus();

    const alert = container.querySelector('.od-alert-info');
    expect(alert).not.toBeNull();
    expect(alert?.textContent).toContain('版本未验证');
    expect(alert?.textContent).toContain('opencode 版本未验证：当前 0.15.0');
    expect(storage.getItem).toHaveBeenCalledWith(VERSION_NOTICE_KEY);
    expect(findDismissButton(container)).toBeDefined();
    unmount();
  });

  it('非 \'1\' 值视为未关闭：照常展示', async () => {
    stubStorage({ [VERSION_NOTICE_KEY]: '0' });
    const { container, unmount } = await mountWithStatus();

    expect(container.querySelector('.od-alert-info')).not.toBeNull();
    unmount();
  });

  it('点击「不再提示」：写入 key=\'1\' 并立即隐藏', async () => {
    const storage = stubStorage();
    const { container, unmount } = await mountWithStatus();
    const btn = findDismissButton(container);

    act(() => btn.click());

    expect(storage.setItem).toHaveBeenCalledWith(VERSION_NOTICE_KEY, '1');
    expect(container.querySelector('.od-alert-info')).toBeNull();
    unmount();
  });

  it('已关闭：版本提示不渲染，但轮询照常发起（watchdog 独立评估）', async () => {
    stubStorage({ [VERSION_NOTICE_KEY]: '1' });
    const { container, unmount } = await mountWithStatus();

    expect(container.querySelector('.od-alert-info')).toBeNull();
    expect(serverStatusMock).toHaveBeenCalled();
    unmount();
  });

  it('已关闭 + watchdog 降级：降级告警照常展示，版本提示不展示', async () => {
    stubStorage({ [VERSION_NOTICE_KEY]: '1' });
    const { container, unmount } = await mountWithStatus({ watchdogState: 'degraded' });

    expect(container.querySelector('.od-alert-danger')).not.toBeNull();
    expect(container.querySelector('.od-alert-danger')?.textContent).toContain('watchdog 已降级');
    expect(container.querySelector('.od-alert-info')).toBeNull();
    unmount();
  });

  it('getItem 抛异常：视为未关闭，组件不抛错、告警照常展示', async () => {
    stubStorage({}, { getItemThrows: new DOMException('denied', 'SecurityError') });
    const { container, unmount } = await mountWithStatus();

    expect(container.querySelector('.od-alert-info')).not.toBeNull();
    expect(container.querySelector('.od-alert-danger')).toBeNull();
    unmount();
  });

  it('setItem 抛异常：当次隐藏生效且不抛错（不持久）', async () => {
    stubStorage({}, { setItemThrows: new DOMException('quota exceeded', 'QuotaExceededError') });
    const { container, unmount } = await mountWithStatus();
    const btn = findDismissButton(container);

    act(() => btn.click());

    expect(container.querySelector('.od-alert-info')).toBeNull();
    unmount();
  });
});

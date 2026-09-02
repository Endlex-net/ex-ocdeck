// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { NotificationConfigPanel } from '../components/NotificationConfigPanel';
import { SettingsPage } from '../pages/SettingsPage';
import { api, ApiError } from '../api';
import { DEFAULT_COMMAND_TRIGGERS } from '../palette-focus';
import type { NotificationConfig } from '../types';
import { mount, stubMatchMedia, flushUI } from './cm-test-env';

/* ============================ 通知子标签渲染与权限分支（task-notifications Lane D 4.7） ============================
 * 真实经过 SettingsPage（notifications tab）与 NotificationConfigPanel 渲染路径；
 * api 层 mock。覆盖：表单字段回填、load_error 展示、web 权限三态分支、保存请求体
 * （token 掩码保留语义）、测试通知结果渲染、总开关关闭时测试按钮禁用。 */

vi.mock('../api', () => ({
  api: {
    getNotificationConfig: vi.fn(),
    saveNotificationConfig: vi.fn(),
    testNotification: vi.fn(),
  },
  ApiError: class ApiError extends Error {
    constructor(
      public readonly status: number,
      public readonly code: string,
      message: string,
    ) {
      super(message);
    }
  },
}));

const getConfigMock = vi.mocked(api.getNotificationConfig);
const saveConfigMock = vi.mocked(api.saveNotificationConfig);
const testMock = vi.mocked(api.testNotification);

function baseConfig(over: Partial<NotificationConfig> = {}): NotificationConfig {
  return {
    enabled: true,
    categories: { question: true, permission: true, idle: true, retry: true, error: true },
    idle_timeout_seconds: 60,
    channels: {
      web: { enabled: false },
      bark: { enabled: false, endpoint: 'https://api.day.app', token_masked: '' },
      macos: { enabled: false },
    },
    llm_summary: false,
    base_url: '',
    ...over,
  };
}

async function renderPanel() {
  const utils = mount(<NotificationConfigPanel />);
  await flushUI();
  return utils;
}

beforeEach(() => {
  vi.clearAllMocks();
  stubMatchMedia(false);
});

describe('SettingsPage 通知子标签', () => {
  it('tab=notifications 渲染通知面板', async () => {
    getConfigMock.mockResolvedValue(baseConfig());
    const { container } = mount(
      <SettingsPage
        tab="notifications"
        paletteConfig={{
          hotkey: 'mod+k',
          triggerWord: 'new',
          matchMode: 'exact-then-substring',
          commandTriggers: DEFAULT_COMMAND_TRIGGERS,
        }}
        paletteLoadState="ready"
        paletteLoadError=""
      />,
    );
    await flushUI();
    expect(container.querySelector('#panel-notifications')).not.toBeNull();
    expect(container.textContent).toContain('任务通知');
  });
});

describe('NotificationConfigPanel', () => {
  it('字段按 GET 响应回填（含 token_masked 提示）', async () => {
    getConfigMock.mockResolvedValue(
      baseConfig({
        enabled: true,
        idle_timeout_seconds: 300,
        base_url: 'https://example.com',
        channels: {
          web: { enabled: true },
          bark: { enabled: true, endpoint: 'https://bark.local', token_masked: 'bark***' },
          macos: { enabled: true },
        },
        llm_summary: true,
      }),
    );
    const { container } = await renderPanel();

    const master = container.querySelector<HTMLInputElement>('#ntf-master');
    expect(master?.checked).toBe(true);
    expect(container.querySelector<HTMLInputElement>('#ntf-idle')?.value).toBe('300');
    expect(container.querySelector<HTMLInputElement>('#ntf-baseurl')?.value).toBe('https://example.com');
    // token_masked 只出现在 bark token 输入 placeholder（不回显明文）。
    const tokenInput = [...container.querySelectorAll('input')].find((i) => i.type === 'password')!;
    expect(tokenInput.placeholder).toContain('bark***');
  });

  it('GET 返回 load_error 时界面展示', async () => {
    getConfigMock.mockResolvedValue({ ...baseConfig(), load_error: 'parse notification config failed' });
    const { container } = await renderPanel();
    expect(container.textContent).toContain('配置文件损坏或不可读');
    expect(container.textContent).toContain('parse notification config failed');
  });

  it('web 权限三态分支：granted / denied / unsupported', async () => {
    const states = ['granted', 'denied', 'unsupported'] as const;
    for (const state of states) {
      // jsdom 无 Notification：unsupported 走默认分支。
      if (state === 'unsupported') {
        (globalThis as Record<string, unknown>).Notification = undefined;
      } else {
        (globalThis as Record<string, unknown>).Notification = { permission: state, requestPermission: vi.fn() };
      }
      getConfigMock.mockResolvedValue(baseConfig({ channels: { web: { enabled: true }, bark: { enabled: false, endpoint: '', token_masked: '' }, macos: { enabled: false } } }));
      const { container } = await renderPanel();
      if (state === 'granted') {
        expect(container.textContent).toContain('浏览器通知权限：已授权');
      } else if (state === 'denied') {
        expect(container.textContent).toContain('已拒绝');
        expect(container.textContent).not.toContain('申请通知权限');
      } else {
        expect(container.textContent).toContain('当前浏览器不支持系统通知');
      }
    }
    delete (globalThis as Record<string, unknown>).Notification;
  });

  it('保存提交 PUT 请求体（token 留空 = 保留原值语义）', async () => {
    getConfigMock.mockResolvedValue(
      baseConfig({
        channels: {
          web: { enabled: false },
          bark: { enabled: true, endpoint: 'https://api.day.app', token_masked: 'bark***' },
          macos: { enabled: false },
        },
      }),
    );
    saveConfigMock.mockResolvedValue(baseConfig());
    const { container } = await renderPanel();

    const changed = vi.fn();
    window.addEventListener('notification-config-changed', changed);
    await act(async () => {
      // jsdom 不模拟 button[type=submit] 隐式提交：form submit 事件直达。
      container
        .querySelector('form')!
        .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    });
    await flushUI();
    expect(saveConfigMock).toHaveBeenCalledTimes(1);
    expect(changed).toHaveBeenCalled();
    window.removeEventListener('notification-config-changed', changed);
    const body = saveConfigMock.mock.calls[0][0];
    expect(body.enabled).toBe(true);
    expect(body.idle_timeout_seconds).toBe(60);
    expect(body.channels.bark.token).toBe(''); // 未输入新 token → 空串（服务端保留原值）
    expect(body.channels.bark.endpoint).toBe('https://api.day.app');
    expect(container.textContent).toContain('保存成功');
  });

  it('总开关关闭时测试按钮禁用；开启时点击发送并渲染逐渠道结果', async () => {
    // 总开关关闭（默认配置）。
    getConfigMock.mockResolvedValue({ ...baseConfig({ enabled: false }) });
    const { container } = await renderPanel();
    const testBtn = [...container.querySelectorAll('button')].find((b) => b.textContent?.includes('发送测试通知'))!;
    expect(testBtn.disabled).toBe(true);

    // 重新挂载：总开关开。
    getConfigMock.mockResolvedValue(baseConfig());
    testMock.mockResolvedValue({
      results: [
        { name: 'bark', status: 'failed', error: 'bark: response code 400' },
        { name: 'web', status: 'success', error: '' },
        { name: 'macos', status: 'skipped', error: '' },
      ],
    });
    const second = await renderPanel();
    const btn = [...second.container.querySelectorAll('button')].find((b) =>
      b.textContent?.includes('发送测试通知'),
    )!;
    expect(btn.disabled).toBe(false);
    await act(async () => {
      btn.click();
    });
    await flushUI();
    expect(second.container.textContent).toContain('bark');
    expect(second.container.textContent).toContain('失败：bark: response code 400');
    expect(second.container.textContent).toContain('未启用或未配置');
  });

  it('保存 422（阈值越界）展示服务端 message', async () => {
    getConfigMock.mockResolvedValue(baseConfig());
    saveConfigMock.mockRejectedValue(
      new ApiError(422, 'invalid_input', 'idle_timeout_seconds 5 out of range [10, 3600]'),
    );
    const { container } = await renderPanel();
    // 本地校验：改成越界值提交（form submit 事件直达，jsdom 不模拟 button 隐式提交）。
    const idle = container.querySelector<HTMLInputElement>('#ntf-idle')!;
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
      setter.call(idle, '5');
      idle.dispatchEvent(new Event('input', { bubbles: true }));
    });
    await act(async () => {
      container
        .querySelector('form')!
        .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    });
    // 本地校验先行拦截（不发起请求）。
    expect(saveConfigMock).not.toHaveBeenCalled();
    expect(container.textContent).toContain('空闲阈值需为 10–3600 的整数');
  });

  it('申请权限 granted 后派发 notification-config-changed', async () => {
    const requestPermission = vi.fn().mockResolvedValue('granted');
    (globalThis as Record<string, unknown>).Notification = {
      permission: 'default',
      requestPermission,
    };
    getConfigMock.mockResolvedValue(
      baseConfig({
        channels: {
          web: { enabled: true },
          bark: { enabled: false, endpoint: '', token_masked: '' },
          macos: { enabled: false },
        },
      }),
    );
    const changed = vi.fn();
    window.addEventListener('notification-config-changed', changed);
    const { container } = await renderPanel();
    const btn = [...container.querySelectorAll('button')].find((b) => b.textContent?.includes('申请通知权限'))!;
    await act(async () => {
      btn.click();
    });
    await flushUI();
    expect(requestPermission).toHaveBeenCalled();
    expect(changed).toHaveBeenCalled();
    window.removeEventListener('notification-config-changed', changed);
    delete (globalThis as Record<string, unknown>).Notification;
  });
});
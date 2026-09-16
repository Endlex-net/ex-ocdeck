// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import type { Root } from 'react-dom/client';
import { BranchPrefixPanel } from '../components/BranchPrefixPanel';
import { SettingsPage } from '../pages/SettingsPage';
import { api, ApiError } from '../api';
import { resolveRoute } from '../router';
import { mount, flushUI, stubMatchMedia } from './cm-test-env';

/* ============================ 分支前缀设置子标签（worktree-branch-prefix spec / tasks.md 4.4） ============================
 * 镜像 palette-settings.test.tsx：真实经过 SettingsPage（branch-prefix tab）与
 * BranchPrefixPanel 渲染路径；api 层 mock。面板自加载（GET→ready→编辑→保存，
 * 加载/保存错误分离），非法输入就地校验不发 PUT。 */

vi.mock('../api', () => ({
  api: {
    getBranchPrefix: vi.fn(),
    putBranchPrefix: vi.fn(),
    getNotificationConfig: vi.fn(),
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

const getMock = vi.mocked(api.getBranchPrefix);
const putMock = vi.mocked(api.putBranchPrefix);

function setInputValue(el: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
  setter.call(el, value);
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

async function submitForm(container: HTMLElement) {
  await act(async () => {
    container
      .querySelector('form')!
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  });
  await flushUI();
}

const roots: Root[] = [];

beforeEach(() => {
  vi.clearAllMocks();
  stubMatchMedia(false);
  getMock.mockResolvedValue({ prefix: 'ocdeck' });
});

afterEach(async () => {
  while (roots.length) {
    const root = roots.pop()!;
    await act(async () => {
      root.unmount();
    });
  }
});

function renderPanel() {
  const utils = mount(<BranchPrefixPanel />);
  roots.push(utils.root);
  return utils;
}

describe('SettingsPage 分支前缀子标签', () => {
  it('tab=branch-prefix 渲染 #panel-branch-prefix', () => {
    const { container, root } = (() => {
      const u = mount(
        <SettingsPage
          tab="branch-prefix"
          paletteConfig={{ hotkey: 'mod+k', triggerWord: 'new', matchMode: 'exact', commandTriggers: {} as never }}
          paletteLoadState="ready"
          paletteLoadError=""
        />,
      );
      roots.push(u.root);
      return u;
    })();
    expect(container.querySelector('#panel-branch-prefix')).not.toBeNull();
    expect(container.textContent).toContain('工作空间');
    expect(root).toBeDefined();
  });

  it('深链 /configs#branch-prefix 直达分支前缀子标签', () => {
    expect(resolveRoute('/configs#branch-prefix')).toEqual({
      kind: 'page',
      page: 'configs',
      fragment: 'branch-prefix',
    });
  });
});

describe('BranchPrefixPanel', () => {
  it('信息层级：概念说明（前缀/slug + 默认 ocdeck）→ 当前生效值 → 输入 → 格式约束与影响范围', async () => {
    const { container } = renderPanel();
    await flushUI();
    const text = container.textContent ?? '';
    // ① 这里配置的是什么：工作空间分支名 = 前缀/slug，默认 ocdeck
    expect(text).toContain('前缀/slug');
    expect(text).toContain('ocdeck/my-feature');
    expect(text).toContain('默认为 ocdeck');
    // ② 当前生效值；③ 格式约束；④ 影响范围（仅新任务，存量任务改名沿用原前缀）
    expect(container.querySelector('#branch-prefix-current')!.textContent).toBe('ocdeck');
    expect(text).toContain('1-50 字符');
    expect(text).toContain('仅影响之后创建的任务');
    expect(text).toContain('沿用该任务原来的前缀');
    // 层级顺序：概念说明在当前生效值与输入之前
    expect(text.indexOf('前缀/slug')).toBeLessThan(text.indexOf('当前生效值'));
    expect(text.indexOf('当前生效值')).toBeLessThan(text.indexOf('分支名前缀'));
  });

  it('草稿合法时展示新分支名示例，非法/为空时不展示', async () => {
    const { container } = renderPanel();
    await flushUI();
    const preview = () => container.querySelector('[data-testid="branch-prefix-preview"]');
    expect(preview()!.textContent).toContain('ocdeck/<slug>');
    await act(async () => {
      setInputValue(container.querySelector<HTMLInputElement>('#branch-prefix-input')!, 'team-x');
    });
    expect(preview()!.textContent).toContain('team-x/<slug>');
    await act(async () => {
      setInputValue(container.querySelector<HTMLInputElement>('#branch-prefix-input')!, 'Team X');
    });
    expect(preview()).toBeNull();
  });

  it('GET 成功后展示当前生效值并以该值初始化输入', async () => {
    getMock.mockResolvedValue({ prefix: 'team-x' });
    const { container } = renderPanel();
    await flushUI();
    expect(container.querySelector('#branch-prefix-current')!.textContent).toBe('team-x');
    expect(container.querySelector<HTMLInputElement>('#branch-prefix-input')!.value).toBe('team-x');
  });

  it('加载失败：展示错误、禁用保存，重试加载后恢复', async () => {
    getMock.mockRejectedValueOnce(new ApiError(0, 'network_error', '无法连接服务端'));
    const { container } = renderPanel();
    await flushUI();
    expect(container.textContent).toContain('加载分支前缀配置失败');
    const saveBtn = [...container.querySelectorAll('button')].find((b) => b.textContent?.trim() === '保存')!;
    expect((saveBtn as HTMLButtonElement).disabled).toBe(true);

    getMock.mockResolvedValue({ prefix: 'ocdeck' });
    await act(async () => {
      [...container.querySelectorAll('button')]
        .find((b) => b.textContent?.trim() === '重试加载')!
        .dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
    });
    await flushUI();
    expect(container.querySelector('#branch-prefix-current')!.textContent).toBe('ocdeck');
  });

  it('合法值保存成功：PUT 携带原值并展示新生效值', async () => {
    putMock.mockResolvedValue({ prefix: 'team-x' });
    const { container } = renderPanel();
    await flushUI();
    await act(async () => {
      setInputValue(container.querySelector<HTMLInputElement>('#branch-prefix-input')!, 'team-x');
    });
    await submitForm(container);
    expect(putMock).toHaveBeenCalledTimes(1);
    expect(putMock).toHaveBeenCalledWith('team-x');
    expect(container.querySelector('#branch-prefix-current')!.textContent).toBe('team-x');
    expect(container.textContent).toContain('保存成功');
  });

  it.each([
    { name: '大写', value: 'Team' },
    { name: '含斜杠', value: 'team/x' },
    { name: '含空格', value: 'team x' },
    { name: '首尾空白', value: ' team' },
    { name: '空串', value: '' },
    { name: '连字符开头', value: '-team' },
    { name: '超长', value: `t${'a'.repeat(50)}` },
  ])('非法前缀（$name）就地展示校验错误，不发 PUT', async ({ value }) => {
    const { container } = renderPanel();
    await flushUI();
    await act(async () => {
      setInputValue(container.querySelector<HTMLInputElement>('#branch-prefix-input')!, value);
    });
    await submitForm(container);
    expect(putMock).not.toHaveBeenCalled();
    expect(container.textContent).toContain('前缀非法');
  });

  it('服务端 422 invalid_input：展示后端错误，生效值不变', async () => {
    putMock.mockRejectedValue(new ApiError(422, 'invalid_input', '前缀含非法字符'));
    const { container } = renderPanel();
    await flushUI();
    // 本地校验放行的合法值，由服务端拒绝（防御契约漂移）
    await act(async () => {
      setInputValue(container.querySelector<HTMLInputElement>('#branch-prefix-input')!, 'team9');
    });
    await submitForm(container);
    expect(putMock).toHaveBeenCalledTimes(1);
    expect(container.textContent).toContain('前缀含非法字符');
    expect(container.querySelector('#branch-prefix-current')!.textContent).toBe('ocdeck');
  });
});

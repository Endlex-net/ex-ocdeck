// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { DEFAULT_PALETTE_CONFIG, PaletteConfigPanel } from '../components/PaletteConfigPanel';
import { SettingsPage } from '../pages/SettingsPage';
import { api, ApiError } from '../api';
import {
  DEFAULT_COMMAND_TRIGGERS,
  PALETTE_CONFIG_CHANGED_EVENT,
  type PaletteCommandId,
  type PaletteConfig,
} from '../palette-focus';
import { resolveRoute } from '../router';
import { mount, stubMatchMedia, flushUI } from './cm-test-env';

/* ============================ 命令面板配置子标签（quick-create-shortcut-support Lane C 3.5） ============================
 * 真实经过 SettingsPage（palette tab）与 PaletteConfigPanel 渲染路径；api 层 mock。
 * 面板不独立 GET：config/loadState/loadError 由 props 下发。 */

vi.mock('../api', () => ({
  api: {
    getPaletteConfig: vi.fn(),
    putPaletteConfig: vi.fn(),
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

const getConfigMock = vi.mocked(api.getPaletteConfig);
const putConfigMock = vi.mocked(api.putPaletteConfig);

function baseConfig(over: Partial<PaletteConfig> = {}): PaletteConfig {
  const { commandTriggers, ...rest } = over;
  return {
    hotkey: 'mod+k',
    triggerWord: 'new',
    matchMode: 'exact-then-substring',
    commandTriggers: commandTriggers ?? DEFAULT_COMMAND_TRIGGERS,
    ...rest,
  };
}

function customTriggers(over: Partial<Record<PaletteCommandId, string>> = {}): Record<PaletteCommandId, string> {
  return { ...DEFAULT_COMMAND_TRIGGERS, ...over };
}

function cmdInput(container: HTMLElement, id: PaletteCommandId): HTMLInputElement {
  return container.querySelector<HTMLInputElement>(`#palette-cmd-${id}`)!;
}

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

function renderPanel(
  over: Partial<{
    config: PaletteConfig;
    loadState: 'loading' | 'ready' | 'error';
    loadError: string;
  }> = {},
) {
  return mount(
    <PaletteConfigPanel
      config={over.config ?? baseConfig()}
      loadState={over.loadState ?? 'ready'}
      loadError={over.loadError ?? ''}
    />,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  stubMatchMedia(false);
});

describe('SettingsPage 命令面板子标签', () => {
  it('tab=palette 渲染 #panel-palette', () => {
    const { container } = mount(
      <SettingsPage
        tab="palette"
        paletteConfig={DEFAULT_PALETTE_CONFIG}
        paletteLoadState="ready"
        paletteLoadError=""
      />,
    );
    expect(container.querySelector('#panel-palette')).not.toBeNull();
    expect(container.textContent).toContain('命令面板');
  });

  it('深链 /configs#palette 直达 palette 子标签', () => {
    expect(resolveRoute('/configs#palette')).toEqual({
      kind: 'page',
      page: 'configs',
      fragment: 'palette',
    });
    const { container } = mount(
      <SettingsPage
        tab="palette"
        paletteConfig={DEFAULT_PALETTE_CONFIG}
        paletteLoadState="ready"
        paletteLoadError=""
      />,
    );
    expect(container.querySelector('#panel-palette')).not.toBeNull();
    expect(container.querySelector('#panel-notifications')).toBeNull();
  });
});

describe('PaletteConfigPanel', () => {
  it('加载成功后以 canonical 值初始化 draft', () => {
    const { container } = renderPanel({
      config: baseConfig({
        hotkey: 'mod+shift+k',
        triggerWord: 'newtask',
        matchMode: 'exact',
      }),
    });
    expect(container.querySelector<HTMLInputElement>('#palette-hotkey')?.value).toBe('mod+shift+k');
    expect(container.querySelector<HTMLInputElement>('#palette-trigger')?.value).toBe('newtask');
    const exact = container.querySelector<HTMLInputElement>('input[name="palette-match-mode"][value="exact"]');
    expect(exact?.checked).toBe(true);
    expect(container.textContent).toContain('⌘⇧K / Ctrl+Shift+K');
  });

  it('加载中禁止保存', async () => {
    const { container } = renderPanel({ loadState: 'loading' });
    const saveBtn = [...container.querySelectorAll('button')].find((b) => b.textContent?.includes('保存'))!;
    expect(saveBtn.disabled).toBe(true);
    await submitForm(container);
    expect(putConfigMock).not.toHaveBeenCalled();
  });

  it('加载失败使用默认配置渲染并提示', () => {
    const { container } = renderPanel({
      config: baseConfig({ hotkey: 'alt+x', triggerWord: 'oops', matchMode: 'exact' }),
      loadState: 'error',
      loadError: 'network timeout',
    });
    expect(container.textContent).toContain('加载命令面板配置失败');
    expect(container.textContent).toContain('network timeout');
    expect(container.querySelector<HTMLInputElement>('#palette-hotkey')?.value).toBe('mod+k');
    expect(container.querySelector<HTMLInputElement>('#palette-trigger')?.value).toBe('new');
    const fallback = container.querySelector<HTMLInputElement>(
      'input[name="palette-match-mode"][value="exact-then-substring"]',
    );
    expect(fallback?.checked).toBe(true);
  });

  it.each([
    { name: '空串', value: '', local: true },
    { name: 'NBSP', value: '\u00A0', local: true },
    { name: 'U+FEFF', value: '\uFEFF', local: true },
    { name: '32 code point', value: 'a'.repeat(32), local: false },
    { name: '33 code point', value: 'a'.repeat(33), local: true },
    { name: '32 emoji code point', value: '😀'.repeat(32), local: false },
    { name: '33 emoji code point', value: '😀'.repeat(33), local: true },
  ])('triggerWord 边界：$name', async ({ value, local }) => {
    putConfigMock.mockResolvedValue(baseConfig({ triggerWord: value || 'new' }));
    const { container } = renderPanel();
    const input = container.querySelector<HTMLInputElement>('#palette-trigger')!;
    await act(async () => {
      setInputValue(input, value);
    });
    await submitForm(container);
    if (local) {
      expect(putConfigMock).not.toHaveBeenCalled();
      expect(container.querySelector('.od-alert-danger')).not.toBeNull();
    } else {
      expect(putConfigMock).toHaveBeenCalledTimes(1);
      expect(putConfigMock.mock.calls[0][0].triggerWord).toBe(value);
    }
  });

  it('加载后不修改直接保存 PUT 原 canonical', async () => {
    putConfigMock.mockResolvedValue(baseConfig({ hotkey: 'mod+k' }));
    const { container } = renderPanel({ config: baseConfig({ hotkey: 'mod+k' }) });
    await submitForm(container);
    expect(putConfigMock).toHaveBeenCalledTimes(1);
    expect(putConfigMock.mock.calls[0][0]).toEqual({
      hotkey: 'mod+k',
      triggerWord: 'new',
      matchMode: 'exact-then-substring',
      commandTriggers: DEFAULT_COMMAND_TRIGGERS,
    });
  });

  it('指令触发词小节固定 8 行并渲染默认词表（cc/pro/reg + 5 空键）', () => {
    const { container } = renderPanel();
    const inputs = [...container.querySelectorAll<HTMLInputElement>('input[id^="palette-cmd-"]')];
    expect(inputs).toHaveLength(8);
    expect(cmdInput(container, 'command-center').value).toBe('cc');
    expect(cmdInput(container, 'projects').value).toBe('pro');
    expect(cmdInput(container, 'register-project').value).toBe('reg');
    for (const id of ['settings-appearance', 'settings-env', 'settings-opencode', 'settings-ai', 'settings-palette'] as const) {
      expect(cmdInput(container, id).value).toBe('');
    }
  });

  it('draft 以 canonical 词表初始化（非默认 8 键全部入框）', () => {
    const triggers = customTriggers({
      'command-center': 'home',
      'settings-appearance': 'look',
      'register-project': '',
    });
    const { container } = renderPanel({ config: baseConfig({ commandTriggers: triggers }) });
    expect(cmdInput(container, 'command-center').value).toBe('home');
    expect(cmdInput(container, 'settings-appearance').value).toBe('look');
    expect(cmdInput(container, 'register-project').value).toBe('');
    expect(cmdInput(container, 'projects').value).toBe('pro');
  });

  it('非空值 fold 重复（cc/CC）行内提示且 MUST NOT 调用 PUT', async () => {
    const { container } = renderPanel();
    await act(async () => {
      setInputValue(cmdInput(container, 'projects'), 'CC');
    });
    expect(container.textContent).toContain('与「指挥中心」重复');
    await submitForm(container);
    expect(putConfigMock).not.toHaveBeenCalled();
  });

  it('指令词与全局 triggerWord fold 相同行内提示且 MUST NOT 调用 PUT', async () => {
    const { container } = renderPanel();
    await act(async () => {
      setInputValue(cmdInput(container, 'command-center'), 'NEW');
    });
    expect(container.textContent).toContain('与「快速新建触发词」重复');
    await submitForm(container);
    expect(putConfigMock).not.toHaveBeenCalled();
  });

  it('指令词字符规则（含空白）行内提示且 MUST NOT 调用 PUT', async () => {
    const { container } = renderPanel();
    await act(async () => {
      setInputValue(cmdInput(container, 'projects'), 'a b');
    });
    expect(cmdInput(container, 'projects').closest('div')?.textContent).toContain('触发词不能包含空白字符');
    await submitForm(container);
    expect(putConfigMock).not.toHaveBeenCalled();
  });

  it('默认配置（5 空键）不修改直接保存成功且 PUT 携带默认词表', async () => {
    putConfigMock.mockResolvedValue(baseConfig());
    const { container } = renderPanel();
    await submitForm(container);
    expect(putConfigMock).toHaveBeenCalledTimes(1);
    expect(putConfigMock.mock.calls[0][0].commandTriggers).toEqual(DEFAULT_COMMAND_TRIGGERS);
    expect(container.textContent).toContain('保存成功');
  });

  it('修改指令词保存成功后 PUT 携带新词表且事件 detail 为四键', async () => {
    const saved = baseConfig({ commandTriggers: customTriggers({ 'command-center': 'go' }) });
    putConfigMock.mockResolvedValue(saved);
    const { container } = renderPanel();
    await act(async () => {
      setInputValue(cmdInput(container, 'command-center'), 'go');
    });
    const changed = vi.fn();
    window.addEventListener(PALETTE_CONFIG_CHANGED_EVENT, changed);
    await submitForm(container);
    window.removeEventListener(PALETTE_CONFIG_CHANGED_EVENT, changed);
    expect(putConfigMock).toHaveBeenCalledTimes(1);
    expect(putConfigMock.mock.calls[0][0].commandTriggers['command-center']).toBe('go');
    expect(changed.mock.calls[0][0].detail).toEqual(saved);
    expect(cmdInput(container, 'command-center').value).toBe('go');
  });

  it('K+Shift+Mod 规范化为 mod+shift+k 后 PUT', async () => {
    putConfigMock.mockResolvedValue(baseConfig({ hotkey: 'mod+shift+k' }));
    const { container } = renderPanel();
    const input = container.querySelector<HTMLInputElement>('#palette-hotkey')!;
    await act(async () => {
      setInputValue(input, 'K+Shift+Mod');
    });
    await submitForm(container);
    expect(putConfigMock).toHaveBeenCalledTimes(1);
    expect(putConfigMock.mock.calls[0][0].hotkey).toBe('mod+shift+k');
  });

  it('PUT 200 派发 od:palette-config-changed，detail 直接为完整 PaletteConfig', async () => {
    const saved = baseConfig({
      hotkey: 'alt+k',
      triggerWord: 'newtask',
      matchMode: 'exact',
    });
    putConfigMock.mockResolvedValue(saved);
    const { container } = renderPanel();
    const hotkey = container.querySelector<HTMLInputElement>('#palette-hotkey')!;
    const trigger = container.querySelector<HTMLInputElement>('#palette-trigger')!;
    await act(async () => {
      setInputValue(hotkey, 'alt+k');
      setInputValue(trigger, 'newtask');
      container
        .querySelector<HTMLInputElement>('input[name="palette-match-mode"][value="exact"]')!
        .click();
    });

    const changed = vi.fn();
    window.addEventListener(PALETTE_CONFIG_CHANGED_EVENT, changed);
    await submitForm(container);
    window.removeEventListener(PALETTE_CONFIG_CHANGED_EVENT, changed);

    expect(changed).toHaveBeenCalledTimes(1);
    const detail = changed.mock.calls[0][0].detail;
    expect(detail).toEqual(saved);
    expect(detail).not.toHaveProperty('config');
    expect(container.textContent).toContain('保存成功');
  });

  it('保存失败展示后端错误且 MUST NOT 派发', async () => {
    putConfigMock.mockRejectedValue(new ApiError(422, 'invalid_input', 'hotkey mod+t is a reserved browser combo'));
    const { container } = renderPanel();
    const changed = vi.fn();
    window.addEventListener(PALETTE_CONFIG_CHANGED_EVENT, changed);
    await submitForm(container);
    window.removeEventListener(PALETTE_CONFIG_CHANGED_EVENT, changed);
    expect(putConfigMock).toHaveBeenCalled();
    expect(changed).not.toHaveBeenCalled();
    expect(container.textContent).toContain('hotkey mod+t is a reserved browser combo');
  });

  it('非法 hotkey（mod+t 保留组合）本地校验失败不 PUT', async () => {
    const { container } = renderPanel();
    const input = container.querySelector<HTMLInputElement>('#palette-hotkey')!;
    await act(async () => {
      setInputValue(input, 'mod+t');
    });
    await submitForm(container);
    expect(putConfigMock).not.toHaveBeenCalled();
    expect(container.querySelector('.od-alert-danger')).not.toBeNull();
  });

  it('triggerWord " new " 不 trim，按原始字符串校验失败不 PUT', async () => {
    const { container } = renderPanel();
    const input = container.querySelector<HTMLInputElement>('#palette-trigger')!;
    await act(async () => {
      setInputValue(input, ' new ');
    });
    await submitForm(container);
    expect(putConfigMock).not.toHaveBeenCalled();
    expect(input.value).toBe(' new ');
    expect(container.querySelector('.od-alert-danger')).not.toBeNull();
  });

  it('面板不独立 GET：渲染与交互全程不调用 getPaletteConfig', async () => {
    const { container } = renderPanel();
    expect(getConfigMock).not.toHaveBeenCalled();
    const hotkey = container.querySelector<HTMLInputElement>('#palette-hotkey')!;
    await act(async () => {
      setInputValue(hotkey, 'alt+k');
    });
    putConfigMock.mockResolvedValue(baseConfig({ hotkey: 'alt+k' }));
    await submitForm(container);
    expect(putConfigMock).toHaveBeenCalledTimes(1);
    expect(getConfigMock).not.toHaveBeenCalled();
  });

  it('热键预览仅在规范化且校验通过时展示，非法 raw 留空', async () => {
    const { container } = renderPanel();
    const preview = () =>
      container.querySelector('#palette-hotkey')!.parentElement!.querySelector('.muted.mono')!.textContent;
    expect(preview()).toBe('⌘K / Ctrl+K');

    const input = container.querySelector<HTMLInputElement>('#palette-hotkey')!;
    await act(async () => {
      setInputValue(input, 'mod++k');
    });
    expect(preview()).toBe('');

    await act(async () => {
      setInputValue(input, 'mod+t');
    });
    expect(preview()).toBe('');

    await act(async () => {
      setInputValue(input, 'K+Shift+Mod');
    });
    expect(preview()).toBe('⌘⇧K / Ctrl+Shift+K');
  });
});

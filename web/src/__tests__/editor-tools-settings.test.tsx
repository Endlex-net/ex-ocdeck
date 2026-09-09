// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { DEFAULT_PALETTE_CONFIG } from '../components/PaletteConfigPanel';
import { SettingsPage } from '../pages/SettingsPage';
import {
  DEFAULT_KEY,
  EDITOR_TOOLS_CHANGED,
  GOLAND_KEY,
  GOLAND_URI_TEMPLATE_KEY,
  VSCODE_KEY,
  VSCODE_URI_TEMPLATE_KEY,
} from '../editor-tools';
import { mount, stubMatchMedia } from './cm-test-env';

/* ============================ 「常用工具」设置子标签（add-frontend-tool-quick-open 2.4/2.5） ============================
 * 真实经过 SettingsPage(tools) 渲染路径；api mock；沿 clipboard-settings.test 范式。
 * 深链路由解析（resolveRoute）断言在 shell-contracts.test.ts 的路由用例中。 */

vi.mock('../api', () => ({
  api: {},
  ApiError: class ApiError extends Error {},
}));

const store = new Map<string, string>();
let savedStorage: Storage | undefined;
let savedMatchMedia: PropertyDescriptor | undefined;
let events = 0;
const countEvent = () => {
  events++;
};

// 统一 afterEach 清理：断言失败也不会泄漏已挂载组件；保存 mount() 包装的 unmount
// （含 container.remove()，评审 F6），避免 document.body 残留空容器。
const cleanups: Array<() => void> = [];

function mountSettings() {
  const utils = mount(
    <SettingsPage
      tab="tools"
      paletteConfig={DEFAULT_PALETTE_CONFIG}
      paletteLoadState="ready"
      paletteLoadError=""
    />,
  );
  cleanups.push(utils.unmount);
  return utils;
}

function toolCheckbox(container: HTMLElement, tool: 'vscode' | 'goland'): HTMLInputElement {
  return container.querySelector<HTMLInputElement>(`#editor-tool-${tool}`)!;
}

beforeEach(() => {
  store.clear();
  events = 0;
  savedStorage = globalThis.localStorage;
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
  savedMatchMedia = Object.getOwnPropertyDescriptor(window, 'matchMedia');
  stubMatchMedia(false);
  window.addEventListener(EDITOR_TOOLS_CHANGED, countEvent);
});

afterEach(async () => {
  while (cleanups.length) {
    const unmount = cleanups.pop()!;
    await act(async () => {
      unmount();
    });
  }
  window.removeEventListener(EDITOR_TOOLS_CHANGED, countEvent);
  vi.restoreAllMocks();
  if (savedMatchMedia) {
    Object.defineProperty(window, 'matchMedia', savedMatchMedia);
  } else {
    delete (window as { matchMedia?: unknown }).matchMedia;
  }
  (globalThis as { localStorage: Storage }).localStorage = savedStorage!;
});

describe('设置页「常用工具」子标签', () => {
  it('深链 tab 渲染面板；缺省（无记录）两个开关均关闭，每行附 hint', () => {
    const { container } = mountSettings();
    expect(container.textContent).toContain('常用工具');
    expect(container.querySelector('#tab-tools')?.getAttribute('aria-selected')).toBe('true');
    expect(toolCheckbox(container, 'vscode').checked).toBe(false);
    expect(toolCheckbox(container, 'goland').checked).toBe(false);
    expect(container.textContent).toContain('需本机已安装 VSCode');
    expect(container.textContent).toContain('需本机已安装 GoLand');
  });

  it('开启开关写入 localStorage（1）并恰好派发一次事件；另一开关不受影响', async () => {
    const { container } = mountSettings();
    await act(async () => {
      toolCheckbox(container, 'vscode').click();
    });
    expect(store.get(VSCODE_KEY)).toBe('1');
    expect(store.has(GOLAND_KEY)).toBe(false);
    expect(store.has(DEFAULT_KEY)).toBe(false); // 开关不触碰默认键
    expect(toolCheckbox(container, 'vscode').checked).toBe(true);
    expect(events).toBe(1);

    // 关闭写 0（非删除），DEFAULT_KEY 仍不被清除或改写
    store.set(DEFAULT_KEY, 'goland');
    events = 0;
    await act(async () => {
      toolCheckbox(container, 'vscode').click();
    });
    expect(store.get(VSCODE_KEY)).toBe('0');
    expect(store.get(DEFAULT_KEY)).toBe('goland');
    expect(events).toBe(1);
  });

  it('已有记录恢复勾选态（持久化刷新语义）', () => {
    store.set(VSCODE_KEY, '1');
    store.set(GOLAND_KEY, '0');
    const { container } = mountSettings();
    expect(toolCheckbox(container, 'vscode').checked).toBe(true);
    expect(toolCheckbox(container, 'goland').checked).toBe(false);
  });

  it('开关写失败不视为生效：UI 不翻转、存储不变、不派发事件', async () => {
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('quota');
    });
    const { container } = mountSettings();
    await act(async () => {
      toolCheckbox(container, 'goland').click();
    });
    expect(toolCheckbox(container, 'goland').checked).toBe(false);
    expect(store.has(GOLAND_KEY)).toBe(false);
    expect(events).toBe(0);
  });
});

describe('设置页「自定义唤起 URI 模板」输入（add-frontend-tool-quick-open 2.5）', () => {
  function templateInput(container: HTMLElement, tool: 'vscode' | 'goland'): HTMLInputElement {
    return container.querySelector<HTMLInputElement>(`#editor-tool-template-${tool}`)!;
  }

  /** 受控输入：原生 setter 赋值 + input 事件（React 受控口径），再失焦保存。 */
  async function blurSave(container: HTMLElement, tool: 'vscode' | 'goland', value: string) {
    const input = templateInput(container, tool);
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
      setter.call(input, value);
      input.dispatchEvent(new Event('input', { bubbles: true }));
      input.dispatchEvent(new FocusEvent('focusout', { bubbles: true }));
    });
  }

  it('placeholder 为内置默认模板；能力说明 hint 呈现', () => {
    const { container } = mountSettings();
    expect(templateInput(container, 'vscode').placeholder).toBe('vscode://file{path}/');
    expect(templateInput(container, 'goland').placeholder).toBe('jetbrains://goland/navigate/reference?project={path}');
    expect(container.textContent).toContain('vscode://vscode-remote/ssh-remote+<主机>{path}');
    expect(container.textContent).toContain('需本机已安装 Remote-SSH 扩展且 SSH 可达');
    expect(container.textContent).toContain('GoLand 不存在通过 URL 打开远程项目的可用形式');
  });

  it('已有记录恢复输入值；失焦保存非空写原文并派发事件', async () => {
    store.set(VSCODE_URI_TEMPLATE_KEY, 'vscode://file{path}');
    const { container } = mountSettings();
    expect(templateInput(container, 'vscode').value).toBe('vscode://file{path}');

    events = 0;
    await blurSave(container, 'goland', 'goland://open?file={path}');
    expect(store.get(GOLAND_URI_TEMPLATE_KEY)).toBe('goland://open?file={path}');
    expect(events).toBe(1);
  });

  it('空值失焦保存 = 清除恢复内置', async () => {
    store.set(VSCODE_URI_TEMPLATE_KEY, 'vscode://file{path}');
    const { container } = mountSettings();
    await blurSave(container, 'vscode', '');
    expect(store.has(VSCODE_URI_TEMPLATE_KEY)).toBe(false);
    expect(templateInput(container, 'vscode').value).toBe('');
  });

  it('未失焦草稿不被无关事件覆盖：开关/默认键变更后草稿保留，失焦仍正常保存（评审 F1 回归）', async () => {
    const { container } = mountSettings();

    // 输入草稿但不失焦
    const input = templateInput(container, 'vscode');
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
      setter.call(input, 'vscode://x{path}');
      input.dispatchEvent(new Event('input', { bubbles: true }));
    });
    expect(input.value).toBe('vscode://x{path}');

    // 无关变更事件：另一工具开关写入派发 + 跨标签页默认键 storage 事件
    await act(async () => {
      store.set(GOLAND_KEY, '1');
      window.dispatchEvent(new CustomEvent(EDITOR_TOOLS_CHANGED));
    });
    await act(async () => {
      store.set(DEFAULT_KEY, 'goland');
      window.dispatchEvent(new Event('storage'));
    });
    expect(templateInput(container, 'vscode').value).toBe('vscode://x{path}'); // 草稿保留

    // 失焦正常保存草稿
    await act(async () => {
      input.dispatchEvent(new FocusEvent('focusout', { bubbles: true }));
    });
    expect(store.get(VSCODE_URI_TEMPLATE_KEY)).toBe('vscode://x{path}');
  });

  it('未编辑字段在事件后照常收敛为存储值', () => {
    store.set(GOLAND_URI_TEMPLATE_KEY, 'goland://open?file={path}');
    const { container } = mountSettings();
    expect(templateInput(container, 'goland').value).toBe('goland://open?file={path}');
    // 外部清除该键（未在编辑）→ 事件收敛为空
    act(() => {
      store.delete(GOLAND_URI_TEMPLATE_KEY);
      window.dispatchEvent(new Event('storage'));
    });
    expect(templateInput(container, 'goland').value).toBe('');
  });

  it('模板写失败不视为生效：存储不变、输入回收敛为已存值', async () => {
    store.set(VSCODE_URI_TEMPLATE_KEY, 'vscode://file{path}');
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('quota');
    });
    const { container } = mountSettings();
    await blurSave(container, 'vscode', 'vscode://broken{path}');
    expect(store.get(VSCODE_URI_TEMPLATE_KEY)).toBe('vscode://file{path}');
    expect(templateInput(container, 'vscode').value).toBe('vscode://file{path}');
    expect(events).toBe(0);
  });
});

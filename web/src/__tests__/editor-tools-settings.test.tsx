// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { DEFAULT_PALETTE_CONFIG } from '../components/PaletteConfigPanel';
import { SettingsPage } from '../pages/SettingsPage';
import {
  CUSTOM_TOOLS_KEY,
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

function toolCheckbox(container: HTMLElement, tool: 'vscode' | 'goland' | 'cursor'): HTMLInputElement {
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
  it('Cursor 开关持久化；模板 placeholder 为 cursor://file{path}/', async () => {
    const { container } = mountSettings();
    expect(
      container.querySelector<HTMLInputElement>('#editor-tool-template-cursor')!.placeholder,
    ).toBe('cursor://file{path}/');
    await act(async () => {
      toolCheckbox(container, 'cursor').click();
    });
    expect(store.get('ocdeck.editorTools.cursor')).toBe('1');
    expect(store.has(DEFAULT_KEY)).toBe(false);
  });

  it('深链 tab 渲染面板；缺省（无记录）三个开关均关闭，每行附 hint', () => {
    const { container } = mountSettings();
    expect(container.textContent).toContain('常用工具');
    expect(container.querySelector('#tab-tools')?.getAttribute('aria-selected')).toBe('true');
    expect(toolCheckbox(container, 'vscode').checked).toBe(false);
    expect(toolCheckbox(container, 'goland').checked).toBe(false);
    expect(toolCheckbox(container, 'cursor').checked).toBe(false);
    expect(container.textContent).toContain('需本机已安装 VSCode');
    expect(container.textContent).toContain('需本机已安装 GoLand');
    expect(container.textContent).toContain('需本机已安装 Cursor');
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
    expect(templateInput(container, 'goland').placeholder).toBe('goland://open?file={path}');
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

describe('设置页「自定义工具」子区（C2 跨标签删除复活防护 + C3 脏草稿出口）', () => {
  /** 受控输入赋值 + 失焦保存（React 受控口径）。 */
  async function blurCustom(container: HTMLElement, kind: 'name' | 'template', suffix: string, value: string) {
    const input = container.querySelector<HTMLInputElement>(`#custom-tool-${kind}-${suffix}`)!;
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
      setter.call(input, value);
      input.dispatchEvent(new Event('input', { bubbles: true }));
      input.dispatchEvent(new FocusEvent('focusout', { bubbles: true }));
    });
  }

  /** 仅输入（不失焦）：制造脏草稿。 */
  async function typeInput(input: HTMLInputElement, value: string) {
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
      setter.call(input, value);
      input.dispatchEvent(new Event('input', { bubbles: true }));
    });
  }

  function addButton(container: HTMLElement): HTMLButtonElement {
    return [...container.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '添加自定义工具',
    )!;
  }

  function findButton(container: HTMLElement, text: string): HTMLButtonElement | null {
    return [...container.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === text,
    ) ?? null;
  }

  function removeButton(container: HTMLElement): HTMLButtonElement {
    return [...container.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '删除',
    )!;
  }

  function rowHints(container: HTMLElement): string[] {
    return [...container.querySelectorAll('.od-hint')]
      .map((n) => n.textContent ?? '')
      .filter((t) => t.includes('暂不出现在快捷打开列表'));
  }

  it('外部删除不复活：删除事件后修改另一行/添加，被删除项仍不存在', async () => {
    store.set(
      CUSTOM_TOOLS_KEY,
      JSON.stringify([
        { id: 'a', name: 'A', template: 'a://x{path}' },
        { id: 'x', name: 'X', template: 'x://x{path}' },
      ]),
    );
    const { container } = mountSettings();
    expect(container.querySelector('#custom-tool-name-x')).not.toBeNull();

    // B 标签删除 X → storage 事件 → A 面板移除 X
    act(() => {
      store.set(CUSTOM_TOOLS_KEY, JSON.stringify([{ id: 'a', name: 'A', template: 'a://x{path}' }]));
      window.dispatchEvent(new Event('storage'));
    });
    expect(container.querySelector('#custom-tool-name-x')).toBeNull();

    // 修改另一行失焦：以最新存储为基准保存，X 不复活
    await blurCustom(container, 'name', 'a', 'A2');
    expect(JSON.parse(store.get(CUSTOM_TOOLS_KEY)!)).toEqual([
      { id: 'a', name: 'A2', template: 'a://x{path}' },
    ]);
    expect(container.querySelector('#custom-tool-name-x')).toBeNull();

    // 添加新行：X 仍不复活
    await act(async () => {
      addButton(container).click();
    });
    const stored = JSON.parse(store.get(CUSTOM_TOOLS_KEY)!) as { id: string }[];
    expect(stored.some((t) => t.id === 'x')).toBe(false);
    expect(stored).toHaveLength(2); // A2 + 新空行
  });

  it('脏草稿保护：外部删除时正在编辑的行保留草稿（存储不复活）', async () => {
    store.set(CUSTOM_TOOLS_KEY, JSON.stringify([{ id: 'x', name: 'X', template: 'x://x{path}' }]));
    const { container } = mountSettings();
    const input = container.querySelector<HTMLInputElement>('#custom-tool-name-x')!;

    // 输入未失焦（脏草稿）
    await typeInput(input, 'Typing…');
    expect(input.value).toBe('Typing…');

    // 外部删除
    act(() => {
      store.delete(CUSTOM_TOOLS_KEY);
      window.dispatchEvent(new Event('storage'));
    });
    // 脏草稿行保留（正在编辑的内容不丢），存储不被复活
    expect(container.querySelector<HTMLInputElement>('#custom-tool-name-x')!.value).toBe('Typing…');
    expect(store.has(CUSTOM_TOOLS_KEY)).toBe(false);
  });

  it('C3 外部删除的脏草稿失焦不丢：不写存储、草稿与提示保留、继续编辑另一字段仍在', async () => {
    store.set(CUSTOM_TOOLS_KEY, JSON.stringify([{ id: 'x', name: 'X', template: 'x://x{path}' }]));
    const { container } = mountSettings();
    await typeInput(container.querySelector<HTMLInputElement>('#custom-tool-name-x')!, 'Typing…');

    // 外部删除（脏草稿保护保留行）
    act(() => {
      store.delete(CUSTOM_TOOLS_KEY);
      window.dispatchEvent(new Event('storage'));
    });

    // 名称失焦：不写存储、不删除草稿
    await act(async () => {
      container
        .querySelector<HTMLInputElement>('#custom-tool-name-x')!
        .dispatchEvent(new FocusEvent('focusout', { bubbles: true }));
    });
    expect(store.has(CUSTOM_TOOLS_KEY)).toBe(false);
    expect(container.querySelector<HTMLInputElement>('#custom-tool-name-x')!.value).toBe('Typing…');
    expect(container.textContent).toContain('该工具已在其他标签页被删除');
    expect(findButton(container, '放弃草稿')).not.toBeNull();
    expect(findButton(container, '另存为新工具')).not.toBeNull();

    // 继续编辑另一字段仍在（模板失焦同样不写存储）
    await blurCustom(container, 'template', 'x', 'y://y{path}');
    expect(store.has(CUSTOM_TOOLS_KEY)).toBe(false);
    expect(container.querySelector<HTMLInputElement>('#custom-tool-template-x')!.value).toBe('y://y{path}');
  });

  it('C3 操作其他行后草稿仍在且原 id 未复活', async () => {
    store.set(
      CUSTOM_TOOLS_KEY,
      JSON.stringify([
        { id: 'a', name: 'A', template: 'a://x{path}' },
        { id: 'x', name: 'X', template: 'x://x{path}' },
      ]),
    );
    const { container } = mountSettings();
    await typeInput(container.querySelector<HTMLInputElement>('#custom-tool-name-x')!, 'Typing…');

    // 外部删除 X（本面板的 X 草稿保留）
    act(() => {
      store.set(CUSTOM_TOOLS_KEY, JSON.stringify([{ id: 'a', name: 'A', template: 'a://x{path}' }]));
      window.dispatchEvent(new Event('storage'));
    });
    expect(container.querySelector<HTMLInputElement>('#custom-tool-name-x')!.value).toBe('Typing…');

    // 修改另一行（A）失焦：X 草稿仍在、x id 不复活
    await blurCustom(container, 'name', 'a', 'A2');
    const stored = JSON.parse(store.get(CUSTOM_TOOLS_KEY)!) as { id: string }[];
    expect(stored).toEqual([{ id: 'a', name: 'A2', template: 'a://x{path}' }]);
    expect(container.querySelector<HTMLInputElement>('#custom-tool-name-x')!.value).toBe('Typing…');
  });

  it('C3 出口一「放弃草稿」：移除草稿行，无存储写入', async () => {
    store.set(CUSTOM_TOOLS_KEY, JSON.stringify([{ id: 'x', name: 'X', template: 'x://x{path}' }]));
    const { container } = mountSettings();
    await typeInput(container.querySelector<HTMLInputElement>('#custom-tool-name-x')!, 'Typing…');
    act(() => {
      store.delete(CUSTOM_TOOLS_KEY);
      window.dispatchEvent(new Event('storage'));
    });

    await act(async () => {
      findButton(container, '放弃草稿')!.click();
    });
    expect(container.querySelector('#custom-tool-name-x')).toBeNull(); // 草稿行移除
    expect(store.has(CUSTOM_TOOLS_KEY)).toBe(false); // 无存储写入
  });

  it('C3 出口二「另存为新工具」：新 id 追加入存储（草稿内容完整），原草稿行移除', async () => {
    store.set(CUSTOM_TOOLS_KEY, JSON.stringify([{ id: 'x', name: 'X', template: 'x://x{path}' }]));
    const { container } = mountSettings();
    await typeInput(container.querySelector<HTMLInputElement>('#custom-tool-name-x')!, 'Typing…');
    act(() => {
      store.delete(CUSTOM_TOOLS_KEY);
      window.dispatchEvent(new Event('storage'));
    });
    await act(async () => {
      container
        .querySelector<HTMLInputElement>('#custom-tool-name-x')!
        .dispatchEvent(new FocusEvent('focusout', { bubbles: true }));
    });

    await act(async () => {
      findButton(container, '另存为新工具')!.click();
    });
    const stored = JSON.parse(store.get(CUSTOM_TOOLS_KEY)!) as { id: string; name: string; template: string }[];
    expect(stored).toHaveLength(1);
    expect(stored[0].id).not.toBe('x'); // 新 id
    expect(stored[0].name).toBe('Typing…');
    expect(stored[0].template).toBe('x://x{path}');
    expect(container.querySelector('#custom-tool-name-x')).toBeNull(); // 原草稿行移除
    // 新行不再处于外部删除状态
    expect(container.textContent).not.toContain('该工具已在其他标签页被删除');
  });

  it('添加 → 空行持久化并派发事件；名称/模板失焦保存整表', async () => {
    const { container } = mountSettings();
    await act(async () => {
      addButton(container).click();
    });
    expect(events).toBe(1);
    const stored0 = JSON.parse(store.get(CUSTOM_TOOLS_KEY)!) as { id: string }[];
    expect(stored0).toHaveLength(1);
    const id = stored0[0].id;

    await blurCustom(container, 'name', id, 'My Editor');
    await blurCustom(container, 'template', id, 'myapp://open?path={path}');
    expect(events).toBe(3);
    expect(store.get(CUSTOM_TOOLS_KEY)).toBe(
      JSON.stringify([{ id, name: 'My Editor', template: 'myapp://open?path={path}' }]),
    );
    // 行不再有「不可用」提示
    expect(rowHints(container)).toHaveLength(0);
  });

  it('名称为空或模板非法 → od-hint 提示不进入可用列表（草稿保留可继续编辑）', async () => {
    store.set(CUSTOM_TOOLS_KEY, JSON.stringify([{ id: 'c1', name: '', template: '' }]));
    const { container } = mountSettings();
    expect(rowHints(container)).toHaveLength(1);
    await blurCustom(container, 'name', 'c1', 'Evil');
    await blurCustom(container, 'template', 'c1', 'javascript:alert(1){path}');
    expect(rowHints(container)).toHaveLength(1); // 模板危险 scheme → 仍不可用
    expect(store.get(CUSTOM_TOOLS_KEY)).toBe(
      JSON.stringify([{ id: 'c1', name: 'Evil', template: 'javascript:alert(1){path}' }]),
    ); // 草稿原文保留
    await blurCustom(container, 'template', 'c1', 'myapp://open?path={path}');
    expect(rowHints(container)).toHaveLength(0); // 名称+合法模板 → 可用
  });

  it('删除按钮：整表移除该行并持久化', async () => {
    store.set(
      CUSTOM_TOOLS_KEY,
      JSON.stringify([
        { id: 'c1', name: 'A', template: 'a://x{path}' },
        { id: 'c2', name: 'B', template: 'b://x{path}' },
      ]),
    );
    const { container } = mountSettings();
    expect(container.querySelectorAll<HTMLInputElement>('input[id^="custom-tool-name-"]')).toHaveLength(2);
    await act(async () => {
      removeButton(container).click();
    });
    expect(store.get(CUSTOM_TOOLS_KEY)).toBe(JSON.stringify([{ id: 'c2', name: 'B', template: 'b://x{path}' }]));
    expect(container.querySelectorAll<HTMLInputElement>('input[id^="custom-tool-name-"]')).toHaveLength(1);
    expect(events).toBe(1);
  });

  it('跨标签 storage 事件收敛：外部新增的自定义工具行出现', () => {
    const { container } = mountSettings();
    expect(container.querySelectorAll('input[id^="custom-tool-name-"]')).toHaveLength(0);
    act(() => {
      store.set(CUSTOM_TOOLS_KEY, JSON.stringify([{ id: 'cx', name: 'Remote', template: 'r://x{path}' }]));
      window.dispatchEvent(new Event('storage'));
    });
    expect(
      container.querySelector<HTMLInputElement>('#custom-tool-name-cx')!.value,
    ).toBe('Remote');
  });

  it('写失败不视为生效：列表收敛回已存值', async () => {
    store.set(CUSTOM_TOOLS_KEY, JSON.stringify([{ id: 'c1', name: 'A', template: 'a://x{path}' }]));
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('quota');
    });
    const { container } = mountSettings();
    await blurCustom(container, 'name', 'c1', 'Changed');
    expect(store.get(CUSTOM_TOOLS_KEY)).toBe(
      JSON.stringify([{ id: 'c1', name: 'A', template: 'a://x{path}' }]),
    );
    expect(container.querySelector<HTMLInputElement>('#custom-tool-name-c1')!.value).toBe('A');
    expect(events).toBe(0);
  });
});

// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { OpenInEditorMenu } from '../components/OpenInEditorMenu';
import {
  CUSTOM_TOOLS_KEY,
  DEFAULT_KEY,
  EDITOR_TOOLS_CHANGED,
  GOLAND_KEY,
  VSCODE_KEY,
  VSCODE_URI_TEMPLATE_KEY,
} from '../editor-tools';
import { mount, stubMatchMedia } from './cm-test-env';

/* ============================ 任务详情页「在编辑器中打开」入口（add-frontend-tool-quick-open 3.4/3.5） ============================
 * 覆盖 spec「任务详情页快捷打开入口」全部场景：显隐、主按钮 URI、下拉写默认（✓ 跟随）、
 * 默认键写失败不唤起、唤起同步异常页面可用、空路径禁用（禁用态呈现；处理器内前置失败零副作用
 * 由「点击重读存储」「构造异常」两例覆盖）、双通道事件收敛、disclosure 关闭、模板点击时读取。 */

const store = new Map<string, string>();
let savedLocalStorage: Storage | undefined;
let savedMatchMedia: PropertyDescriptor | undefined;
let events = 0;
const countEvent = () => {
  events++;
};
/** jsdom 的 location.assign 不可 redefine；整体 stub global location（window.location 同源生效）。 */
const assignMock = vi.fn((url: string) => {
  assignCalls.push(url);
});
let assignCalls: string[] = [];

// 统一 afterEach 清理：断言失败也不会泄漏已挂载组件；保存 mount() 包装的 unmount
// （含 container.remove()，评审 F6），避免 document.body 残留空容器。
const cleanups: Array<() => void> = [];

function enable(vscode: boolean, goland: boolean) {
  if (vscode) store.set(VSCODE_KEY, '1');
  if (goland) store.set(GOLAND_KEY, '1');
}

function renderMenu(worktreePath = '/tmp/wt') {
  const utils = mount(<OpenInEditorMenu worktreePath={worktreePath} />);
  cleanups.push(utils.unmount);
  return utils;
}

function mainButton(container: HTMLElement): HTMLButtonElement {
  return container.querySelector<HTMLButtonElement>('button[title^="使用"]')!;
}

function caretButton(container: HTMLElement): HTMLButtonElement {
  return container.querySelector<HTMLButtonElement>('button[aria-label="选择编辑器"]')!;
}

function menuItems(container: HTMLElement): HTMLButtonElement[] {
  return [...container.querySelectorAll<HTMLButtonElement>('#open-in-editor-menu .overflow-item')];
}

function itemLabels(container: HTMLElement): string[] {
  return menuItems(container).map((b) => b.textContent!.trim());
}

async function openMenu(container: HTMLElement) {
  await act(async () => {
    caretButton(container).click();
  });
}

async function clickItem(container: HTMLElement, label: string) {
  const btn = menuItems(container).find((b) => b.textContent!.includes(label))!;
  await act(async () => {
    btn.click();
  });
}

beforeEach(() => {
  store.clear();
  events = 0;
  assignCalls = [];
  assignMock.mockClear();
  savedLocalStorage = globalThis.localStorage;
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
  assignMock.mockImplementation((url: string) => {
    assignCalls.push(url);
  });
  vi.stubGlobal('location', { assign: assignMock });
  window.addEventListener(EDITOR_TOOLS_CHANGED, countEvent);
  savedMatchMedia = Object.getOwnPropertyDescriptor(window, 'matchMedia');
  stubMatchMedia(false);
});

afterEach(async () => {
  while (cleanups.length) {
    const unmount = cleanups.pop()!;
    await act(async () => {
      unmount();
    });
  }
  window.removeEventListener(EDITOR_TOOLS_CHANGED, countEvent);
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  if (savedMatchMedia) {
    Object.defineProperty(window, 'matchMedia', savedMatchMedia);
  } else {
    delete (window as { matchMedia?: unknown }).matchMedia;
  }
  if (savedLocalStorage === undefined) {
    delete (globalThis as { localStorage?: Storage }).localStorage;
  } else {
    (globalThis as { localStorage: Storage }).localStorage = savedLocalStorage;
  }
});

describe('入口显隐', () => {
  it('两个工具均关闭 → 完全隐藏、无占位', () => {
    const { container } = renderMenu();
    expect(container.innerHTML).toBe('');
  });

  it('仅启用一个工具也保持「主按钮 + ⌄」组合结构', () => {
    enable(true, false);
    const { container } = renderMenu();
    expect(mainButton(container).getAttribute('aria-label')).toBe('使用 VSCode 打开');
    expect(caretButton(container)).not.toBeNull();
  });

  it('存储的默认工具已被关闭 → 主按钮回退 VSCode', () => {
    enable(true, false);
    store.set(DEFAULT_KEY, 'goland');
    const { container } = renderMenu();
    expect(mainButton(container).getAttribute('aria-label')).toBe('使用 VSCode 打开');
  });
});

describe('点击主流程', () => {
  it('主按钮直接以默认编辑器打开（URI 按 worktree_path 构造）', async () => {
    enable(true, false);
    const { container } = renderMenu('/Users/me/my project');
    await act(async () => {
      mainButton(container).click();
    });
    expect(assignCalls).toEqual(['vscode://file/Users/me/my%20project/']);
    expect(events).toBe(0); // 主按钮不写默认键
  });

  it('下拉选择即打开并写默认键，✓ 跟随且刷新语义（重挂载）保持', async () => {
    enable(true, true);
    const { container } = renderMenu('/tmp/wt');
    await openMenu(container);
    expect(itemLabels(container)).toEqual(['VSCode ✓', 'GoLand']);

    await clickItem(container, 'GoLand');
    expect(assignCalls).toEqual(['goland://open?file=%2Ftmp%2Fwt']);
    expect(store.get(DEFAULT_KEY)).toBe('goland');

    // ✓ 跟随（下拉关闭）；重新打开后 GoLand 带 ✓
    expect(container.querySelector('#open-in-editor-menu')).toBeNull();
    await openMenu(container);
    expect(itemLabels(container)).toEqual(['VSCode', 'GoLand ✓']);

    // 重挂载（刷新语义）后主按钮仍为 GoLand
    const remount = renderMenu('/tmp/wt');
    expect(mainButton(remount.container).getAttribute('aria-label')).toBe('使用 GoLand 打开');
  });

  it('点击重读存储（不用渲染快照）：底层已关闭且未派发事件 → 主按钮与下拉项均前置失败零副作用', async () => {
    enable(true, true);
    store.set(DEFAULT_KEY, 'vscode');
    const { container } = renderMenu('/tmp/wt');

    // 主按钮：挂载后 VSCode 被底层关闭（不派发事件，渲染快照仍显示启用）
    await act(async () => {
      store.set(VSCODE_KEY, '0');
      mainButton(container).click();
    });
    expect(assignCalls).toEqual([]);
    expect(events).toBe(0);
    expect(store.get(DEFAULT_KEY)).toBe('vscode');
    expect(container.querySelector('#open-in-editor-menu')).toBeNull();

    // 下拉项：GoLand 同样在打开菜单后被底层关闭 → 点击项不写默认键、不唤起、菜单关闭
    await act(async () => {
      store.set(GOLAND_KEY, '0');
    });
    await openMenu(container);
    await clickItem(container, 'GoLand');
    expect(assignCalls).toEqual([]);
    expect(events).toBe(0);
    expect(store.get(DEFAULT_KEY)).toBe('vscode');
    expect(container.querySelector('#open-in-editor-menu')).toBeNull();
  });

  it('URI 构造同步异常（孤立 UTF-16 代理对路径）→ 零副作用、菜单关闭、②不执行', async () => {
    enable(true, true);
    const { container } = renderMenu('/tmp/\uD800');

    // 主按钮：encodeURIComponent 抛 URIError，被第①步兜底捕获
    await act(async () => {
      mainButton(container).click();
    });
    expect(assignCalls).toEqual([]);
    expect(events).toBe(0);
    expect(store.has(DEFAULT_KEY)).toBe(false);

    // 下拉项：构造失败在第①步，②默认键写入不得执行
    await openMenu(container);
    await clickItem(container, 'GoLand');
    expect(assignCalls).toEqual([]);
    expect(events).toBe(0);
    expect(store.has(DEFAULT_KEY)).toBe(false);
    expect(container.querySelector('#open-in-editor-menu')).toBeNull();
  });

  it('默认键写失败不唤起、保留原默认与 ✓、菜单关闭', async () => {
    enable(true, true);
    store.set(DEFAULT_KEY, 'vscode');
    vi.spyOn(localStorage, 'setItem').mockImplementation((k: string) => {
      if (k === DEFAULT_KEY) throw new Error('quota');
    });
    const { container } = renderMenu('/tmp/wt');
    await openMenu(container);
    await clickItem(container, 'GoLand');

    expect(assignCalls).toEqual([]);
    expect(store.get(DEFAULT_KEY)).toBe('vscode');
    expect(container.querySelector('#open-in-editor-menu')).toBeNull();
    await openMenu(container);
    expect(itemLabels(container)).toEqual(['VSCode ✓', 'GoLand']);
  });

  it('location.assign 同步抛异常：页面可用、已保存默认不回滚、菜单关闭', async () => {
    enable(true, true);
    assignMock.mockImplementation(() => {
      throw new Error('navigation blocked');
    });
    const { container } = renderMenu('/tmp/wt');
    await openMenu(container);
    await clickItem(container, 'GoLand');

    // 写入成功在先、唤起异常被捕获：默认不回滚，菜单关闭
    expect(store.get(DEFAULT_KEY)).toBe('goland');
    expect(container.querySelector('#open-in-editor-menu')).toBeNull();
    expect(mainButton(container).getAttribute('aria-label')).toBe('使用 GoLand 打开');
  });

  it('空路径禁用：组合保留、双按钮呈禁用态（处理器内前置失败的零副作用由上两例覆盖）', () => {
    enable(true, true);
    const { container } = renderMenu('');
    expect(mainButton(container).disabled).toBe(true);
    expect(caretButton(container).disabled).toBe(true);
  });
});

describe('事件收敛（EDITOR_TOOLS_CHANGED 与 storage 各覆盖开关与默认键两类变更）', () => {
  it('EDITOR_TOOLS_CHANGED：开关变更收敛显隐、默认键变更收敛 ✓（默认实际变化）', async () => {
    const { container } = renderMenu();
    expect(container.innerHTML).toBe('');

    // 开关变更（另一标签页/设置页写入后派发）
    act(() => {
      enable(true, false);
      window.dispatchEvent(new CustomEvent(EDITOR_TOOLS_CHANGED));
    });
    expect(mainButton(container).getAttribute('aria-label')).toBe('使用 VSCode 打开');

    // 默认键变更：两工具均启用、原默认 VSCode → 写 GoLand → 主按钮与 ✓ 都要变
    act(() => {
      enable(true, true);
      store.set(DEFAULT_KEY, 'goland');
      window.dispatchEvent(new CustomEvent(EDITOR_TOOLS_CHANGED));
    });
    expect(mainButton(container).getAttribute('aria-label')).toBe('使用 GoLand 打开');
    await openMenu(container);
    expect(itemLabels(container)).toEqual(['VSCode', 'GoLand ✓']);
  });

  it('storage 事件：开关变更收敛显隐、默认键变更收敛 ✓', () => {
    enable(true, true);
    const { container } = renderMenu();
    expect(mainButton(container).getAttribute('aria-label')).toBe('使用 VSCode 打开');

    // 默认键跨标签页变更
    act(() => {
      store.set(DEFAULT_KEY, 'goland');
      window.dispatchEvent(new Event('storage'));
    });
    expect(mainButton(container).getAttribute('aria-label')).toBe('使用 GoLand 打开');

    // 开关跨标签页变更：仅剩 VSCode → 默认回退 VSCode；全关 → 隐藏
    act(() => {
      store.delete(GOLAND_KEY);
      window.dispatchEvent(new Event('storage'));
    });
    expect(mainButton(container).getAttribute('aria-label')).toBe('使用 VSCode 打开');
    act(() => {
      store.delete(VSCODE_KEY);
      window.dispatchEvent(new Event('storage'));
    });
    expect(container.innerHTML).toBe('');
  });
});

describe('Cursor 内置工具', () => {
  it('三工具均启用：下拉顺序 VSCode → GoLand → Cursor；选择 Cursor 写默认并唤起 cursor://', async () => {
    enable(true, true);
    store.set('ocdeck.editorTools.cursor', '1');
    const { container } = renderMenu('/tmp/wt');
    await openMenu(container);
    expect(itemLabels(container)).toEqual(['VSCode ✓', 'GoLand', 'Cursor']); // 无默认记录 → 回退首个启用

    await clickItem(container, 'Cursor');
    expect(assignCalls).toEqual(['cursor://file/tmp/wt/']);
    expect(store.get(DEFAULT_KEY)).toBe('cursor');
    await openMenu(container);
    expect(itemLabels(container)).toEqual(['VSCode', 'GoLand', 'Cursor ✓']);
  });

  it('存储默认 cursor → 主按钮 Cursor（内置分支 URI），关闭后回退 VSCode', async () => {
    enable(true, false);
    store.set('ocdeck.editorTools.cursor', '1');
    store.set(DEFAULT_KEY, 'cursor');
    const { container } = renderMenu('/Users/me/my project');
    expect(mainButton(container).getAttribute('aria-label')).toBe('使用 Cursor 打开');
    await act(async () => {
      mainButton(container).click();
    });
    expect(assignCalls).toEqual(['cursor://file/Users/me/my%20project/']);
  });
});

describe('自定义工具（名称 + URI 模板）', () => {
  const customRow = (id: string, name: string, template: string): string =>
    JSON.stringify([{ id, name, template }]);

  it('自定义工具出现在下拉（内置之后、存储顺序）；下拉选择写 custom:<id> 默认并按模板唤起', async () => {
    enable(true, true);
    store.set(CUSTOM_TOOLS_KEY, customRow('c1', 'My Editor', 'myapp://open?path={path}'));
    const { container } = renderMenu('/tmp/wt');
    await openMenu(container);
    expect(itemLabels(container)).toEqual(['VSCode ✓', 'GoLand', 'My Editor']);

    await clickItem(container, 'My Editor');
    expect(assignCalls).toEqual(['myapp://open?path=/tmp/wt']);
    expect(store.get(DEFAULT_KEY)).toBe('custom:c1');
    await openMenu(container);
    expect(itemLabels(container)).toEqual(['VSCode', 'GoLand', 'My Editor ✓']);
  });

  it('默认 custom:<id> → 主按钮用自定义工具名；存储删除该工具后主按钮回退 VSCode', async () => {
    enable(true, false);
    store.set(CUSTOM_TOOLS_KEY, customRow('c1', 'My Editor', 'myapp://open?path={path}'));
    store.set(DEFAULT_KEY, 'custom:c1');
    const { container } = renderMenu('/Users/me/my project');
    expect(mainButton(container).getAttribute('aria-label')).toBe('使用 My Editor 打开');

    // 工具被删除（跨标签）→ 主按钮回退 VSCode
    act(() => {
      store.delete(CUSTOM_TOOLS_KEY);
      window.dispatchEvent(new Event('storage'));
    });
    expect(mainButton(container).getAttribute('aria-label')).toBe('使用 VSCode 打开');
  });

  it('非法自定义模板（危险 scheme）→ 不出现在下拉；默认记忆指向它时主按钮回退内置', async () => {
    enable(true, true);
    store.set(CUSTOM_TOOLS_KEY, customRow('c1', 'Evil', 'javascript:alert(1){path}'));
    const { container } = renderMenu('/tmp/wt');
    await openMenu(container);
    expect(itemLabels(container)).toEqual(['VSCode ✓', 'GoLand']); // 不可用自定义工具不展示（C1 统一谓词）

    // 默认记忆指向不可用自定义工具 → 主按钮回退内置
    store.set(DEFAULT_KEY, 'custom:c1');
    const remount = renderMenu('/tmp/wt');
    expect(mainButton(remount.container).getAttribute('aria-label')).toBe('使用 VSCode 打开');
  });

  it('空草稿行排在合法工具之前 → 主按钮跳过空草稿选合法者；默认工具变不可用后回退', async () => {
    enable(false, false);
    store.set(
      CUSTOM_TOOLS_KEY,
      JSON.stringify([
        { id: 'draft', name: '', template: '' },
        { id: 'ok', name: 'My Editor', template: 'myapp://open?path={path}' },
      ]),
    );
    const { container } = renderMenu('/tmp/wt');
    expect(mainButton(container).getAttribute('aria-label')).toBe('使用 My Editor 打开'); // 空草稿不参与解析

    // 默认记忆指向合法自定义工具，随后模板被改为非法 → 主按钮回退
    store.set(DEFAULT_KEY, 'custom:ok');
    const remount = renderMenu('/tmp/wt');
    expect(mainButton(remount.container).getAttribute('aria-label')).toBe('使用 My Editor 打开');
    act(() => {
      store.set(
        CUSTOM_TOOLS_KEY,
        JSON.stringify([
          { id: 'draft', name: '', template: '' },
          { id: 'ok', name: 'My Editor', template: 'javascript:x{path}' },
        ]),
      );
      window.dispatchEvent(new Event('storage'));
    });
    expect(container.querySelector('button[title^="使用"]')).toBeNull(); // 无可用工具 → 隐藏
  });

  it('菜单打开后自定义工具名称被清空（跨标签）→ 该项从下拉移除', async () => {
    enable(true, true);
    store.set(CUSTOM_TOOLS_KEY, customRow('c1', 'My Editor', 'myapp://open?path={path}'));
    const { container } = renderMenu('/tmp/wt');
    await openMenu(container);
    expect(itemLabels(container)).toContain('My Editor');

    act(() => {
      store.set(CUSTOM_TOOLS_KEY, customRow('c1', '', 'myapp://open?path={path}'));
      window.dispatchEvent(new Event('storage'));
    });
    expect(itemLabels(container)).not.toContain('My Editor');
  });

  it('菜单渲染后工具变不可用（点击重读校验）→ 点击不唤起、不写默认键（C1 竞态回归）', async () => {
    enable(true, true);
    store.set(CUSTOM_TOOLS_KEY, customRow('c1', 'My Editor', 'myapp://open?path={path}'));
    const { container } = renderMenu('/tmp/wt');
    await openMenu(container);
    expect(itemLabels(container)).toContain('My Editor');

    // 渲染后底层模板被改为危险 scheme（未派发事件，渲染快照仍是旧名）
    await act(async () => {
      store.set(CUSTOM_TOOLS_KEY, customRow('c1', 'My Editor', 'javascript:alert(1){path}'));
    });
    await clickItem(container, 'My Editor');
    expect(assignCalls).toEqual([]);
    expect(store.has(DEFAULT_KEY)).toBe(false);
  });

  it('空名自定义工具行不进下拉；storage 事件收敛新增的自定义工具', () => {
    enable(false, false);
    store.set(CUSTOM_TOOLS_KEY, customRow('c0', '', ''));
    const { container } = renderMenu('/tmp/wt');
    expect(container.innerHTML).toBe(''); // 内置全关 + 自定义空名（不展示）→ 无可用工具

    // 跨标签新增合法自定义工具 → storage 事件收敛 → 入口出现
    act(() => {
      store.set(CUSTOM_TOOLS_KEY, customRow('c1', 'My Editor', 'myapp://open?path={path}'));
      window.dispatchEvent(new Event('storage'));
    });
    expect(mainButton(container).getAttribute('aria-label')).toBe('使用 My Editor 打开');
  });
});

describe('自定义唤起 URI 模板（add-frontend-tool-quick-open 3.5：点击时读取最新模板）', () => {
  it('自定义模板生效：主按钮按模板构造 URI（注入值不追加尾斜杠）', async () => {
    enable(true, false);
    store.set(VSCODE_URI_TEMPLATE_KEY, 'vscode://file{path}');
    const { container } = renderMenu('/Users/me/my project');
    await act(async () => {
      mainButton(container).click();
    });
    expect(assignCalls).toEqual(['vscode://file/Users/me/my%20project']);
  });

  it('非法模板（缺 {path}）回退内置分支', async () => {
    enable(true, false);
    store.set(VSCODE_URI_TEMPLATE_KEY, 'vscode://file/');
    const { container } = renderMenu('/tmp/wt');
    await act(async () => {
      mainButton(container).click();
    });
    expect(assignCalls).toEqual(['vscode://file/tmp/wt/']);
  });

  it('模板修改后下次点击即时生效（EDITOR_TOOLS_CHANGED 通道写入 → 点击重读）', async () => {
    enable(true, false);
    const { container } = renderMenu('/tmp/wt');

    // 第一次点击：无模板 → 内置分支
    await act(async () => {
      mainButton(container).click();
    });
    expect(assignCalls).toEqual(['vscode://file/tmp/wt/']);

    // 设置面板保存模板（写入 + 派发变更事件）
    act(() => {
      store.set(VSCODE_URI_TEMPLATE_KEY, 'vscode://vscode-remote/ssh-remote+devbox{path}');
      window.dispatchEvent(new CustomEvent(EDITOR_TOOLS_CHANGED));
    });

    // 第二次点击：读取最新模板
    await act(async () => {
      mainButton(container).click();
    });
    expect(assignCalls).toEqual(['vscode://file/tmp/wt/', 'vscode://vscode-remote/ssh-remote+devbox/tmp/wt']);
  });
});

describe('disclosure 模式', () => {
  it('打开聚焦首项；Escape 关闭；外部点击（backdrop）关闭', async () => {
    enable(true, true);
    const { container } = renderMenu();

    await openMenu(container);
    expect(document.activeElement).toBe(menuItems(container)[0]);

    // Escape 关闭
    await act(async () => {
      container
        .querySelector('#open-in-editor-menu')!
        .dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    });
    expect(container.querySelector('#open-in-editor-menu')).toBeNull();

    // 外部点击：backdrop 覆盖全屏，点击即外部
    await openMenu(container);
    await act(async () => {
      container.querySelector('.overflow-backdrop')!.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });
    expect(container.querySelector('#open-in-editor-menu')).toBeNull();
  });
});

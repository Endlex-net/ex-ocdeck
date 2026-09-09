// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  buildEditorUri,
  DEFAULT_KEY,
  EDITOR_TOOLS_CHANGED,
  GOLAND_KEY,
  GOLAND_URI_TEMPLATE_KEY,
  isValidUriTemplate,
  loadDefaultTool,
  loadEditorTools,
  loadEditorUriTemplate,
  resolveDefaultTool,
  saveDefaultTool,
  saveEditorTool,
  saveEditorUriTemplate,
  VSCODE_KEY,
  VSCODE_URI_TEMPLATE_KEY,
  type EditorTools,
} from '../editor-tools';

/* ============================ 常用工具偏好存储与 URI 构造（add-frontend-tool-quick-open 1.2） ============================
 * localStorage stub：Map 后端 + 按需 spyOn 注入读写失败（沿 mobile-mode.test.ts 模式）。 */

const store = new Map<string, string>();
let savedLocalStorage: Storage | undefined;
let events = 0;
const countEvent = () => {
  events++;
};

beforeEach(() => {
  store.clear();
  events = 0;
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
  window.addEventListener(EDITOR_TOOLS_CHANGED, countEvent);
});

afterEach(() => {
  window.removeEventListener(EDITOR_TOOLS_CHANGED, countEvent);
  vi.restoreAllMocks();
  if (savedLocalStorage === undefined) {
    delete (globalThis as { localStorage?: Storage }).localStorage;
  } else {
    (globalThis as { localStorage: Storage }).localStorage = savedLocalStorage;
  }
});

describe('loadEditorTools / saveEditorTool（开关存储）', () => {
  it('缺省（无记录）全部关闭', () => {
    expect(loadEditorTools()).toEqual({ vscode: false, goland: false });
  });

  it('开启持久化为 1；开启 VSCode 后 GoLand 仍为关闭', () => {
    saveEditorTool('vscode', true);
    expect(store.get(VSCODE_KEY)).toBe('1');
    expect(loadEditorTools()).toEqual({ vscode: true, goland: false });
  });

  it('关闭写 0（非删除），且不动 DEFAULT_KEY', () => {
    store.set(VSCODE_KEY, '1');
    store.set(DEFAULT_KEY, 'goland');
    saveEditorTool('vscode', false);
    expect(store.get(VSCODE_KEY)).toBe('0');
    expect(store.get(DEFAULT_KEY)).toBe('goland');
  });

  it('损坏数据只回退关闭，不改写 localStorage', () => {
    store.set(VSCODE_KEY, 'yes');
    store.set(GOLAND_KEY, '');
    expect(loadEditorTools()).toEqual({ vscode: false, goland: false });
    expect(store.get(VSCODE_KEY)).toBe('yes');
    expect(store.get(GOLAND_KEY)).toBe('');
  });

  it('成功写入恰好派发一次变更事件', () => {
    saveEditorTool('vscode', true);
    expect(events).toBe(1);
    saveEditorTool('goland', false);
    expect(events).toBe(2);
  });

  it('写入失败向上抛出、不派发变更事件', () => {
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('quota');
    });
    expect(() => saveEditorTool('vscode', true)).toThrow('quota');
    expect(events).toBe(0);
  });

  it('localStorage 读取异常按全关处理，不抛出', () => {
    vi.spyOn(localStorage, 'getItem').mockImplementation(() => {
      throw new Error('unavailable');
    });
    expect(loadEditorTools()).toEqual({ vscode: false, goland: false });
  });
});

describe('loadDefaultTool / saveDefaultTool（默认工具键）', () => {
  it('默认工具持久化；非法值返回 null 不改写', () => {
    expect(loadDefaultTool()).toBeNull();
    saveDefaultTool('goland');
    expect(store.get(DEFAULT_KEY)).toBe('goland');
    expect(loadDefaultTool()).toBe('goland');
    store.set(DEFAULT_KEY, 'vim');
    expect(loadDefaultTool()).toBeNull();
    expect(store.get(DEFAULT_KEY)).toBe('vim');
  });

  it('默认键读取异常按无记录返回 null，不抛至渲染层', () => {
    store.set(DEFAULT_KEY, 'vscode');
    vi.spyOn(localStorage, 'getItem').mockImplementation(() => {
      throw new Error('unavailable');
    });
    expect(loadDefaultTool()).toBeNull();
  });

  it('默认键写入失败向上抛出、不派发', () => {
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('quota');
    });
    expect(() => saveDefaultTool('vscode')).toThrow('quota');
    expect(events).toBe(0);
  });
});

describe('resolveDefaultTool（默认编辑器解析）', () => {
  const tools = (vscode: boolean, goland: boolean): EditorTools => ({ vscode, goland });

  it('stored 合法且已启用 → stored', () => {
    expect(resolveDefaultTool(tools(true, true), 'goland')).toBe('goland');
  });

  it('存储的默认工具已被关闭 → 按 vscode → goland 回退', () => {
    expect(resolveDefaultTool(tools(true, false), 'goland')).toBe('vscode');
    expect(resolveDefaultTool(tools(false, true), 'vscode')).toBe('goland');
  });

  it('关闭后重新开启恢复显式选择（期间未写入 DEFAULT_KEY）', () => {
    saveDefaultTool('goland');
    const closed = resolveDefaultTool(loadEditorTools(), loadDefaultTool());
    expect(closed).toBeNull();
    expect(store.get(DEFAULT_KEY)).toBe('goland');
    saveEditorTool('goland', true);
    expect(resolveDefaultTool(loadEditorTools(), loadDefaultTool())).toBe('goland');
  });

  it('无已启用工具 → null', () => {
    expect(resolveDefaultTool(tools(false, false), 'vscode')).toBeNull();
    expect(resolveDefaultTool(tools(false, false), null)).toBeNull();
  });

  it('默认键读取异常（null）时按回退规则解析，不抛出', () => {
    expect(resolveDefaultTool(tools(true, true), null)).toBe('vscode');
    expect(resolveDefaultTool(tools(false, true), null)).toBe('goland');
  });
});

describe('buildEditorUri（编辑器唤起 URI 构造，spec Scenario 逐条）', () => {
  it('VSCode：含空格路径分段编码并以恰好一个尾斜杠结尾', () => {
    expect(buildEditorUri('vscode', '/Users/me/my project')).toBe('vscode://file/Users/me/my%20project/');
  });

  it('VSCode：已有尾斜杠不重复追加', () => {
    expect(buildEditorUri('vscode', '/Users/me/proj/')).toBe('vscode://file/Users/me/proj/');
  });

  it('VSCode：多重尾斜杠收敛为一个', () => {
    expect(buildEditorUri('vscode', '/Users/me/proj//')).toBe('vscode://file/Users/me/proj/');
  });

  it('VSCode：Windows 盘符冒号保留字面、反斜杠归一', () => {
    expect(buildEditorUri('vscode', 'C:\\work\\my proj')).toBe('vscode://file/C:/work/my%20proj/');
  });

  it('GoLand：Windows 路径归一后整体一次编码', () => {
    expect(buildEditorUri('goland', 'C:\\work\\my proj')).toBe('goland://open?file=C%3A%2Fwork%2Fmy%20proj');
  });

  it('GoLand：含百分号路径单次编码（% → %25，不双重编码）', () => {
    expect(buildEditorUri('goland', '/tmp/a b%c')).toBe('goland://open?file=%2Ftmp%2Fa%20b%25c');
  });

  it('空路径返回空串', () => {
    expect(buildEditorUri('vscode', '')).toBe('');
    expect(buildEditorUri('goland', '')).toBe('');
  });
});

describe('自定义唤起 URI 模板：读写与合法性（add-frontend-tool-quick-open 1.3）', () => {
  it('缺省/空串/读取异常返回 null 按未设置处理，不改写 localStorage', () => {
    expect(loadEditorUriTemplate('vscode')).toBeNull();
    store.set(VSCODE_URI_TEMPLATE_KEY, '');
    expect(loadEditorUriTemplate('vscode')).toBeNull();
    expect(store.get(VSCODE_URI_TEMPLATE_KEY)).toBe(''); // 空串值不被改写
    store.set(GOLAND_URI_TEMPLATE_KEY, 'goland://open?file={path}');
    vi.spyOn(localStorage, 'getItem').mockImplementation(() => {
      throw new Error('unavailable');
    });
    expect(loadEditorUriTemplate('goland')).toBeNull();
  });

  it('非空写原文并恰好派发一次；空串保存清除对应键', () => {
    saveEditorUriTemplate('vscode', 'vscode-insiders://file{path}?x=1');
    expect(store.get(VSCODE_URI_TEMPLATE_KEY)).toBe('vscode-insiders://file{path}?x=1');
    expect(events).toBe(1);

    saveEditorUriTemplate('goland', 'goland://open?file={path}');
    expect(events).toBe(2);
    saveEditorUriTemplate('goland', '');
    expect(store.has(GOLAND_URI_TEMPLATE_KEY)).toBe(false);
    expect(events).toBe(3);
  });

  it('模板写失败向上抛出、不派发、不改写', () => {
    store.set(VSCODE_URI_TEMPLATE_KEY, 'vscode://file{path}');
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('quota');
    });
    expect(() => saveEditorUriTemplate('vscode', 'vscode://x{path}')).toThrow('quota');
    expect(store.get(VSCODE_URI_TEMPLATE_KEY)).toBe('vscode://file{path}');
    expect(events).toBe(0);
  });

  it('合法性：含 {path} 且 scheme 前缀属于该工具族', () => {
    expect(isValidUriTemplate('vscode', 'vscode://file{path}')).toBe(true);
    expect(isValidUriTemplate('vscode', 'vscode-insiders://file{path}/')).toBe(true);
    expect(isValidUriTemplate('goland', 'goland://open?file={path}')).toBe(true);
    expect(isValidUriTemplate('goland', 'jetbrains://goland/navigate/reference?project={path}')).toBe(true);
    // 缺占位符
    expect(isValidUriTemplate('vscode', 'vscode://file/')).toBe(false);
    // scheme 不符 / 危险 scheme
    expect(isValidUriTemplate('goland', 'vscode://file{path}')).toBe(false);
    expect(isValidUriTemplate('vscode', 'javascript:alert(1){path}')).toBe(false);
    expect(isValidUriTemplate('vscode', 'file{path}')).toBe(false);
  });
});

describe('buildEditorUri 模板分支（spec「自定义唤起 URI 模板」Scenario 逐条）', () => {
  it('自定义模板生效（本地）：无尾斜杠模板不再追加', () => {
    expect(buildEditorUri('vscode', '/Users/me/my project', 'vscode://file{path}')).toBe(
      'vscode://file/Users/me/my%20project',
    );
  });

  it('远程 SSH 模板（VSCode）：authority 的 + 字面保留、路径分段编码', () => {
    expect(buildEditorUri('vscode', '/home/me/proj', 'vscode://vscode-remote/ssh-remote+devbox{path}')).toBe(
      'vscode://vscode-remote/ssh-remote+devbox/home/me/proj',
    );
  });

  it('缺省模板走内置分支（与内置 VSCode 分支一致）', () => {
    expect(buildEditorUri('vscode', '/Users/me/proj')).toBe('vscode://file/Users/me/proj/');
    expect(buildEditorUri('vscode', '/Users/me/proj', null)).toBe('vscode://file/Users/me/proj/');
  });

  it('非法模板（缺 {path}）回退内置，不改写存储的模板值', () => {
    store.set(VSCODE_URI_TEMPLATE_KEY, 'vscode://file/');
    expect(buildEditorUri('vscode', '/Users/me/proj', 'vscode://file/')).toBe('vscode://file/Users/me/proj/');
    expect(store.get(VSCODE_URI_TEMPLATE_KEY)).toBe('vscode://file/');
  });

  it('scheme 不符（goland 工具配 vscode 模板）回退内置 goland 分支', () => {
    expect(buildEditorUri('goland', '/Users/me/proj', 'vscode://file{path}')).toBe(
      'goland://open?file=%2FUsers%2Fme%2Fproj',
    );
  });

  it('注入值保留开头 /、不做尾斜杠收敛；全部 {path} 均替换', () => {
    expect(buildEditorUri('vscode', '/Users/me/proj//', 'vscode://file{path}/')).toBe(
      'vscode://file/Users/me/proj///',
    );
    expect(buildEditorUri('vscode', '/a b', 'vscode://x{path}y{path}z')).toBe('vscode://x/a%20by/a%20bz');
  });

  it('注入值分段编码：盘符冒号保留字面、空格编码、/ 不编码', () => {
    expect(buildEditorUri('goland', 'C:\\work\\my proj', 'goland://open?file={path}')).toBe(
      'goland://open?file=C:/work/my%20proj',
    );
  });

  it('jetbrains:// 模板（GoLand）：白名单接受且注入生效（回归：曾因白名单仅含 goland:// 被静默回退内置）', () => {
    expect(buildEditorUri('goland', '/Users/me/my project', 'jetbrains://goland/navigate/reference?project={path}')).toBe(
      'jetbrains://goland/navigate/reference?project=/Users/me/my%20project',
    );
  });
});

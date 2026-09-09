// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  buildCustomEditorUri,
  buildEditorUri,
  CUSTOM_TOOLS_KEY,
  DEFAULT_KEY,
  EDITOR_TOOLS_CHANGED,
  GOLAND_KEY,
  GOLAND_URI_TEMPLATE_KEY,
  isUsableCustomTool,
  isValidCustomUriTemplate,
  isValidUriTemplate,
  loadCustomEditorTools,
  loadDefaultTool,
  loadEditorTools,
  loadEditorUriTemplate,
  resolveDefaultTool,
  saveCustomEditorTools,
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
    expect(loadEditorTools()).toEqual({ vscode: false, goland: false, cursor: false });
  });

  it('开启持久化为 1；开启 VSCode 后 GoLand/Cursor 仍为关闭', () => {
    saveEditorTool('vscode', true);
    expect(store.get(VSCODE_KEY)).toBe('1');
    expect(loadEditorTools()).toEqual({ vscode: true, goland: false, cursor: false });
  });

  it('Cursor 开关持久化', () => {
    saveEditorTool('cursor', true);
    expect(loadEditorTools().cursor).toBe(true);
    saveEditorTool('cursor', false);
    expect(loadEditorTools().cursor).toBe(false);
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
    expect(loadEditorTools()).toEqual({ vscode: false, goland: false, cursor: false });
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
    expect(loadEditorTools()).toEqual({ vscode: false, goland: false, cursor: false });
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

  it('custom:<id> 合法（id 非空）；裸 custom: 非法', () => {
    saveDefaultTool('custom:abc');
    expect(store.get(DEFAULT_KEY)).toBe('custom:abc');
    expect(loadDefaultTool()).toBe('custom:abc');
    store.set(DEFAULT_KEY, 'custom:');
    expect(loadDefaultTool()).toBeNull();
    store.set(DEFAULT_KEY, 'custom');
    expect(loadDefaultTool()).toBeNull();
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

describe('resolveDefaultTool（默认编辑器解析：内置 vscode→goland→cursor，自定义殿后）', () => {
  const tools = (vscode: boolean, goland: boolean, cursor = false): EditorTools => ({ vscode, goland, cursor });
  const custom = (id: string, name = id, template = 'myapp://open?path={path}') => ({ id, name, template });

  it('stored 内置合法且已启用 → stored', () => {
    expect(resolveDefaultTool(tools(true, true), [], 'goland')).toEqual({ kind: 'builtin', tool: 'goland' });
    expect(resolveDefaultTool(tools(false, false, true), [], 'cursor')).toEqual({ kind: 'builtin', tool: 'cursor' });
  });

  it('存储的默认工具已被关闭 → 按 vscode → goland → cursor 回退', () => {
    expect(resolveDefaultTool(tools(true, false), [], 'goland')).toEqual({ kind: 'builtin', tool: 'vscode' });
    expect(resolveDefaultTool(tools(false, true), [], 'vscode')).toEqual({ kind: 'builtin', tool: 'goland' });
    expect(resolveDefaultTool(tools(false, false, true), [], 'goland')).toEqual({
      kind: 'builtin',
      tool: 'cursor',
    });
  });

  it('stored custom:<id> 且 id 存在 → 该自定义工具；id 不存在 → 回退', () => {
    const customs = [custom('c1', 'My Editor'), custom('c2')];
    expect(resolveDefaultTool(tools(false, false), customs, 'custom:c1')).toEqual({
      kind: 'custom',
      tool: customs[0],
    });
    expect(resolveDefaultTool(tools(false, false), customs, 'custom:missing')).toEqual({
      kind: 'custom',
      tool: customs[0],
    }); // 回退：无内置启用 → 首个自定义
    expect(resolveDefaultTool(tools(true, false), customs, 'custom:missing')).toEqual({
      kind: 'builtin',
      tool: 'vscode',
    });
  });

  it('关闭后重新开启恢复显式选择（期间未写入 DEFAULT_KEY）', () => {
    saveDefaultTool('goland');
    const closed = resolveDefaultTool(loadEditorTools(), [], loadDefaultTool());
    expect(closed).toBeNull();
    expect(store.get(DEFAULT_KEY)).toBe('goland');
    saveEditorTool('goland', true);
    expect(resolveDefaultTool(loadEditorTools(), [], loadDefaultTool())).toEqual({
      kind: 'builtin',
      tool: 'goland',
    });
  });

  it('无内置启用 → 首个自定义工具（存储顺序）；全无 → null', () => {
    const customs = [custom('c1'), custom('c2')];
    expect(resolveDefaultTool(tools(false, false), customs, null)).toEqual({ kind: 'custom', tool: customs[0] });
    expect(resolveDefaultTool(tools(false, false), [], null)).toBeNull();
    expect(resolveDefaultTool(tools(false, false), [], 'vscode')).toBeNull();
  });

  it('默认键读取异常（null）时按回退规则解析，不抛出', () => {
    expect(resolveDefaultTool(tools(true, true, false), [], null)).toEqual({ kind: 'builtin', tool: 'vscode' });
    expect(resolveDefaultTool(tools(false, true, false), [], null)).toEqual({ kind: 'builtin', tool: 'goland' });
  });

  it('不可用自定义工具（空名/模板非法）不参与解析（C1 统一谓词）', () => {
    const unusable = custom('bad', '', 'myapp://open?path={path}'); // 空名
    const unusable2 = custom('bad2', 'Evil', 'javascript:x{path}'); // 危险模板
    expect(resolveDefaultTool(tools(false, false), [unusable, unusable2], 'custom:bad')).toBeNull();
    expect(resolveDefaultTool(tools(false, false), [unusable, unusable2], null)).toBeNull();
    // 首个自定义回退跳过不可用项
    const usable = custom('ok', 'OK', 'myapp://open?path={path}');
    expect(resolveDefaultTool(tools(false, false), [unusable, usable], null)).toEqual({
      kind: 'custom',
      tool: usable,
    });
  });
});

describe('isUsableCustomTool（C1 可用性唯一谓词）', () => {
  it('名称 trim 非空且模板合法 → 可用', () => {
    expect(isUsableCustomTool({ id: 'c', name: 'My Editor', template: 'myapp://open?path={path}' })).toBe(true);
    expect(isUsableCustomTool({ id: 'c', name: '  X  ', template: 'myapp://open?path={path}' })).toBe(true);
  });

  it('空名/纯空白名/模板非法 → 不可用', () => {
    expect(isUsableCustomTool({ id: 'c', name: '', template: 'myapp://open?path={path}' })).toBe(false);
    expect(isUsableCustomTool({ id: 'c', name: '   ', template: 'myapp://open?path={path}' })).toBe(false);
    expect(isUsableCustomTool({ id: 'c', name: 'X', template: 'myapp://open' })).toBe(false);
    expect(isUsableCustomTool({ id: 'c', name: 'X', template: 'javascript:x{path}' })).toBe(false);
  });
});

describe('loadCustomEditorTools / saveCustomEditorTools（自定义工具列表，ora 无 change 直修）', () => {
  it('缺省回空数组；合法 JSON 数组往返', () => {
    expect(loadCustomEditorTools()).toEqual([]);
    const tools = [
      { id: 'c1', name: 'My Editor', template: 'myapp://open?path={path}' },
      { id: 'c2', name: 'X', template: 'x://y{path}' },
    ];
    saveCustomEditorTools(tools);
    expect(store.get(CUSTOM_TOOLS_KEY)).toBe(JSON.stringify(tools));
    expect(loadCustomEditorTools()).toEqual(tools);
    expect(events).toBe(1); // 成功写入恰好派发一次
  });

  it('容错：JSON 损坏/非数组/元素缺字段 → 跳过或回空数组，不改写存储', () => {
    store.set(CUSTOM_TOOLS_KEY, '{broken');
    expect(loadCustomEditorTools()).toEqual([]);
    store.set(CUSTOM_TOOLS_KEY, '{"id":"c1"}');
    expect(loadCustomEditorTools()).toEqual([]);
    store.set(CUSTOM_TOOLS_KEY, JSON.stringify([
      { id: 'ok', name: 'OK', template: 'a://b{path}' },
      { noId: true },
      { id: '', name: 'X', template: 'a://b{path}' }, // id 空 → 跳过
      { id: 'c2', template: 'a://b{path}' }, // 缺 name → 跳过
      { id: 'c3', name: 'Y' }, // 缺 template → 跳过
      null,
      'str',
    ]));
    expect(loadCustomEditorTools()).toEqual([{ id: 'ok', name: 'OK', template: 'a://b{path}' }]);
    expect(store.has(CUSTOM_TOOLS_KEY)).toBe(true); // 不改写存储
  });

  it('写入失败向上抛出、不派发', () => {
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('quota');
    });
    expect(() => saveCustomEditorTools([])).toThrow('quota');
    expect(events).toBe(0);
  });
});

describe('isValidCustomUriTemplate（自定义模板安全边界）', () => {
  it('合法：通用 scheme 语法 + 含 {path}', () => {
    expect(isValidCustomUriTemplate('myapp://open?path={path}')).toBe(true);
    expect(isValidCustomUriTemplate('a+b.c://x{path}')).toBe(true);
    expect(isValidCustomUriTemplate('cursor://file{path}')).toBe(true);
  });

  it('非法：缺 {path} / 无 scheme / 危险 scheme（大小写不敏感）', () => {
    expect(isValidCustomUriTemplate('myapp://open')).toBe(false);
    expect(isValidCustomUriTemplate('no-scheme{path}')).toBe(false);
    expect(isValidCustomUriTemplate('1app://x{path}')).toBe(false); // scheme 首字符必须字母
    expect(isValidCustomUriTemplate('javascript:alert(1){path}')).toBe(false);
    expect(isValidCustomUriTemplate('JAVASCRIPT:x{path}')).toBe(false);
    expect(isValidCustomUriTemplate('data:text/html,{path}')).toBe(false);
    expect(isValidCustomUriTemplate('vbscript:x{path}')).toBe(false);
    expect(isValidCustomUriTemplate('file:///x{path}')).toBe(false);
  });

  it('非法：前导空白与 scheme 内嵌控制字符（scheme 匹配自模板首字符起）', () => {
    expect(isValidCustomUriTemplate(' myapp://x{path}')).toBe(false); // 前导空格：scheme 不在首位
    expect(isValidCustomUriTemplate('my\napp://x{path}')).toBe(false); // scheme 内嵌 LF
    expect(isValidCustomUriTemplate('myapp\u0000://x{path}')).toBe(false); // scheme 内嵌 NUL
    expect(isValidCustomUriTemplate('java\tscript:x{path}')).toBe(false); // 危险 scheme 变体（内嵌 TAB）
    // 合法对照：scheme 字符集内的 + . - 与大写字母
    expect(isValidCustomUriTemplate('MY+APP.v2://x{path}')).toBe(true);
  });
});

describe('buildCustomEditorUri（自定义工具唤起 URI）', () => {
  const tool = { id: 'c1', name: 'My Editor', template: 'myapp://open?path={path}' };

  it('合法模板：{path} 替换为模板路径注入值（分段编码/盘符冒号保留/保留开头 /）', () => {
    expect(buildCustomEditorUri(tool, '/Users/me/my project')).toBe('myapp://open?path=/Users/me/my%20project');
    expect(buildCustomEditorUri(tool, 'C:\\work\\my proj')).toBe('myapp://open?path=C:/work/my%20proj');
  });

  it('非法模板（缺 {path}/危险 scheme）→ 空串（无内置回退，调用侧不唤起）', () => {
    expect(buildCustomEditorUri({ ...tool, template: 'myapp://open' }, '/tmp/wt')).toBe('');
    expect(buildCustomEditorUri({ ...tool, template: 'javascript:x{path}' }, '/tmp/wt')).toBe('');
  });

  it('空路径 → 空串', () => {
    expect(buildCustomEditorUri(tool, '')).toBe('');
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

  it('Cursor：与 VSCode 分支同构（分段编码、盘符冒号保留、恰好一个尾斜杠）', () => {
    expect(buildEditorUri('cursor', '/Users/me/my project')).toBe('cursor://file/Users/me/my%20project/');
    expect(buildEditorUri('cursor', 'C:\\work\\proj')).toBe('cursor://file/C:/work/proj/');
  });

  it('Cursor：自定义 cursor:// 模板生效', () => {
    expect(buildEditorUri('cursor', '/Users/me/proj', 'cursor://file{path}')).toBe('cursor://file/Users/me/proj');
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

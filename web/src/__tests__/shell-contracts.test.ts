import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import {
  resolveRoute,
  normalizeFrom,
  resolveBackHref,
  isConfigsTab,
  navigate,
  type NavigateEnv,
} from '../router';
import {
  resolveThemePreference,
  resolveEffectiveTheme,
  isThemePreference,
  createStore,
  __resetProjectsStoreForTest,
  __projectsStoreAccessForTest,
  __themeStoreForTest,
  type StoreDeps,
  type StoreSnapshot,
  type StreamHandlers,
} from '../hooks';
import { matchCommand, type PaletteCommand } from '../components/CommandPalette';
import { runProjectMutation, createErrorMessage, deleteErrorMessage } from '../hooks';
import { ApiError } from '../api';
import type { Project } from '../types';

/* ============================ 4.9 契约层纯函数测试 ============================ */

describe('路由解析与重定向', () => {
  it('#/ → 指挥中心（home）', () => {
    expect(resolveRoute('/')).toEqual({ kind: 'page', page: 'home', fragment: undefined });
  });

  it('空 hash 回退 home', () => {
    expect(resolveRoute('')).toEqual({ kind: 'page', page: 'home', fragment: undefined });
  });

  it('#/projects → projects 页', () => {
    expect(resolveRoute('/projects')).toEqual({ kind: 'page', page: 'projects', fragment: undefined });
  });

  it('#/projects#<id> → projects 页带深链 fragment', () => {
    expect(resolveRoute('/projects#abc')).toEqual({ kind: 'page', page: 'projects', fragment: 'abc' });
  });

  it('#/configs → configs 页回退 appearance tab', () => {
    expect(resolveRoute('/configs')).toEqual({ kind: 'page', page: 'configs', fragment: 'appearance' });
  });

  it('#/configs#ai → configs 页选中 ai tab', () => {
    expect(resolveRoute('/configs#ai')).toEqual({ kind: 'page', page: 'configs', fragment: 'ai' });
  });

  it('#/configs#未知tab → 回退 appearance', () => {
    expect(resolveRoute('/configs#foobar')).toEqual({
      kind: 'page',
      page: 'configs',
      fragment: 'appearance',
    });
  });

  it('#/task/:id → task 页带 taskID', () => {
    expect(resolveRoute('/task/abc123')).toEqual({
      kind: 'page',
      page: 'task',
      taskID: 'abc123',
      from: 'home',
      fragment: undefined,
    });
  });

  it('#/task/:id?from=home → from=home', () => {
    expect(resolveRoute('/task/abc?from=home')).toEqual({
      kind: 'page',
      page: 'task',
      taskID: 'abc',
      from: 'home',
      fragment: undefined,
    });
  });

  it('#/task/:id?from=projects → from=projects', () => {
    expect(resolveRoute('/task/abc?from=projects')).toEqual({
      kind: 'page',
      page: 'task',
      taskID: 'abc',
      from: 'projects',
      fragment: undefined,
    });
  });

  it('#/task/:id?from=active → from=home（legacy 别名映射）', () => {
    expect(resolveRoute('/task/abc?from=active')).toEqual({
      kind: 'page',
      page: 'task',
      taskID: 'abc',
      from: 'home',
      fragment: undefined,
    });
  });

  it('#/task/:id?from=未知 → from=home（缺省回退）', () => {
    expect(resolveRoute('/task/abc?from=foobar')).toEqual({
      kind: 'page',
      page: 'task',
      taskID: 'abc',
      from: 'home',
      fragment: undefined,
    });
  });

  it('#/active → 重定向 #/', () => {
    expect(resolveRoute('/active')).toEqual({ kind: 'redirect', target: '/' });
  });

  it('#/ai-config → 重定向 #/configs#ai', () => {
    expect(resolveRoute('/ai-config')).toEqual({ kind: 'redirect', target: '/configs#ai' });
  });

  it('#/project/:id → 重定向 #/projects#<id>', () => {
    expect(resolveRoute('/project/abc')).toEqual({ kind: 'redirect', target: '/projects#abc' });
  });

  it('非法深链回退 home（不报错）', () => {
    expect(resolveRoute('/random/path')).toEqual({ kind: 'page', page: 'home', fragment: undefined });
  });

  it('#/task/:id 带额外 fragment 正常解析', () => {
    expect(resolveRoute('/task/abc?from=home#logs')).toEqual({
      kind: 'page',
      page: 'task',
      taskID: 'abc',
      from: 'home',
      fragment: 'logs',
    });
  });
});

describe('?from 归一映射', () => {
  it('projects 原样保留', () => {
    expect(normalizeFrom('projects')).toBe('projects');
  });

  it('active 映射到 home（legacy 别名）', () => {
    expect(normalizeFrom('active')).toBe('home');
  });

  it('home 原样保留', () => {
    expect(normalizeFrom('home')).toBe('home');
  });

  it('未知值回退 home', () => {
    expect(normalizeFrom('foobar')).toBe('home');
  });

  it('null 回退 home', () => {
    expect(normalizeFrom(null)).toBe('home');
  });

  it('undefined 回退 home', () => {
    expect(normalizeFrom(undefined)).toBe('home');
  });

  it('空串回退 home', () => {
    expect(normalizeFrom('')).toBe('home');
  });

  it('resolveBackHref home → #/', () => {
    expect(resolveBackHref('home')).toBe('/');
  });

  it('resolveBackHref projects 带 projectID → #/projects#<id>', () => {
    expect(resolveBackHref('projects', 'abc')).toBe('/projects#abc');
  });

  it('resolveBackHref projects 无 projectID → #/', () => {
    expect(resolveBackHref('projects')).toBe('/');
  });
});

describe('主题 resolver', () => {
  it('isThemePreference 识别合法值', () => {
    expect(isThemePreference('system')).toBe(true);
    expect(isThemePreference('light')).toBe(true);
    expect(isThemePreference('dark')).toBe(true);
  });

  it('isThemePreference 拒绝非法值', () => {
    expect(isThemePreference('auto')).toBe(false);
    expect(isThemePreference('')).toBe(false);
    expect(isThemePreference(null)).toBe(false);
    expect(isThemePreference(undefined)).toBe(false);
  });

  it('resolveThemePreference 合法值原样返回', () => {
    expect(resolveThemePreference('system')).toBe('system');
    expect(resolveThemePreference('light')).toBe('light');
    expect(resolveThemePreference('dark')).toBe('dark');
  });

  it('resolveThemePreference 非法值回退 system', () => {
    expect(resolveThemePreference('auto')).toBe('system');
    expect(resolveThemePreference(null)).toBe('system');
    expect(resolveThemePreference(undefined)).toBe('system');
    expect(resolveThemePreference('')).toBe('system');
  });

  it('resolveEffectiveTheme system 跟随系统', () => {
    expect(resolveEffectiveTheme('system', true)).toBe('dark');
    expect(resolveEffectiveTheme('system', false)).toBe('light');
  });

  it('resolveEffectiveTheme 显式偏好优先于系统', () => {
    expect(resolveEffectiveTheme('light', true)).toBe('light');
    expect(resolveEffectiveTheme('dark', false)).toBe('dark');
  });
});

describe('useTheme 跨组件共享状态', () => {
  beforeEach(() => {
    __themeStoreForTest.__reset('system');
  });

  it('setPreference 后 getSnapshot 反映新值', () => {
    expect(__themeStoreForTest.getSnapshot()).toBe('system');
    __themeStoreForTest.setPreference('dark');
    expect(__themeStoreForTest.getSnapshot()).toBe('dark');
    __themeStoreForTest.setPreference('light');
    expect(__themeStoreForTest.getSnapshot()).toBe('light');
  });

  it('setPreference 通知已注册订阅者（跨消费者一致）', () => {
    let a = __themeStoreForTest.getSnapshot();
    let b = __themeStoreForTest.getSnapshot();
    const unsubA = __themeStoreForTest.subscribe(() => {
      a = __themeStoreForTest.getSnapshot();
    });
    const unsubB = __themeStoreForTest.subscribe(() => {
      b = __themeStoreForTest.getSnapshot();
    });
    // 模拟两个消费者：任一 setPreference 后两者 snapshot 同步更新
    __themeStoreForTest.setPreference('dark');
    expect(a).toBe('dark');
    expect(b).toBe('dark');
    __themeStoreForTest.setPreference('light');
    expect(a).toBe('light');
    expect(b).toBe('light');
    unsubA();
    unsubB();
  });

  it('相同偏好 setPreference 不重复通知', () => {
    let calls = 0;
    const unsub = __themeStoreForTest.subscribe(() => {
      calls++;
    });
    __themeStoreForTest.setPreference('system'); // 当前已是 system
    expect(calls).toBe(0);
    __themeStoreForTest.setPreference('dark');
    expect(calls).toBe(1);
    __themeStoreForTest.setPreference('dark'); // 同值不通知
    expect(calls).toBe(1);
    unsub();
  });

  it('订阅取消后不再收到通知', () => {
    let v = __themeStoreForTest.getSnapshot();
    const unsub = __themeStoreForTest.subscribe(() => {
      v = __themeStoreForTest.getSnapshot();
    });
    unsub();
    __themeStoreForTest.setPreference('dark');
    expect(v).toBe('system'); // 取消后未更新
  });
});

describe('命令面板匹配器', () => {
  const cmds: PaletteCommand[] = [
    { group: '页面', label: '指挥中心', hint: '首页', href: '/', keywords: 'command center home 首页 活跃' },
    { group: '页面', label: '项目管理', hint: '工作区', href: '/projects', keywords: 'projects workspace 项目 工作区' },
    { group: '页面', label: '设置 · 终端外观', href: '/configs#appearance', keywords: 'settings terminal font 设置 终端 字体' },
    { group: '任务', label: '重构 agent 通信', hint: 'ocdeck', href: '/task/t1', keywords: 'task workbench busy 工作台' },
    { group: '操作', label: '新建任务', hint: '指挥中心', href: '/', keywords: 'new create task 新建 创建' },
  ];

  it('空 query 匹配全部', () => {
    expect(cmds.filter((c) => matchCommand(c, ''))).toHaveLength(5);
  });

  it('中文关键词「设置」匹配设置条目', () => {
    const matched = cmds.filter((c) => matchCommand(c, '设置'));
    expect(matched.some((c) => c.label.includes('设置'))).toBe(true);
  });

  it('中文关键词「项目」匹配项目管理', () => {
    const matched = cmds.filter((c) => matchCommand(c, '项目'));
    expect(matched.some((c) => c.label === '项目管理')).toBe(true);
  });

  it('中文关键词「新建」匹配新建任务操作', () => {
    const matched = cmds.filter((c) => matchCommand(c, '新建'));
    expect(matched.some((c) => c.label === '新建任务')).toBe(true);
  });

  it('英文关键词 home 匹配指挥中心', () => {
    const matched = cmds.filter((c) => matchCommand(c, 'home'));
    expect(matched.some((c) => c.label === '指挥中心')).toBe(true);
  });

  it('多词按空白分词，每个词都须匹配', () => {
    expect(cmds.filter((c) => matchCommand(c, '设置 终端')).some((c) => c.label.includes('终端外观'))).toBe(true);
    expect(cmds.filter((c) => matchCommand(c, '设置 不存在词'))).toHaveLength(0);
  });

  it('无匹配返回空', () => {
    expect(cmds.filter((c) => matchCommand(c, 'zzz不存在'))).toHaveLength(0);
  });

  it('hint 内容参与匹配', () => {
    const matched = cmds.filter((c) => matchCommand(c, '首页'));
    expect(matched.some((c) => c.label === '指挥中心')).toBe(true);
  });

  it('group 内容参与匹配', () => {
    const matched = cmds.filter((c) => matchCommand(c, '操作'));
    expect(matched.some((c) => c.label === '新建任务')).toBe(true);
  });
});

describe('ConfigsTab 类型守卫', () => {
  it('合法 tab', () => {
    expect(isConfigsTab('appearance')).toBe(true);
    expect(isConfigsTab('env')).toBe(true);
    expect(isConfigsTab('opencode')).toBe(true);
    expect(isConfigsTab('ai')).toBe(true);
    expect(isConfigsTab('notifications')).toBe(true);
  });

  it('非法 tab', () => {
    expect(isConfigsTab('foobar')).toBe(false);
    expect(isConfigsTab('')).toBe(false);
    expect(isConfigsTab(null)).toBe(false);
    expect(isConfigsTab(undefined)).toBe(false);
  });
});

describe('共享 store single-flight 与 trailing refresh（驱动生产 createStore）', () => {
  beforeEach(() => {
    __resetProjectsStoreForTest();
  });

  /** 受控 loader：返回按调用顺序排队的 (resolve值|reject函数)。 */
  function makeControlledLoader() {
    const calls: Array<{ resolve: (v: Project[]) => void; reject: (e: Error) => void }> = [];
    let callCount = 0;
    const loader = () =>
      new Promise<Project[]>((resolve, reject) => {
        callCount += 1;
        const idx = callCount - 1;
        calls[idx] = { resolve, reject };
      });
    const resolveCall = (idx: number, value: Project[]) => calls[idx]?.resolve(value);
    const rejectCall = (idx: number, err: Error) => calls[idx]?.reject(err);
    return { loader, resolveCall, rejectCall, getCallCount: () => callCount };
  }

  /** 等待所有微任务排空（doLoad async 链跨多个 await 点，需多轮 flush）。 */
  async function flushMicrotasks(n = 10) {
    for (let i = 0; i < n; i++) await Promise.resolve();
  }

  /** 订阅 handle 快照变化的收集器，返回读取最新快照的函数。 */
  function collectSnapshot(handle: ReturnType<typeof createStore>) {
    let snap: StoreSnapshot = handle.getSnapshot();
    handle.store.subscribe(() => {
      snap = handle.getSnapshot();
    });
    return () => snap;
  }

  it('single-flight：在途期间并发 pollOnce 不并发新 loader，resolve 后仍只 1 次（pollOnce 不设 trailing）', async () => {
    const { loader, resolveCall, getCallCount } = makeControlledLoader();
    const deps: StoreDeps = { loader };
    const handle = createStore(deps);
    // createStore 已启动首次加载（callCount=1）
    expect(getCallCount()).toBe(1);
    // 在途期间并发 pollOnce：不并发新 loader 调用
    void handle.store.pollOnce();
    void handle.store.pollOnce();
    expect(getCallCount()).toBe(1);
    // resolve 首次加载 → pollOnce 不设 trailing，不应补发，callCount 仍为 1
    resolveCall(0, []);
    await flushMicrotasks();
    expect(getCallCount()).toBe(1);
    handle.store.dispose();
  });

  it('trailing refresh：在途时等待补发请求成功后才 settle', async () => {
    const { loader, resolveCall, getCallCount } = makeControlledLoader();
    const deps: StoreDeps = { loader };
    const handle = createStore(deps);
    // 第一次加载在途（createStore 启动时已发起）
    void handle.store.pollOnce(); // 合并到首次（不设 trailing）
    let refreshSettled = false;
    let refreshRejected = false;
    const refreshP = handle.store.refresh();
    refreshP.then(
      () => {
        refreshSettled = true;
      },
      () => {
        refreshRejected = true;
      },
    );
    // 在途期间 refresh 不应 settle
    await flushMicrotasks();
    expect(refreshSettled).toBe(false);
    expect(refreshRejected).toBe(false);
    // 解析首次加载 → 应触发补发（第二次 loader 调用）
    resolveCall(0, []);
    await flushMicrotasks();
    expect(getCallCount()).toBe(2);
    // 补发未完成，refresh 仍不应 settle
    await flushMicrotasks();
    expect(refreshSettled).toBe(false);
    // 解析补发请求（成功）→ refresh resolve
    resolveCall(1, []);
    await refreshP;
    expect(refreshSettled).toBe(true);
    expect(refreshRejected).toBe(false);
    handle.store.dispose();
  });

  it('trailing refresh：补发请求失败时 waiter reject；pendingTrailing 语义不丢（下次触发仍补发）', async () => {
    const { loader, resolveCall, rejectCall, getCallCount } = makeControlledLoader();
    const deps: StoreDeps = { loader };
    const handle = createStore(deps);
    const getSnap = collectSnapshot(handle);
    // 首次成功
    resolveCall(0, [{ id: 'p1', name: 'A', path: '/a', kind: 'repo', default_branch: 'main', created_at: 0, task_count: 0, tasks_by_status: {}, tasks: [] }]);
    await flushMicrotasks();
    expect(getSnap().projects).toHaveLength(1);

    // 在途加载 + refresh（trailing）：补发失败 → waiter reject
    void handle.store.pollOnce(); // 新一轮（callCount=2）
    let refreshRejected = false;
    let refreshErr: Error | undefined;
    const refreshP = handle.store.refresh();
    refreshP.catch((e: Error) => {
      refreshRejected = true;
      refreshErr = e;
    });
    await flushMicrotasks();
    expect(getCallCount()).toBe(2);
    // 解析在途加载成功 → 触发补发（callCount=3）
    resolveCall(1, getSnap().projects);
    await flushMicrotasks();
    expect(getCallCount()).toBe(3);
    // 补发请求失败 → waiter reject
    rejectCall(2, new Error('trailing boom'));
    await flushMicrotasks().catch(() => {});
    // 捕获 refreshP 的 rejection 防止 unhandled
    await refreshP.catch(() => {});
    expect(refreshRejected).toBe(true);
    expect(refreshErr?.message).toBe('trailing boom');
    // store error 暴露
    expect(getSnap().error).toBe('trailing boom');

    // pendingTrailing 语义不丢：refresh 失败后再次 refresh（无在途）应重新加载
    // （补发失败后 inflight=null、pendingTrailing=false，新 refresh 直接加载）
    expect(getCallCount()).toBe(3);
    const refreshP2 = handle.store.refresh();
    await flushMicrotasks();
    expect(getCallCount()).toBe(4);
    resolveCall(3, [{ id: 'p2', name: 'B', path: '/b', kind: 'repo', default_branch: 'main', created_at: 0, task_count: 0, tasks_by_status: {}, tasks: [] }]);
    await refreshP2;
    expect(getSnap().error).toBe('');
    expect(getSnap().projects[0].id).toBe('p2');
    handle.store.dispose();
  });

  it('失败保留旧数据 + error 暴露；成功时 error 清除', async () => {
    const { loader, resolveCall, rejectCall } = makeControlledLoader();
    const deps: StoreDeps = { loader };
    const handle = createStore(deps);
    const getSnap = collectSnapshot(handle);
    // 首次成功
    resolveCall(0, [{ id: 'p1', name: 'A', path: '/a', kind: 'repo', default_branch: 'main', created_at: 0, task_count: 0, tasks_by_status: {}, tasks: [] }]);
    await flushMicrotasks();
    expect(getSnap().projects).toHaveLength(1);
    expect(getSnap().error).toBe('');
    // 第二次加载：失败
    void handle.store.pollOnce(); // 新一轮
    rejectCall(1, new Error('boom'));
    await flushMicrotasks().catch(() => {});
    // 旧数据保留 + error 暴露
    expect(getSnap().projects).toHaveLength(1);
    expect(getSnap().error).toBe('boom');
    // 第三次成功 → error 清除
    void handle.store.pollOnce();
    resolveCall(2, [{ id: 'p2', name: 'B', path: '/b', kind: 'repo', default_branch: 'main', created_at: 0, task_count: 0, tasks_by_status: {}, tasks: [] }]);
    await flushMicrotasks();
    expect(getSnap().error).toBe('');
    expect(getSnap().projects).toHaveLength(1);
    expect(getSnap().projects[0].id).toBe('p2');
    handle.store.dispose();
  });

  it('loading 仅在无数据且请求在途时为 true；有数据后不置 loading', async () => {
    const { loader, resolveCall } = makeControlledLoader();
    const deps: StoreDeps = { loader };
    const handle = createStore(deps);
    const getSnap = collectSnapshot(handle);
    // 首次请求在途 → loading true
    expect(getSnap().loading).toBe(true);
    expect(getSnap().initialized).toBe(false);
    resolveCall(0, []);
    await flushMicrotasks();
    // 有数据后 loading false
    expect(getSnap().loading).toBe(false);
    expect(getSnap().initialized).toBe(true);
    // 第二次轮询（有数据）：不置 loading
    void handle.store.pollOnce();
    expect(getSnap().loading).toBe(false);
    resolveCall(1, []);
    await flushMicrotasks();
    expect(getSnap().loading).toBe(false);
    handle.store.dispose();
  });

  it('实例隔离：两个 createStore 实例独立快照/监听', async () => {
    const { loader: loader1, resolveCall: resolve1 } = makeControlledLoader();
    const { loader: loader2, resolveCall: resolve2 } = makeControlledLoader();
    const h1 = createStore({ loader: loader1 });
    const h2 = createStore({ loader: loader2 });
    const snap1 = collectSnapshot(h1);
    const snap2 = collectSnapshot(h2);
    resolve1(0, [{ id: 'a', name: 'A', path: '/a', kind: 'repo', default_branch: 'main', created_at: 0, task_count: 0, tasks_by_status: {}, tasks: [] }]);
    resolve2(0, [{ id: 'b', name: 'B', path: '/b', kind: 'repo', default_branch: 'main', created_at: 0, task_count: 0, tasks_by_status: {}, tasks: [] }]);
    await flushMicrotasks();
    expect(snap1().projects[0]?.id).toBe('a');
    expect(snap2().projects[0]?.id).toBe('b');
    h1.store.dispose();
    h2.store.dispose();
  });

  it('dispose 释放定时器（startTimer 清理函数被调用）', () => {
    let stopped = false;
    const { loader } = makeControlledLoader();
    const handle = createStore({
      loader,
      startTimer: () => () => {
        stopped = true;
      },
    });
    handle.store.dispose();
    expect(stopped).toBe(true);
  });

  it('mutation 成功 + refresh 失败 → mutation 呈现成功、不显示 mutation 错误', async () => {
    // 驱动生产 runProjectMutation：API 成功 → onSuccess → refresh（失败静默）。
    // mutation 呈成功（返回 true），mutationError 不被设置，refresh 失败进 store error 通道。
    const { loader, resolveCall, rejectCall } = makeControlledLoader();
    const handle = createStore({ loader });
    const getSnap = collectSnapshot(handle);
    // 首次加载成功（初始数据）
    resolveCall(0, [{ id: 'p1', name: 'A', path: '/a', kind: 'repo', default_branch: 'main', created_at: 0, task_count: 0, tasks_by_status: {}, tasks: [] }]);
    await flushMicrotasks();
    expect(getSnap().error).toBe('');

    let mutationError = '';
    let successSideEffect = false;
    // 驱动生产 runProjectMutation：模拟 mutation API 成功
    const ok = await runProjectMutation({
      mutate: async () => {
        /* API 成功 */
      },
      refresh: () => handle.store.refresh(),
      onSuccess: () => {
        successSideEffect = true;
      },
      onError: (err) => {
        mutationError = createErrorMessage(err);
      },
    });
    expect(ok).toBe(true);
    expect(successSideEffect).toBe(true);

    // refresh 在途（无 inflight 时直接发起）：触发 refresh → loader 调用
    await flushMicrotasks();
    // refresh 加载失败
    rejectCall(1, new Error('refresh boom'));
    await flushMicrotasks().catch(() => {});
    // mutation 仍呈成功、mutation 错误为空（refresh 失败不回落 mutation 文案）
    expect(mutationError).toBe('');
    // refresh 失败暴露在 store error 通道（storeError），由页面 storeError 展示
    expect(getSnap().error).toBe('refresh boom');
    handle.store.dispose();
  });

  it('mutation 失败 → 报 mutation 错误且不 refresh（API 失败不触发 trailing refresh）', async () => {
    // 驱动生产 runProjectMutation：API 失败 → onError 报 mutation 错误、不调 refresh。
    const { loader, resolveCall, getCallCount } = makeControlledLoader();
    const handle = createStore({ loader });
    const getSnap = collectSnapshot(handle);
    // 首次加载成功（初始数据，保证后续无 inflight 干扰）
    resolveCall(0, [{ id: 'p1', name: 'A', path: '/a', kind: 'repo', default_branch: 'main', created_at: 0, task_count: 0, tasks_by_status: {}, tasks: [] }]);
    await flushMicrotasks();
    const callCountBefore = getCallCount();

    let mutationError = '';
    let successSideEffect = false;
    const apiErr = new ApiError(409, 'conflict', 'project has tasks');
    const ok = await runProjectMutation({
      mutate: async () => {
        throw apiErr;
      },
      refresh: () => handle.store.refresh(),
      onSuccess: () => {
        successSideEffect = true;
      },
      onError: (err) => {
        mutationError = deleteErrorMessage(err);
      },
    });
    expect(ok).toBe(false);
    expect(successSideEffect).toBe(false);
    // 报 mutation 错误（删除 409 → 项目下仍有任务）
    expect(mutationError).toBe('项目下仍有任务，无法删除');
    // 不触发 refresh（API 失败不调 trailing refresh）
    await flushMicrotasks();
    expect(getCallCount()).toBe(callCountBefore);
    expect(getSnap().error).toBe('');
    handle.store.dispose();
  });
});

describe('共享 store SSE 订阅与 30s 兜底轮询（projects-stream design D6）', () => {
  beforeEach(() => {
    __resetProjectsStoreForTest();
  });

  const p = (id: string): Project => ({
    id,
    name: id,
    path: '/p/' + id,
    kind: 'repo',
    default_branch: 'main',
    created_at: 0,
    task_count: 0,
    tasks_by_status: {},
    tasks: [],
  });

  /** 等待所有微任务排空（doLoad async 链跨多个 await 点）。 */
  async function flushMicrotasks(n = 10) {
    for (let i = 0; i < n; i++) await Promise.resolve();
  }

  /** 捕获流回调的 fake streamFactory：工厂创建即被调用，close 为 spy。 */
  function fakeStream() {
    let handlers!: StreamHandlers;
    const close = vi.fn();
    const factory = vi.fn((h: StreamHandlers) => {
      handlers = h;
      return { close };
    });
    return {
      factory,
      close,
      emitData: (projects: Project[]) => handlers.onData(projects),
      emitError: (message: string) => handlers.onError(message),
    };
  }

  it('store 创建即建立流订阅；dispose 关闭订阅', () => {
    const stream = fakeStream();
    const handle = createStore({ loader: async () => [], streamFactory: stream.factory });
    expect(stream.factory).toHaveBeenCalledTimes(1);
    handle.store.dispose();
    expect(stream.close).toHaveBeenCalledTimes(1);
  });

  it('snapshot/update 帧整表替换快照：首帧即 initialized、清 error，update 覆盖', async () => {
    const stream = fakeStream();
    const handle = createStore({ loader: async () => [], streamFactory: stream.factory });
    stream.emitError('项目连接中断'); // 首帧前错误 → error 通道
    expect(handle.getSnapshot().error).toBe('项目连接中断');
    stream.emitData([p('a')]); // snapshot 帧
    let snap = handle.getSnapshot();
    expect(snap.projects.map((x) => x.id)).toEqual(['a']);
    expect(snap.initialized).toBe(true);
    expect(snap.loading).toBe(false);
    expect(snap.error).toBe('');
    stream.emitData([p('b')]); // update 帧（整表替换，非合并）
    snap = handle.getSnapshot();
    expect(snap.projects.map((x) => x.id)).toEqual(['b']);
    handle.store.dispose();
  });

  it('流失败静默保留旧数据：error 暴露、projects 不清空', async () => {
    const stream = fakeStream();
    const handle = createStore({ loader: async () => [], streamFactory: stream.factory });
    stream.emitData([p('a')]);
    stream.emitError('项目连接中断');
    const snap = handle.getSnapshot();
    expect(snap.projects).toHaveLength(1); // 保留旧数据（侧栏不闪空态）
    expect(snap.error).toBe('项目连接中断');
    expect(snap.loading).toBe(false);
    handle.store.dispose();
  });

  it('兜底轮询保持数据新鲜：注入 timer 触发仍走 loader（single-flight pollOnce 不变）', async () => {
    const stream = fakeStream();
    let loadCount = 0;
    let latest = [p('x1')];
    const loader = async () => {
      loadCount += 1;
      return latest;
    };
    let fire!: () => void;
    const handle = createStore({
      loader,
      startTimer: (f) => {
        fire = f;
        return () => {};
      },
      streamFactory: stream.factory,
    });
    expect(loadCount).toBe(1); // createStore 首次加载
    await flushMicrotasks(); // 等首次加载 settle（否则 fire 被合并进在途请求）
    latest = [p('x2')]; // 模拟流外变更（项目 CRUD 无事件），仅 REST 可见
    fire(); // 兜底周期 tick
    await flushMicrotasks();
    expect(loadCount).toBe(2);
    expect(handle.getSnapshot().projects.map((x) => x.id)).toEqual(['x2']);
    fire();
    await flushMicrotasks();
    expect(loadCount).toBe(3);
    handle.store.dispose();
  });

  it('流订阅存在时 refresh() 仍立即触发 REST 重载（用户主动操作立即收敛）', async () => {
    const stream = fakeStream();
    let loadCount = 0;
    const loader = async () => {
      loadCount += 1;
      return [];
    };
    const handle = createStore({ loader, streamFactory: stream.factory });
    expect(loadCount).toBe(1);
    await handle.store.refresh();
    expect(loadCount).toBe(2);
    handle.store.dispose();
  });

  it('兜底周期源码契约：30s 常量、不存在固定 5s 轮询', () => {
    const src = readFileSync(fileURLToPath(new URL('../hooks.ts', import.meta.url)), 'utf8');
    expect(src).toContain('PROJECTS_FALLBACK_POLL_MS = 30000');
    expect(src).not.toMatch(/setInterval\(fire, 5000\)/);
  });
});

describe('共享 store 401 终止与懒重建（UNAUTHORIZED_EVENT → reset → 重新认证后重建）', () => {
  type Listener = (e: Event) => void;
  let listeners: Listener[];

  beforeEach(() => {
    __resetProjectsStoreForTest();
    listeners = [];
    // fake window：只承接 addEventListener（getStoreHandle 首建时的 401 重置绑定）
    vi.stubGlobal('window', {
      addEventListener: (_type: string, l: Listener) => listeners.push(l),
      removeEventListener: (_type: string, _l: Listener) => {},
      dispatchEvent: () => true,
    });
  });
  afterEach(() => {
    __resetProjectsStoreForTest();
    vi.unstubAllGlobals();
  });

  /** 计数 loader + 计数流工厂（close spy）。 */
  function makeDeps() {
    const counts = { loads: 0, streams: 0, closes: 0 };
    const deps: StoreDeps = {
      loader: async () => {
        counts.loads += 1;
        return [];
      },
      streamFactory: () => {
        counts.streams += 1;
        return {
          close: () => {
            counts.closes += 1;
          },
        };
      },
    };
    return { deps, counts };
  }

  /** 模拟 401：api.ts request()/sse.ts 流在 clearToken 后派发 UNAUTHORIZED_EVENT。 */
  const fireUnauthorized = () => {
    for (const l of listeners) l(new Event('ocdeck:unauthorized'));
  };

  it('流 401 → 事件释放 store；重新认证后再次访问懒重建（新工厂 + 立即首次加载），事件双发不重复订阅', async () => {
    const a = makeDeps();
    const h1 = __projectsStoreAccessForTest(a.deps);
    expect(a.counts.streams).toBe(1); // 创建即订阅
    expect(a.counts.loads).toBe(1); // 立即首次加载
    expect(listeners).toHaveLength(1); // 首建即绑定 401 重置（一次）

    fireUnauthorized(); // 模拟流 401：sse 核心已 clearToken + dispatch，订阅永久终止
    fireUnauthorized(); // 并发 REST 401 同事件双发：reset 幂等
    expect(a.counts.closes).toBe(1); // 旧 store 流被关闭（且仅一次）
    expect(listeners).toHaveLength(1); // 不重复绑定

    // 重新认证后的消费方首次访问：懒重建，走 getStoreHandle 真实路径
    const b = makeDeps();
    const h2 = __projectsStoreAccessForTest(b.deps);
    expect(h2).not.toBe(h1);
    expect(b.counts.streams).toBe(1); // 新流恰好一次（无双订阅）
    expect(b.counts.loads).toBe(1); // 首次加载恢复（loader 立即 resolve）
    for (let i = 0; i < 10; i++) await Promise.resolve();
    expect(h2.getSnapshot().initialized).toBe(true);
  });

  it('单例复用：事件之外重复访问不重建（同一句柄、工厂只调一次）', () => {
    const a = makeDeps();
    const h1 = __projectsStoreAccessForTest(a.deps);
    const h2 = __projectsStoreAccessForTest(a.deps);
    expect(h2).toBe(h1);
    expect(a.counts.streams).toBe(1);
    expect(a.counts.loads).toBe(1);
  });
});

describe('navigate replace 历史行为', () => {
  /** fake history：记录 replaceState/pushHash 调用，无 jsdom 依赖。 */
  function makeFakeEnv(): NavigateEnv & {
    replaces: string[];
    pushes: string[];
    dispatched: number;
  } {
    return {
      replaces: [],
      pushes: [],
      dispatched: 0,
      replaceState(_data: unknown, _unused: string, url: string) {
        this.replaces.push(url);
      },
      pushHash(hash: string) {
        this.pushes.push(hash);
      },
      dispatchHashChange() {
        this.dispatched += 1;
      },
    };
  }

  it('replace=true 走 replaceState 且不新增历史项（重定向用）', () => {
    const env = makeFakeEnv();
    navigate('/projects#abc', true, env);
    expect(env.replaces).toEqual(['#/projects#abc']);
    expect(env.pushes).toHaveLength(0);
    // replaceState 不触发 hashchange，navigate 手动派发一次
    expect(env.dispatched).toBe(1);
  });

  it('replace=false 走 pushHash（正常导航新增历史项）', () => {
    const env = makeFakeEnv();
    navigate('/projects', false, env);
    expect(env.pushes).toEqual(['#/projects']);
    expect(env.replaces).toHaveLength(0);
    expect(env.dispatched).toBe(0);
  });

  it('重定向目标正确（replace 导航不污染返回栈）', () => {
    expect(resolveRoute('/active')).toEqual({ kind: 'redirect', target: '/' });
    expect(resolveRoute('/project/abc')).toEqual({ kind: 'redirect', target: '/projects#abc' });
  });
});

/* ============================ 命令面板 palette-focus 信号 ============================ */

import {
  emitPaletteFocus,
  consumePendingPaletteFocus,
  clearPendingPaletteFocus,
  PALETTE_FOCUS_EVENT,
  __resetPaletteFocusForTest,
} from '../palette-focus';

describe('od:palette-focus（命令面板展开+聚焦）', () => {
  beforeEach(() => {
    __resetPaletteFocusForTest();
  });

  it('emit 写入 pending；consume 一次后清空（跨路由 mount 兜底）', () => {
    emitPaletteFocus('new-task-name');
    expect(consumePendingPaletteFocus('new-task-name')).toBe(true);
    expect(consumePendingPaletteFocus('new-task-name')).toBe(false);
  });

  it('listener 处理后 clearPending，mount 不再二次消费', () => {
    emitPaletteFocus('register-project-name');
    clearPendingPaletteFocus('register-project-name');
    expect(consumePendingPaletteFocus('register-project-name')).toBe(false);
  });

  it('consume 只匹配 expected id', () => {
    emitPaletteFocus('new-task-name');
    expect(consumePendingPaletteFocus('register-project-name')).toBe(false);
    expect(consumePendingPaletteFocus('new-task-name')).toBe(true);
  });

  it('事件名常量对齐 design 源', () => {
    expect(PALETTE_FOCUS_EVENT).toBe('od:palette-focus');
  });
});
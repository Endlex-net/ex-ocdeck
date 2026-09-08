// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act, useState } from 'react';
import { SystemEnvPanel } from '../components/SystemEnvPanel';
import { GlobalEnvEditor } from '../components/GlobalEnvEditor';
import { api, ApiError } from '../api';
import type { GlobalEnvMode, GlobalEnvVar, HostEnvVar } from '../types';
import { flushUI, mount, rerender } from './cm-test-env';

/* ============================ SystemEnvPanel 行为验收（host-env-sync-and-display tasks 3.6） ============================
 * 逐场景覆盖 spec「系统环境变量展示区」：
 * 首次 loading、默认掩码/点击切换、搜索过滤、添加按钮四态优先级
 * （已配置 → 系统保留 → 非法 key → 可添加）、刷新成功联动、刷新失败保留原列表。
 * 另含评审修复回归：K1（全局区增删改同步系统区「已配置」标注，防 manual 被 upsert 覆盖）、
 * K2（配置集未就绪/失败时添加不放行；添加后即时标记；迟到旧响应不覆盖新状态）。 */

vi.mock('../api', async (importOriginal) => {
  const orig = await importOriginal<typeof import('../api')>();
  return {
    ...orig,
    api: {
      getHostEnv: vi.fn(async () => ({ vars: [] })),
      refreshHostEnv: vi.fn(async () => ({ vars: [] })),
      getGlobalEnv: vi.fn(async () => ({ vars: [], restartRequired: false })),
      putGlobalEnv: vi.fn(async () => ({ restartRequired: false })),
      deleteGlobalEnv: vi.fn(async () => ({ restartRequired: false })),
    },
  };
});

const mocked = api as unknown as {
  getHostEnv: ReturnType<typeof vi.fn>;
  refreshHostEnv: ReturnType<typeof vi.fn>;
  getGlobalEnv: ReturnType<typeof vi.fn>;
  putGlobalEnv: ReturnType<typeof vi.fn>;
  deleteGlobalEnv: ReturnType<typeof vi.fn>;
};

const HOST_VARS: HostEnvVar[] = [
  { key: 'PATH', value: '/usr/bin:/bin', source: 'both' },
  { key: 'NPM_TOKEN', value: 'secret-token', source: 'shell' },
  { key: 'OCDECK_TOKEN', value: 'internal', source: 'process' },
  { key: 'OPENCODE_SERVER_PASSWORD', value: 'pw', source: 'process' },
  { key: '1BAD-KEY', value: 'x', source: 'shell' },
  { key: 'HOME', value: '/Users/me', source: 'process' },
];

const GLOBAL_VARS: GlobalEnvVar[] = [
  { key: 'PATH', mode: 'follow_host', value: '', resolvedValue: '/usr/bin:/bin' },
];

function hostVars(vars: HostEnvVar[] = HOST_VARS) {
  return { vars };
}

function globalVars(vars: GlobalEnvVar[] = GLOBAL_VARS) {
  return { vars, restartRequired: false };
}

function renderPanel(onGlobalChanged: () => void = () => {}) {
  return mount(<SystemEnvPanel reloadTick={0} onGlobalChanged={onGlobalChanged} />);
}

function rowOf(container: HTMLElement, key: string): HTMLElement {
  const row = [...container.querySelectorAll<HTMLElement>('.env-row')].find(
    (r) => r.querySelector('.env-key')?.textContent === key,
  );
  expect(row, `行「${key}」存在`).toBeTruthy();
  return row!;
}

function valueBtn(row: HTMLElement): HTMLButtonElement {
  return row.querySelector<HTMLButtonElement>('.env-value-btn')!;
}

function addBtn(row: HTMLElement): HTMLButtonElement {
  const btns = [...row.querySelectorAll<HTMLButtonElement>('button')].filter(
    (b) => !b.classList.contains('env-value-btn'),
  );
  expect(btns).toHaveLength(1);
  return btns[0];
}

/** jsdom 受控 input 赋值（React 受控组件走原生 setter + input 事件）。 */
function setInput(input: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
  act(() => {
    setter.call(input, value);
    input.dispatchEvent(new Event('input', { bubbles: true }));
  });
}

/** 复刻 SettingsPage 共享 reload 信号接线的组合 harness（K1 验收）。 */
function EnvHarness() {
  const [tick, setTick] = useState(0);
  const bump = () => setTick((t) => t + 1);
  return (
    <>
      <GlobalEnvEditor reloadTick={tick} onChanged={bump} />
      <SystemEnvPanel reloadTick={tick} onGlobalChanged={bump} />
    </>
  );
}

/** 仅系统面板、共享信号自接（添加后触发 reload 场景）。 */
function PanelHarness() {
  const [tick, setTick] = useState(0);
  return <SystemEnvPanel reloadTick={tick} onGlobalChanged={() => setTick((t) => t + 1)} />;
}

/** 组合 harness 中按渲染顺序取全局区/系统区根节点。 */
function panels(container: HTMLElement) {
  const editors = container.querySelectorAll<HTMLElement>('.env-editor');
  expect(editors.length).toBe(2);
  return { global: editors[0], system: editors[1] };
}

function globalVarsOf(vars: GlobalEnvVar[]) {
  return { vars, restartRequired: false };
}

beforeEach(() => {
  vi.clearAllMocks();
  mocked.getHostEnv.mockResolvedValue(hostVars());
  mocked.refreshHostEnv.mockResolvedValue(hostVars());
  mocked.getGlobalEnv.mockResolvedValue(globalVars());
  mocked.putGlobalEnv.mockResolvedValue({ restartRequired: false });
});

describe('首次加载', () => {
  it('加载期间展示 loading，完成后呈现合并视图', async () => {
    let resolve!: (v: { vars: HostEnvVar[] }) => void;
    mocked.getHostEnv.mockReturnValue(new Promise((r) => (resolve = r)));
    const { container, unmount } = renderPanel();
    expect(container.textContent).toContain('正在读取宿主环境变量');
    expect(container.querySelector('.env-list')).toBeNull();
    await act(async () => resolve(hostVars()));
    await flushUI();
    expect(container.textContent).not.toContain('正在读取宿主环境变量');
    expect(container.querySelectorAll('.env-row')).toHaveLength(HOST_VARS.length);
    unmount();
  });

  it('加载失败显示错误提示', async () => {
    mocked.getHostEnv.mockRejectedValue(new ApiError(500, 'internal', 'capture failed'));
    const { container, unmount } = renderPanel();
    await flushUI();
    expect(container.querySelector('.error-line')?.textContent).toContain('capture failed');
    expect(container.querySelector('.env-list')).toBeNull();
    unmount();
  });
});

describe('掩码与点击切换', () => {
  it('默认定长掩码，点击行值区域显示明文，再点恢复掩码', async () => {
    const { container, unmount } = renderPanel();
    await flushUI();
    const row = rowOf(container, 'NPM_TOKEN');
    const btn = valueBtn(row);
    // 定长掩码：不泄露值长度
    expect(btn.textContent).toBe('••••••••');
    act(() => btn.click());
    expect(btn.textContent).toBe('secret-token');
    act(() => btn.click());
    expect(btn.textContent).toBe('••••••••');
    unmount();
  });

  it('行级状态：切换一行不影响其他行', async () => {
    const { container, unmount } = renderPanel();
    await flushUI();
    act(() => valueBtn(rowOf(container, 'HOME')).click());
    expect(valueBtn(rowOf(container, 'HOME')).textContent).toBe('/Users/me');
    expect(valueBtn(rowOf(container, 'PATH')).textContent).toBe('••••••••');
    unmount();
  });
});

describe('来源 badge', () => {
  it('process / shell / both 分别标注 进程 / Shell / 两者', async () => {
    const { container, unmount } = renderPanel();
    await flushUI();
    expect(rowOf(container, 'HOME').querySelector('.badge')?.textContent).toBe('进程');
    expect(rowOf(container, 'NPM_TOKEN').querySelector('.badge')?.textContent).toBe('Shell');
    expect(rowOf(container, 'PATH').querySelector('.badge')?.textContent).toBe('两者');
    unmount();
  });
});

describe('搜索过滤', () => {
  it('按 key 子串过滤，无匹配给出提示', async () => {
    const { container, unmount } = renderPanel();
    await flushUI();
    const input = container.querySelector<HTMLInputElement>('.env-toolbar input')!;
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
    act(() => {
      setter.call(input, 'OCDECK');
      input.dispatchEvent(new Event('input', { bubbles: true }));
    });
    const keys = [...container.querySelectorAll('.env-key')].map((el) => el.textContent);
    expect(keys).toEqual(['OCDECK_TOKEN']);
    act(() => {
      setter.call(input, '不存在');
      input.dispatchEvent(new Event('input', { bubbles: true }));
    });
    expect(container.querySelectorAll('.env-row')).toHaveLength(0);
    expect(container.textContent).toContain('无匹配');
    unmount();
  });
});

describe('添加按钮四态优先级', () => {
  it('已配置 → 系统保留 → 非法 key → 可添加，前三态禁用并标注原因', async () => {
    const { container, unmount } = renderPanel();
    await flushUI();
    // 已配置（PATH 同时也是合法 key，验证优先级最高）
    const configuredBtn = addBtn(rowOf(container, 'PATH'));
    expect(configuredBtn.textContent).toBe('已配置');
    expect(configuredBtn.disabled).toBe(true);
    // 系统保留：OCDECK_* 前缀与 OPENCODE_SERVER_PASSWORD
    const reservedPrefix = addBtn(rowOf(container, 'OCDECK_TOKEN'));
    expect(reservedPrefix.textContent).toBe('系统保留');
    expect(reservedPrefix.disabled).toBe(true);
    const reservedExact = addBtn(rowOf(container, 'OPENCODE_SERVER_PASSWORD'));
    expect(reservedExact.textContent).toBe('系统保留');
    expect(reservedExact.disabled).toBe(true);
    // 非法 key（未配置、非保留）
    const invalidBtn = addBtn(rowOf(container, '1BAD-KEY'));
    expect(invalidBtn.textContent).toBe('非法 key');
    expect(invalidBtn.disabled).toBe(true);
    // 可添加
    const okBtn = addBtn(rowOf(container, 'HOME'));
    expect(okBtn.textContent).toBe('添加');
    expect(okBtn.disabled).toBe(false);
    // 禁用行仍正常展示（键并集契约）
    expect(rowOf(container, 'OCDECK_TOKEN').querySelector('.env-key')?.textContent).toBe(
      'OCDECK_TOKEN',
    );
    unmount();
  });

  it('点击添加调 putGlobalEnv(key, follow_host, 空值) 并通知全局列表重载', async () => {
    const onGlobalChanged = vi.fn();
    const { container, unmount } = renderPanel(onGlobalChanged);
    await flushUI();
    await act(async () => {
      addBtn(rowOf(container, 'HOME')).click();
    });
    expect(mocked.putGlobalEnv).toHaveBeenCalledWith('HOME', 'follow_host', '');
    expect(onGlobalChanged).toHaveBeenCalledTimes(1);
    unmount();
  });
});

describe('刷新', () => {
  it('成功：替换列表并触发全局列表重载联动', async () => {
    const onGlobalChanged = vi.fn();
    const { container, unmount } = renderPanel(onGlobalChanged);
    await flushUI();
    mocked.refreshHostEnv.mockResolvedValue(
      hostVars([{ key: 'NEW_VAR', value: 'v2', source: 'shell' }]),
    );
    const refreshBtn = [...container.querySelectorAll<HTMLButtonElement>('.env-toolbar button')][0];
    expect(refreshBtn.textContent).toBe('刷新');
    await act(async () => {
      refreshBtn.click();
    });
    expect(mocked.refreshHostEnv).toHaveBeenCalledTimes(1);
    expect(onGlobalChanged).toHaveBeenCalledTimes(1);
    expect(container.querySelectorAll('.env-row')).toHaveLength(1);
    expect(container.querySelector('.env-key')?.textContent).toBe('NEW_VAR');
    unmount();
  });

  it('失败：保留原列表 + 错误提示，不触发联动', async () => {
    const onGlobalChanged = vi.fn();
    const { container, unmount } = renderPanel(onGlobalChanged);
    await flushUI();
    mocked.refreshHostEnv.mockRejectedValue(new ApiError(500, 'internal', 'refresh failed'));
    const refreshBtn = [...container.querySelectorAll<HTMLButtonElement>('.env-toolbar button')][0];
    await act(async () => {
      refreshBtn.click();
    });
    expect(onGlobalChanged).not.toHaveBeenCalled();
    expect(container.querySelectorAll('.env-row')).toHaveLength(HOST_VARS.length);
    expect(container.querySelector('.error-line')?.textContent).toContain('refresh failed');
    unmount();
  });
});

describe('固定说明文案', () => {
  it('展示刷新生效时机说明', async () => {
    const { container, unmount } = renderPanel();
    await flushUI();
    expect(container.querySelector('.env-hint')?.textContent).toContain(
      '刷新更新宿主解析结果；任务需挂起后激活生效。',
    );
    unmount();
  });
});

describe('L2：全局区响应时效（oracle 收尾）', () => {
  it('旧请求 A 迟到不覆盖 tick 广播后新请求 B 的结果（resolvedValue 保持新值）', async () => {
    // A：GlobalEnvEditor 初始 load 挂起（旧缓存响应）
    let resolveA!: (v: { vars: GlobalEnvVar[]; restartRequired: boolean }) => void;
    mocked.getGlobalEnv.mockReturnValueOnce(
      new Promise((r) => (resolveA = r)),
    );
    const { container, unmount } = mount(<EnvHarness />);
    await flushUI();
    const { global, system } = panels(container);
    // 系统区刷新成功 → tick 广播 → 全局区启动新 load B（返回新 resolvedValue）
    mocked.getGlobalEnv.mockImplementation(async () =>
      globalVarsOf([
        { key: 'NEW_KEY', mode: 'follow_host', value: '', resolvedValue: 'B-VALUE' },
      ]),
    );
    const refreshBtn = [...system.querySelectorAll<HTMLButtonElement>('.env-toolbar button')][0];
    await act(async () => {
      refreshBtn.click();
    });
    await flushUI();
    expect(global.textContent).toContain('B-VALUE');
    // 旧 A 随后到达：被丢弃，全局区保持 B 的结果
    await act(async () =>
      resolveA(
        globalVarsOf([
          { key: 'OLD_KEY', mode: 'follow_host', value: '', resolvedValue: 'A-OLD' },
        ]),
      ),
    );
    await flushUI();
    expect(global.textContent).toContain('B-VALUE');
    expect(global.textContent).not.toContain('A-OLD');
    expect(global.textContent).not.toContain('OLD_KEY');
    unmount();
  });
});

describe('K1/K2 残留：重载在途窗口（oracle Round 2）', () => {
  /** 公共场景：全局区把 HOME 保存为 manual，保存成功后系统区 reload 挂起/失败。 */
  async function setupSaveWindow(opts: { initial: GlobalEnvVar[] }) {
    let current: GlobalEnvVar[] = opts.initial;
    mocked.getGlobalEnv.mockImplementation(async () => globalVarsOf(current));
    mocked.putGlobalEnv.mockImplementation(async (key: string, mode: string, value: string) => {
      current = [...current.filter((v) => v.key !== key), { key, mode: mode as GlobalEnvMode, value, resolvedValue: '' }];
      return { restartRequired: false };
    });
    const mounted = mount(<EnvHarness />);
    await flushUI();
    const { global, system } = panels(mounted.container);
    // 保存前：HOME 可添加
    expect(addBtn(rowOf(system, 'HOME')).disabled).toBe(false);
    // 全局区以 manual 保存 HOME
    setInput(
      global.querySelector<HTMLInputElement>('form.env-add input[placeholder="KEY"]')!,
      'HOME',
    );
    act(() =>
      [...global.querySelectorAll<HTMLButtonElement>('form.env-add .env-mode-opt')]
        .find((b) => b.textContent === '手动配置')!
        .click(),
    );
    setInput(
      global.querySelector<HTMLInputElement>('form.env-add input[placeholder="value"]')!,
      'keep-me',
    );
    return { mounted, global, system, submitSave: async () => {
      await act(async () => {
        global
          .querySelector('form.env-add')!
          .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
      });
    }, getCurrent: () => current };
  }

  it('tick 广播后配置集 pending 期间：不可添加、点击不产生第二次 PUT；成功后按新集合恢复', async () => {
    const { mounted, system, submitSave } = await setupSaveWindow({ initial: [] });
    // 保存成功后的 reload 挂起（GlobalEnvEditor 与 SystemEnvPanel 共享同一 pending）
    let resolveReload!: (v: { vars: GlobalEnvVar[]; restartRequired: boolean }) => void;
    const pending = new Promise<{ vars: GlobalEnvVar[]; restartRequired: boolean }>(
      (r) => (resolveReload = r),
    );
    mocked.getGlobalEnv.mockImplementation(async () => pending);
    await submitSave();
    await flushUI();
    expect(mocked.putGlobalEnv).toHaveBeenCalledWith('HOME', 'manual', 'keep-me');
    // 窗口期：旧集合（无 HOME）只供展示，无添加资格
    const homeAdd = addBtn(rowOf(system, 'HOME'));
    expect(homeAdd.disabled).toBe(true);
    expect(homeAdd.title).toContain('正在读取全局配置');
    const putCalls = mocked.putGlobalEnv.mock.calls.length;
    await act(async () => {
      homeAdd.click();
    });
    expect(mocked.putGlobalEnv.mock.calls.length).toBe(putCalls);
    // reload 成功返回新集合（含 HOME manual）→ 按新集合显示已配置
    await act(async () => resolveReload(globalVarsOf([
      { key: 'HOME', mode: 'manual', value: 'keep-me', resolvedValue: '' },
    ])));
    await flushUI();
    expect(addBtn(rowOf(system, 'HOME')).textContent).toBe('已配置');
    expect(addBtn(rowOf(system, 'HOME')).disabled).toBe(true);
    mounted.unmount();
  });

  it('tick 广播后 reload 失败：普通 key 不放行并提示；已知行级原因保留（K5）', async () => {
    // 旧集合含 PATH（已配置），用于验证已知标签在失败态下保留
    const { mounted, system, submitSave } = await setupSaveWindow({
      initial: [{ key: 'PATH', mode: 'follow_host', value: '', resolvedValue: '/usr/bin' }],
    });
    mocked.getGlobalEnv.mockRejectedValue(new ApiError(500, 'internal', 'cfg down'));
    await submitSave();
    await flushUI();
    // 失败提示 + 重试入口（系统区自己的 error-line）
    expect(system.querySelector('.error-line')?.textContent).toContain(
      '读取全局环境变量配置失败',
    );
    // 普通 key（HOME 不在旧集合）：不放行并显示读取状态
    const homeAdd = addBtn(rowOf(system, 'HOME'));
    expect(homeAdd.disabled).toBe(true);
    expect(homeAdd.title).toContain('读取全局配置失败');
    const putCalls = mocked.putGlobalEnv.mock.calls.length;
    await act(async () => {
      homeAdd.click();
    });
    expect(mocked.putGlobalEnv.mock.calls.length).toBe(putCalls);
    // 已知行级原因不被读取状态覆盖
    expect(addBtn(rowOf(system, 'PATH')).textContent).toBe('已配置');
    expect(addBtn(rowOf(system, 'OCDECK_TOKEN')).textContent).toBe('系统保留');
    expect(addBtn(rowOf(system, '1BAD-KEY')).textContent).toBe('非法 key');
    mounted.unmount();
  });
});

describe('K1：全局区与系统区状态同步（组合级）', () => {
  it('全局区新增 manual 后系统区同行变「已配置」禁用；删除后恢复「可添加」；manual 不被覆盖', async () => {
    let current: GlobalEnvVar[] = [];
    mocked.getGlobalEnv.mockImplementation(async () => globalVarsOf(current));
    mocked.putGlobalEnv.mockImplementation(async (key: string, mode: string, value: string) => {
      current = [{ key, mode: mode as GlobalEnvMode, value, resolvedValue: '' }];
      return { restartRequired: false };
    });
    mocked.deleteGlobalEnv.mockImplementation(async () => {
      current = [];
      return { restartRequired: false };
    });

    const { container, unmount } = mount(<EnvHarness />);
    await flushUI();
    const { global, system } = panels(container);

    // 初始：HOME 在系统区可添加
    expect(addBtn(rowOf(system, 'HOME')).textContent).toBe('添加');
    expect(addBtn(rowOf(system, 'HOME')).disabled).toBe(false);

    // 全局区把 HOME 加为 manual（值为 keep-me）
    setInput(global.querySelector<HTMLInputElement>('form.env-add input[placeholder="KEY"]')!, 'HOME');
    const manualBtn = [
      ...global.querySelectorAll<HTMLButtonElement>('form.env-add .env-mode-opt'),
    ].find((b) => b.textContent === '手动配置')!;
    act(() => manualBtn.click());
    setInput(
      global.querySelector<HTMLInputElement>('form.env-add input[placeholder="value"]')!,
      'keep-me',
    );
    await act(async () => {
      global
        .querySelector('form.env-add')!
        .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    });
    await flushUI();
    expect(mocked.putGlobalEnv).toHaveBeenCalledWith('HOME', 'manual', 'keep-me');

    // 系统区同行变为「已配置」禁用——不会被 upsert 覆盖成 follow_host
    const homeAdd = addBtn(rowOf(system, 'HOME'));
    expect(homeAdd.textContent).toBe('已配置');
    expect(homeAdd.disabled).toBe(true);
    mocked.putGlobalEnv.mockClear();
    await act(async () => {
      homeAdd.click();
    });
    expect(mocked.putGlobalEnv).not.toHaveBeenCalled();

    // 全局区删除 HOME → 系统区恢复「可添加」
    const delBtn = [...global.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '删除',
    )!;
    act(() => delBtn.click());
    const confirmBtn = [...global.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === '确认删除',
    )!;
    await act(async () => {
      confirmBtn.click();
    });
    await flushUI();
    expect(mocked.deleteGlobalEnv).toHaveBeenCalledWith('HOME');
    expect(addBtn(rowOf(system, 'HOME')).textContent).toBe('添加');
    expect(addBtn(rowOf(system, 'HOME')).disabled).toBe(false);
    unmount();
  });
});

describe('K2：配置集未就绪不放行', () => {
  it('getGlobalEnv 延迟期间：宿主列表正常展示，添加按钮全部禁用且不发起 PUT', async () => {
    let resolveCfg!: (v: { vars: GlobalEnvVar[]; restartRequired: boolean }) => void;
    mocked.getGlobalEnv.mockReturnValue(new Promise((r) => (resolveCfg = r)));
    const { container, unmount } = renderPanel();
    await flushUI();
    // 宿主列表照常展示
    expect(container.querySelectorAll('.env-row')).toHaveLength(HOST_VARS.length);
    const homeAdd = addBtn(rowOf(container, 'HOME'));
    expect(homeAdd.disabled).toBe(true);
    expect(homeAdd.title).toContain('正在读取全局配置');
    await act(async () => {
      homeAdd.click();
    });
    expect(mocked.putGlobalEnv).not.toHaveBeenCalled();
    // 配置集到达后放行
    await act(async () => resolveCfg(globalVars()));
    await flushUI();
    expect(addBtn(rowOf(container, 'HOME')).disabled).toBe(false);
    unmount();
  });

  it('getGlobalEnv 失败：错误提示 + 重试入口；重试成功后放行', async () => {
    mocked.getGlobalEnv.mockRejectedValueOnce(new ApiError(500, 'internal', 'cfg down'));
    const { container, unmount } = renderPanel();
    await flushUI();
    expect(container.querySelector('.error-line')?.textContent).toContain(
      '读取全局环境变量配置失败',
    );
    expect(addBtn(rowOf(container, 'HOME')).disabled).toBe(true);
    expect(addBtn(rowOf(container, 'HOME')).title).toContain('读取全局配置失败');
    const retry = [...container.querySelectorAll<HTMLButtonElement>('.error-line button')].find(
      (b) => b.textContent === '重试',
    )!;
    await act(async () => {
      retry.click();
    });
    await flushUI();
    expect(addBtn(rowOf(container, 'HOME')).disabled).toBe(false);
    unmount();
  });

  it('添加成功后到 reload 完成前保持「已配置」禁用（不等 reload）', async () => {
    let resolveReload!: (v: { vars: GlobalEnvVar[]; restartRequired: boolean }) => void;
    // 首次加载正常；添加触发的共享信号 reload 挂起
    mocked.getGlobalEnv
      .mockResolvedValueOnce(globalVarsOf([]))
      .mockReturnValueOnce(new Promise((r) => (resolveReload = r)));
    const { container, unmount } = mount(<PanelHarness />);
    await flushUI();
    await act(async () => {
      addBtn(rowOf(container, 'HOME')).click();
    });
    expect(mocked.putGlobalEnv).toHaveBeenCalledWith('HOME', 'follow_host', '');
    // reload 尚未返回：按钮已本地标记「已配置」禁用
    const btn = addBtn(rowOf(container, 'HOME'));
    expect(btn.textContent).toBe('已配置');
    expect(btn.disabled).toBe(true);
    await act(async () =>
      resolveReload(
        globalVarsOf([{ key: 'HOME', mode: 'follow_host', value: '', resolvedValue: '' }]),
      ),
    );
    await flushUI();
    expect(addBtn(rowOf(container, 'HOME')).textContent).toBe('已配置');
    unmount();
  });

  it('迟到的旧 getGlobalEnv 响应不覆盖新状态', async () => {
    let resolveOld!: (v: { vars: GlobalEnvVar[]; restartRequired: boolean }) => void;
    mocked.getGlobalEnv.mockReturnValueOnce(new Promise((r) => (resolveOld = r)));
    const { container, root, unmount } = renderPanel();
    await flushUI();
    // 共享信号触发第二次加载，快速返回空集（HOME 未配置）
    mocked.getGlobalEnv.mockResolvedValueOnce(globalVarsOf([]));
    rerender(root, <SystemEnvPanel reloadTick={1} onGlobalChanged={() => {}} />);
    await flushUI();
    expect(addBtn(rowOf(container, 'HOME')).disabled).toBe(false);
    // 旧响应迟到并声称 HOME 已配置：被丢弃，仍保持「可添加」
    await act(async () =>
      resolveOld(globalVarsOf([{ key: 'HOME', mode: 'manual', value: 'x', resolvedValue: '' }])),
    );
    await flushUI();
    const btn = addBtn(rowOf(container, 'HOME'));
    expect(btn.textContent).toBe('添加');
    expect(btn.disabled).toBe(false);
    unmount();
  });
});

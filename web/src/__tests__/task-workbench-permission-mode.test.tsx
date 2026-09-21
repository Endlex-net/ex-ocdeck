// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { TaskWorkbenchPage } from '../pages/TaskWorkbenchPage';
import { api } from '../api';
import { subscribeTask } from '../sse';
import { mount, flushUI } from './cm-test-env';
import type { Task, TaskDetail, TaskPermissionMode } from '../types';

/* ==================== TaskWorkbenchPage 权限模式接线（task-permission-mode P3 gate F1 +
 * fix-ai-auto-permission-and-tab-focus 5.3） ====================
 * 走真实页面渲染路径（详情流 onData → 页头徽标组 / 设置 tab 的 TaskInfoCard）：
 * - 徽标接线：非缺省 permission_mode 必须在页头徽标组显示对应定稿文案；
 * - effective 合并保留：通用 PATCH 响应（无 effective_permission_mode）回写不得丢失
 *   「下次激活生效」提示（D8 mergeTaskInfoPatch）；SSE 不推帧时提示仍存在；
 * - 模式端点响应仅合并两模式字段（D8 mergePermissionModeView）。 */

type TaskSubOpts = {
  onData: (t: TaskDetail) => void;
  onError: (m: string) => void;
  onGone: () => void;
};
let taskSub: TaskSubOpts | null = null;

vi.mock('../sse', () => ({
  subscribeTask: vi.fn((_id: string, opts: TaskSubOpts) => {
    taskSub = opts;
    return { close: vi.fn() };
  }),
}));

vi.mock('../api', () => ({
  api: {
    listTerminals: vi.fn(async () => []),
    updateTask: vi.fn(),
    getTask: vi.fn(),
    updateTaskPermissionMode: vi.fn(),
  },
  ApiError: class ApiError extends Error {
    constructor(
      public code: string,
      message: string,
    ) {
      super(message);
    }
  },
}));

const updateTaskMock = vi.mocked(api.updateTask);
const updatePermissionModeMock = vi.mocked(api.updateTaskPermissionMode);

vi.mock('../hooks', () => ({
  useMediaQuery: () => false,
  useProjects: () => ({ projects: [] }),
  useProjectsRefresh: () => vi.fn(async () => {}),
}));

/* 终端/面板重依赖打桩：本测试只关心页头徽标组与设置 tab，不加载 xterm/git 面板。 */
vi.mock('../terminal/TerminalView', () => ({ TerminalView: () => null }));
vi.mock('../components/GitPanel', () => ({ GitPanel: () => null }));
vi.mock('../components/EnvEditor', () => ({ EnvEditor: () => null }));

function makeTask(permission_mode: TaskPermissionMode): TaskDetail {
  return {
    id: 't1',
    project_id: 'p1',
    project_kind: 'repo',
    name: 'demo-task',
    branch: 'main',
    base_ref: '',
    status: 'active',
    worktree_path: '/tmp/wt',
    mode: 'worktree',
    permission_mode,
    effective_permission_mode: permission_mode,
    init_status: 'none',
    created_at: 1,
    updated_at: 2,
  };
}

/** 通用 PATCH 响应 DTO（D8）：通用 Task 不含 effective_permission_mode。 */
function toTaskDto(t: TaskDetail): Task {
  const { effective_permission_mode: _eff, ...dto } = t;
  return dto;
}

/** 页头徽标组内按文案定位徽标（与状态/init/agent 徽标同组，按定稿文案区分）。
 *  未命中归一为 null（find 的 undefined 会被 not.toBeNull 放过）。 */
function headerBadge(container: HTMLElement, label: string) {
  return (
    [...container.querySelectorAll('.page-header .badge')].find((b) => b.textContent === label) ??
    null
  );
}

function btnByText(container: HTMLElement, text: string): HTMLButtonElement {
  const b = [...container.querySelectorAll('button')].find((x) => x.textContent?.trim() === text);
  if (!b) throw new Error(`button not found: ${text}`);
  return b;
}

function setInput(el: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
  setter.call(el, value);
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

async function click(el: HTMLElement) {
  await act(async () => {
    el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
  });
  await flushUI();
}

function modeSelect(container: HTMLElement): HTMLSelectElement {
  return container.querySelector<HTMLSelectElement>('#task-info-permission-mode')!;
}

async function openSettings(container: HTMLElement) {
  await click(
    [...container.querySelectorAll<HTMLButtonElement>('.tabstrip button')].find(
      (b) => b.textContent === '设置',
    )!,
  );
}

beforeEach(() => {
  taskSub = null;
  vi.mocked(subscribeTask).mockClear();
  updateTaskMock.mockReset();
  updatePermissionModeMock.mockReset();
});

describe('TaskWorkbenchPage 权限模式徽标接线（P3 gate F1）', () => {
  it.each([
    ['all-approve', '全部批准'],
    ['ai-auto', 'AI 自动识别'],
    ['ask', '人工批准'],
  ] as const)('详情流 task.permission_mode=%s → 页头徽标组显示「%s」', (mode, label) => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    expect(taskSub).not.toBeNull();
    act(() => taskSub!.onData(makeTask(mode)));
    const badge = headerBadge(container, label);
    expect(badge).not.toBeNull();
    expect(badge!.className).toContain('badge-muted');
    expect(badge!.getAttribute('title')).toBe(`权限模式：${mode}`);
    unmount();
  });

  it('流推送不变更权限模式：连续两帧徽标文案稳定', () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask('all-approve')));
    expect(headerBadge(container, '全部批准')).not.toBeNull();
    act(() => taskSub!.onData(makeTask('all-approve')));
    expect(headerBadge(container, '全部批准')).not.toBeNull();
    unmount();
  });
});

describe('effective 合并保留与提示收敛（fix-ai-auto-permission-and-tab-focus 5.3/D8）', () => {
  it('模式不一致后保存任务名称（通用 PATCH 回写无 effective）：「下次激活生效」提示仍存在', async () => {
    // 运行中进程按 ask 处理，已保存值已改为 all-approve → 常驻提示
    const diverged = { ...makeTask('all-approve'), effective_permission_mode: 'ask' as const };
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(diverged));
    await openSettings(container);
    expect(container.textContent).toContain('当前运行进程仍按「人工批准」处理，新模式将在下次激活生效');

    // 保存任务名称：通用 PATCH 响应为通用 Task（不含 effective_permission_mode），
    // 且期间 SSE 不推任何帧——提示 MUST 仍在（mergeTaskInfoPatch 合并保留）
    updateTaskMock.mockResolvedValueOnce({ ...toTaskDto(diverged), name: '新任务名' });
    await click(btnByText(container, '编辑'));
    setInput(container.querySelector<HTMLInputElement>('#task-info-name')!, '新任务名');
    await click(btnByText(container, '保存'));
    expect(updateTaskMock).toHaveBeenCalledTimes(1);
    expect(updateTaskMock.mock.calls[0][1]).toEqual({ name: '新任务名', branch_slug: 'main' });
    expect(container.textContent).toContain('新任务名');
    // SSE 未推帧：名称已更新而提示仍存在 → effective 由通用 PATCH 回写合并保留
    expect(container.textContent).toContain('当前运行进程仍按「人工批准」处理，新模式将在下次激活生效');
    unmount();
  });

  it('详情 SSE 帧收敛两模式字段：不一致→提示出现；一致（如进程退出）→提示消失', async () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData({ ...makeTask('all-approve'), effective_permission_mode: 'ask' }));
    await openSettings(container);
    // 详情读模型输出（D6）：已保存 all-approve、运行中按 ask → 提示出现
    expect(container.textContent).toContain('当前运行进程仍按「人工批准」处理，新模式将在下次激活生效');
    // 下一帧两值一致（如运行进程已退出、按持久化值推导）→ 提示消失，选择器跟随
    act(() => taskSub!.onData(makeTask('all-approve')));
    expect(container.textContent).not.toContain('新模式将在下次激活生效');
    unmount();
  });

  it('模式端点保存成功：仅合并两模式字段——页头徽标与提示更新、其余字段不回退', async () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask('ask')));
    await openSettings(container);
    // 查看态只读无选择器；进编辑态选值后随「保存」走专用模式端点
    expect(container.querySelector('#task-info-permission-mode')).toBeNull();
    updateTaskMock.mockResolvedValueOnce(toTaskDto(makeTask('ask')));
    updatePermissionModeMock.mockResolvedValueOnce({
      permission_mode: 'all-approve',
      effective_permission_mode: 'ask',
    });
    await click(btnByText(container, '编辑'));
    const setter = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'value')!.set!;
    await act(async () => {
      setter.call(modeSelect(container), 'all-approve');
      modeSelect(container).dispatchEvent(new Event('change', { bubbles: true }));
    });
    await flushUI();
    await click(btnByText(container, '保存'));
    expect(updatePermissionModeMock).toHaveBeenCalledTimes(1);
    expect(updatePermissionModeMock).toHaveBeenCalledWith('t1', 'all-approve');
    // 通用 PATCH 仅携带名称/分支（幂等同值写），MUST NOT 携带权限模式字段
    expect(updateTaskMock).toHaveBeenCalledTimes(1);
    expect(updateTaskMock.mock.calls[0][1]).toEqual({ name: 'demo-task', branch_slug: 'main' });
    // 两模式字段合并：页头徽标切换为已保存值、查看态文案与提示按响应两字段展示
    expect(headerBadge(container, '全部批准')).not.toBeNull();
    expect(container.querySelector('#task-info-permission-mode')).toBeNull();
    expect(container.textContent).toContain('全部批准');
    expect(container.textContent).toContain('当前运行进程仍按「人工批准」处理，新模式将在下次激活生效');
    // 其余字段保持 SSE 帧原值（未被端点响应替换）
    expect(container.textContent).toContain('demo-task');
    unmount();
  });
});

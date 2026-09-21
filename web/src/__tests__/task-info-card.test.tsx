// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { useEffect, useState } from 'react';
import type { Root } from 'react-dom/client';
import { TaskInfoCard, splitBranch } from '../components/TaskInfoCard';
import { api, ApiError } from '../api';
import { mount, flushUI } from './cm-test-env';
import {
  mergePermissionModeView,
  mergeTaskInfoPatch,
  type TaskDetail,
  type TaskPermissionModeView,
} from '../types';

/* ============================ TaskInfoCard（task-info-editable tasks.md 4.2 +
 * fix-ai-auto-permission-and-tab-focus 5.4） ============================
 * 覆盖矩阵：展示 / 编辑改名（无确认）/ slug 变更危险确认 / 失败保持编辑态 / gitless /
 * 取消放弃 / 保存结果不确定（MUST 刷新后以当前分支重新确认，MUST NOT 直接重发旧请求）/
 * 权限模式查看态只读、编辑态选择器随「保存」走专用模式端点（未变不调/变更不经通用 PATCH/
 * 禁用/重叠与旧响应不覆盖/失败 GET 复验）与不一致提示。
 * api 层 mock；onSaved/onPermissionModeSaved/onRefreshed 由 Harness 模拟父级合并收敛
 * 本地任务态（同 TaskWorkbenchPage 接线：通用 PATCH 合并保留 effective、模式端点仅合并两模式字段）。 */

vi.mock('../api', () => ({
  api: {
    updateTask: vi.fn(),
    getTask: vi.fn(),
    updateTaskPermissionMode: vi.fn(),
  },
  // 与 api.ts 真实签名同形（status, code, message）
  ApiError: class ApiError extends Error {
    constructor(
      public status: number,
      public code: string,
      message: string,
    ) {
      super(message);
    }
  },
}));

const updateTaskMock = vi.mocked(api.updateTask);
const getTaskMock = vi.mocked(api.getTask);
const updatePermissionModeMock = vi.mocked(api.updateTaskPermissionMode);

function makeTask(over: Partial<TaskDetail> = {}): TaskDetail {
  return {
    id: 't1',
    project_id: 'p1',
    project_kind: 'repo',
    name: '旧任务名',
    branch: 'ocdeck/old-slug',
    base_ref: 'refs/heads/main',
    status: 'suspended',
    worktree_path: '/data/worktrees/proj/old-slug-a1b2',
    mode: 'worktree',
    permission_mode: 'ask',
    effective_permission_mode: 'ask',
    init_status: 'succeeded',
    created_at: 1700000000,
    updated_at: 1700000000,
    ...over,
  };
}

/** 模拟 TaskWorkbenchPage 接线：通用 PATCH 响应合并保留 effective（D8）；
 *  模式端点响应仅合并两模式字段；刷新结果整体替换（详情读模型）。
 *  pushRef：暴露 setTask，模拟编辑期间 SSE 帧推送更新任务态（I5 回归）。 */
function Harness({
  initial,
  pushRef,
}: {
  initial: TaskDetail;
  pushRef?: { current: ((t: TaskDetail) => void) | null };
}) {
  const [task, setTask] = useState(initial);
  useEffect(() => {
    if (pushRef) pushRef.current = setTask;
  });
  return (
    <TaskInfoCard
      task={task}
      projectName="ocdeck-proj"
      onSaved={(t) => setTask((prev) => mergeTaskInfoPatch(prev, t))}
      onPermissionModeSaved={(view) => setTask((prev) => mergePermissionModeView(prev, view))}
      onRefreshed={(t) => setTask(t)}
    />
  );
}

function setInput(el: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
  setter.call(el, value);
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

function btnByText(container: HTMLElement, text: string): HTMLButtonElement {
  const b = [...container.querySelectorAll('button')].find((x) => x.textContent?.trim() === text);
  if (!b) throw new Error(`button not found: ${text}`);
  return b;
}

/** 保存按钮（busy 时文案为「保存中…」，按位置/样式定位）。 */
function saveBtn(container: HTMLElement): HTMLButtonElement {
  return container.querySelector<HTMLButtonElement>('.task-info-actions .btn-primary')!;
}

async function click(el: HTMLElement) {
  await act(async () => {
    el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
  });
  await flushUI();
}

async function enterEdit(container: HTMLElement) {
  await click(btnByText(container, '编辑'));
}

function nameInput(container: HTMLElement) {
  return container.querySelector<HTMLInputElement>('#task-info-name');
}

function slugInput(container: HTMLElement) {
  return container.querySelector<HTMLInputElement>('#task-info-branch-slug');
}

function modeSelect(container: HTMLElement): HTMLSelectElement {
  return container.querySelector<HTMLSelectElement>('#task-info-permission-mode')!;
}

async function selectMode(el: HTMLSelectElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'value')!.set!;
  await act(async () => {
    setter.call(el, value);
    el.dispatchEvent(new Event('change', { bubbles: true }));
  });
  await flushUI();
}

const roots: Root[] = [];

beforeEach(() => {
  updateTaskMock.mockReset();
  getTaskMock.mockReset();
  updatePermissionModeMock.mockReset();
});

afterEach(async () => {
  while (roots.length) {
    const root = roots.pop()!;
    await act(async () => {
      root.unmount();
    });
  }
});

function renderCard(
  initial: TaskDetail = makeTask(),
  pushRef?: { current: ((t: TaskDetail) => void) | null },
) {
  const utils = mount(<Harness initial={initial} pushRef={pushRef} />);
  roots.push(utils.root);
  return utils;
}

describe('splitBranch（前缀/slug 拆分）', () => {
  it('常规前缀分支 / 嵌套 slug / 无斜杠三分支', () => {
    expect(splitBranch('ocdeck/old-slug')).toEqual({ prefix: 'ocdeck', slug: 'old-slug' });
    expect(splitBranch('ocdeck/feature/X')).toEqual({ prefix: 'ocdeck/feature', slug: 'X' });
    expect(splitBranch('plain')).toEqual({ prefix: '', slug: 'plain' });
  });
});

describe('展示态', () => {
  it('字段连续排列：项目/状态/基础分支/worktree 路径/创建时间/任务名称/分支 + 单编辑按钮', () => {
    const { container } = renderCard();
    const labels = [...container.querySelectorAll('.task-info-label')].map((x) => x.textContent);
    expect(labels).toEqual([
      '项目',
      '状态',
      '基础分支',
      'worktree 路径',
      '创建时间',
      '任务名称',
      '分支',
      '权限模式',
    ]);
    expect(container.textContent).toContain('ocdeck-proj');
    expect(container.textContent).toContain('main'); // base_ref 短名
    expect(container.textContent).toContain('/data/worktrees/proj/old-slug-a1b2');
    expect(container.textContent).toContain('旧任务名');
    expect(container.textContent).toContain('ocdeck/old-slug');
    expect(container.querySelector('.task-info-head button')?.textContent).toBe('编辑');
    // 展示态无输入框
    expect(container.querySelector('input')).toBeNull();
  });

  it('gitless（dir）：不渲染分支行，基础分支行显示路径', () => {
    const { container } = renderCard(
      makeTask({ project_kind: 'dir', branch: '', base_ref: '', worktree_path: '/repo/dir-proj' }),
    );
    const labels = [...container.querySelectorAll('.task-info-label')].map((x) => x.textContent);
    expect(labels).not.toContain('分支');
    expect(labels).not.toContain('基础分支');
    expect(container.textContent).toContain('/repo/dir-proj');
  });

  it('gitless（repo local-path）：同样不渲染分支行', () => {
    const { container } = renderCard(makeTask({ mode: 'local-path', branch: '', base_ref: '' }));
    const labels = [...container.querySelectorAll('.task-info-label')].map((x) => x.textContent);
    expect(labels).not.toContain('分支');
  });
});

describe('编辑态', () => {
  it('仅改名保存：无需确认直接 PATCH，成功回到展示态展示新名', async () => {
    const updated = makeTask({ name: '新任务名' });
    updateTaskMock.mockResolvedValue(updated);
    const { container } = renderCard();
    await enterEdit(container);
    await act(async () => {
      setInput(nameInput(container)!, '新任务名');
    });
    await click(btnByText(container, '保存'));
    // slug 未变 → 无危险确认 modal
    expect(container.querySelector('.modal-backdrop')).toBeNull();
    expect(updateTaskMock).toHaveBeenCalledTimes(1);
    expect(updateTaskMock.mock.calls[0][0]).toBe('t1');
    expect(updateTaskMock.mock.calls[0][1]).toEqual({ name: '新任务名', branch_slug: 'old-slug' });
    // 回到展示态并展示新值
    expect(container.querySelector('#task-info-name')).toBeNull();
    expect(container.textContent).toContain('新任务名');
  });

  it('两项均未修改：保存为幂等同值写（仍发 PATCH、无确认）', async () => {
    updateTaskMock.mockResolvedValue(makeTask());
    const { container } = renderCard();
    await enterEdit(container);
    await click(btnByText(container, '保存'));
    expect(container.querySelector('.modal-backdrop')).toBeNull();
    expect(updateTaskMock).toHaveBeenCalledTimes(1);
    expect(updateTaskMock.mock.calls[0][1]).toEqual({ name: '旧任务名', branch_slug: 'old-slug' });
  });

  it('slug 变更：实时预览最终分支名（沿用原前缀）', async () => {
    const { container } = renderCard();
    await enterEdit(container);
    expect(container.textContent).toContain('最终分支名：ocdeck/old-slug');
    await act(async () => {
      setInput(slugInput(container)!, 'new-slug');
    });
    expect(container.textContent).toContain('最终分支名：ocdeck/new-slug');
  });

  it('slug 变更保存：先弹危险确认（旧分支名废弃/路径与提交不变），确认后才 PATCH', async () => {
    const updated = makeTask({ branch: 'ocdeck/new-slug' });
    updateTaskMock.mockResolvedValue(updated);
    const { container } = renderCard();
    await enterEdit(container);
    await act(async () => {
      setInput(slugInput(container)!, 'new-slug');
    });
    await click(btnByText(container, '保存'));
    // 确认前 MUST NOT 发起保存
    expect(updateTaskMock).not.toHaveBeenCalled();
    const modal = container.querySelector('.modal-backdrop');
    expect(modal).not.toBeNull();
    expect(modal!.textContent).toContain('ocdeck/old-slug');
    expect(modal!.textContent).toContain('ocdeck/new-slug');
    expect(modal!.textContent).toContain('旧分支名将废弃');
    expect(modal!.textContent).toContain('worktree 路径与提交内容不变');
    await click(btnByText(container, '确认修改'));
    expect(updateTaskMock).toHaveBeenCalledTimes(1);
    expect(updateTaskMock.mock.calls[0][1]).toEqual({ name: '旧任务名', branch_slug: 'new-slug' });
    expect(container.textContent).toContain('ocdeck/new-slug');
    expect(container.querySelector('.modal-backdrop')).toBeNull();
  });

  it('slug 变更但取消确认：留在编辑态，不发起保存', async () => {
    const { container } = renderCard();
    await enterEdit(container);
    await act(async () => {
      setInput(slugInput(container)!, 'new-slug');
    });
    await click(btnByText(container, '保存'));
    expect(container.querySelector('.modal-backdrop')).not.toBeNull();
    await click(container.querySelector('.modal-actions .btn:not(.btn-danger)') as HTMLElement);
    expect(updateTaskMock).not.toHaveBeenCalled();
    expect(container.querySelector('.modal-backdrop')).toBeNull();
    // 仍处编辑态且草稿保留
    expect(slugInput(container)?.value).toBe('new-slug');
  });

  it('取消：放弃全部修改回到展示态，不发起任何保存', async () => {
    const { container } = renderCard();
    await enterEdit(container);
    await act(async () => {
      setInput(nameInput(container)!, '改名未遂');
      setInput(slugInput(container)!, 'aborted');
    });
    await click(btnByText(container, '取消'));
    expect(updateTaskMock).not.toHaveBeenCalled();
    expect(container.querySelector('#task-info-name')).toBeNull();
    expect(container.textContent).toContain('旧任务名');
    expect(container.textContent).toContain('ocdeck/old-slug');
  });

  it('名称 trim 后为空：照常发 PATCH，invalid_input 也进入恢复门禁（P4-3 + P4-4）', async () => {
    // 恢复 {} 挂起：断言恢复成功前保存禁用（不自动重发、不可携带旧草稿提交）
    let resolveRecover!: (t: TaskDetail) => void;
    updateTaskMock.mockRejectedValueOnce(new ApiError(422, 'invalid_input', '任务名称不能为空'));
    updateTaskMock.mockImplementationOnce(
      () =>
        new Promise<TaskDetail>((res) => {
          resolveRecover = res;
        }),
    );
    getTaskMock.mockResolvedValue(makeTask());

    const { container } = renderCard();
    await enterEdit(container);
    await act(async () => {
      setInput(nameInput(container)!, '   ');
    });
    await click(btnByText(container, '保存'));
    // 不做本地拦截：草稿 PATCH 照常发出；服务端拒绝后保留原始错误展示
    expect(updateTaskMock).toHaveBeenCalledTimes(2);
    expect(updateTaskMock.mock.calls[0][1]).toEqual({ name: '   ', branch_slug: 'old-slug' });
    expect(updateTaskMock.mock.calls[1]).toEqual(['t1', {}]); // 自动恢复：空 PATCH 收敛
    expect(container.textContent).toContain('[invalid_input] 任务名称不能为空');
    // 草稿保留；恢复未完成（{} 在途）前保存禁用、GET 未发起、无第二次草稿 PATCH
    expect(nameInput(container)?.value).toBe('   ');
    expect(saveBtn(container).disabled).toBe(true);
    expect(getTaskMock).not.toHaveBeenCalled();

    // 恢复成功（{} + GET）后解禁，可重新保存
    await act(async () => {
      resolveRecover(makeTask());
    });
    await flushUI();
    expect(getTaskMock).toHaveBeenCalledTimes(1);
    expect(saveBtn(container).disabled).toBe(false);
    expect(container.textContent).toContain('[invalid_input] 任务名称不能为空'); // 原始错误仍展示
  });

  it('服务端拒绝（409 conflict）：保留错误 + 草稿，同样进入恢复门禁（P4-4）', async () => {
    updateTaskMock.mockRejectedValueOnce(new ApiError(409, 'conflict', '目标分支已存在'));
    updateTaskMock.mockResolvedValue(makeTask());
    getTaskMock.mockResolvedValue(makeTask());

    const { container } = renderCard();
    await enterEdit(container);
    await act(async () => {
      setInput(nameInput(container)!, '新任务名');
    });
    await click(btnByText(container, '保存'));
    expect(container.textContent).toContain('[conflict] 目标分支已存在');
    // 保持编辑态与用户输入；恢复 = 空 PATCH {} + GET，MUST NOT 自动重发草稿
    expect(updateTaskMock).toHaveBeenCalledTimes(2);
    expect(updateTaskMock.mock.calls[1]).toEqual(['t1', {}]);
    expect(getTaskMock).toHaveBeenCalledTimes(1);
    expect(nameInput(container)?.value).toBe('新任务名');
    expect(slugInput(container)?.value).toBe('old-slug');
    expect(container.textContent).toContain('已恢复任务状态');
  });

  it('gitless 编辑态：仅名称可编辑，无 slug 输入，PATCH 不携带 branch_slug', async () => {
    const dirTask = makeTask({ project_kind: 'dir', branch: '', base_ref: '' });
    updateTaskMock.mockResolvedValue({ ...dirTask, name: 'dir 新名' });
    const { container } = renderCard(dirTask);
    await enterEdit(container);
    expect(nameInput(container)).not.toBeNull();
    expect(slugInput(container)).toBeNull();
    await act(async () => {
      setInput(nameInput(container)!, 'dir 新名');
    });
    await click(btnByText(container, '保存'));
    expect(updateTaskMock.mock.calls[0][1]).toEqual({ name: 'dir 新名' });
    expect('branch_slug' in updateTaskMock.mock.calls[0][1]).toBe(false);
  });
});

describe('保存结果不确定（超时/网络错误/internal/git_error）', () => {
  it('MUST 先恢复（空 PATCH 收敛 + GET）并以当前分支重新确认 slug，MUST NOT 直接重发旧请求', async () => {
    // 模拟服务端已提交但响应丢失：分支已是 ocdeck/feature/X
    const committed = makeTask({ name: '新任务名', branch: 'ocdeck/feature/X' });
    updateTaskMock.mockRejectedValueOnce(new ApiError(0, 'network_error', '无法连接服务端'));
    updateTaskMock.mockResolvedValue(committed);
    getTaskMock.mockResolvedValue(committed);

    const { container } = renderCard();
    await enterEdit(container);
    await act(async () => {
      setInput(nameInput(container)!, '新任务名');
      setInput(slugInput(container)!, 'feature/X');
    });
    await click(btnByText(container, '保存'));
    await click(btnByText(container, '确认修改'));
    // 第一次请求发出后结果不确定：恢复 = 空 PATCH {}（仅 R1 收敛）→ GET 刷新
    expect(updateTaskMock).toHaveBeenCalledTimes(2);
    expect(updateTaskMock.mock.calls[1]).toEqual(['t1', {}]);
    expect(getTaskMock).toHaveBeenCalledTimes(1);
    expect(getTaskMock).toHaveBeenCalledWith('t1');
    // 顺序契约：空 PATCH 收敛先于 GET
    expect(updateTaskMock.mock.invocationCallOrder[1]).toBeLessThan(
      getTaskMock.mock.invocationCallOrder[0],
    );
    expect(container.textContent).toContain('保存结果不确定，已恢复任务状态');
    // 保持编辑态，MUST NOT 自动重发旧请求（除恢复用空 PATCH 外无草稿 PATCH）
    expect(nameInput(container)).not.toBeNull();

    // 再次保存：slug 相对「当前分支 ocdeck/feature/X」重新判定——draft feature/X 换算出
    // ocdeck/feature/feature/X（嵌套 slug 重放非幂等），仍需重新弹确认且展示刷新后的当前分支
    await click(btnByText(container, '保存'));
    const modal = container.querySelector('.modal-backdrop');
    expect(modal).not.toBeNull();
    expect(modal!.textContent).toContain('ocdeck/feature/X');
    expect(modal!.textContent).toContain('ocdeck/feature/feature/X');
    await click(btnByText(container, '确认修改'));
    expect(updateTaskMock).toHaveBeenCalledTimes(3);
    // 草稿 PATCH 重发必须晚于恢复（getTask），且以刷新后的当前分支为确认基准
    expect(getTaskMock.mock.invocationCallOrder[0]).toBeLessThan(
      updateTaskMock.mock.invocationCallOrder[2],
    );
  });

  it('提交成功后返回 internal（提交后重读失败）：按结果不确定走恢复门禁（嵌套 slug 防护）', async () => {
    // 服务端已提交（分支已改名）但返回 internal——不得视为"确定未生效"直接重试
    const committed = makeTask({ branch: 'ocdeck/feature/X' });
    updateTaskMock.mockRejectedValueOnce(new ApiError(500, 'internal', '提交后重读任务失败'));
    updateTaskMock.mockResolvedValue(committed);
    getTaskMock.mockResolvedValue(committed);

    const { container } = renderCard();
    await enterEdit(container);
    await act(async () => {
      setInput(slugInput(container)!, 'feature/X');
    });
    await click(btnByText(container, '保存'));
    await click(btnByText(container, '确认修改'));
    // 走恢复门禁：空 PATCH {} 收敛 + GET，而非直接展示 [internal] 允许按旧基准确认重试
    expect(updateTaskMock).toHaveBeenCalledTimes(2);
    expect(updateTaskMock.mock.calls[1]).toEqual(['t1', {}]);
    expect(getTaskMock).toHaveBeenCalledTimes(1);
    expect(container.textContent).toContain('保存结果不确定，已恢复任务状态');
    expect(container.textContent).toContain('[internal] 提交后重读任务失败'); // 原始错误保留展示
    // 再次保存以恢复后的当前分支重新确认（嵌套 slug 防护：不直接产生 feature/feature/X 重放）
    await click(btnByText(container, '保存'));
    const modal = container.querySelector('.modal-backdrop');
    expect(modal!.textContent).toContain('ocdeck/feature/X');
    expect(modal!.textContent).toContain('ocdeck/feature/feature/X');
  });

  it('历史意图存在：conflict 后经恢复取得真实新基准，确认展示历史恢复后的新分支（P4-4）', async () => {
    // 场景：存在未收敛历史意图——首次保存返回 conflict（R1 收敛阶段被拒/或历史恢复已生效）。
    // 恢复（空 PATCH {} 成功，R1 收敛补做历史提交）后 GET 返回已被历史恢复改成的嵌套新分支。
    const historical = makeTask({ branch: 'ocdeck/feature/Y' });
    updateTaskMock.mockRejectedValueOnce(new ApiError(409, 'conflict', '存在未收敛的恢复意图'));
    updateTaskMock.mockResolvedValue(historical);
    getTaskMock.mockResolvedValue(historical);

    const { container } = renderCard();
    await enterEdit(container);
    await act(async () => {
      setInput(slugInput(container)!, 'feature/X');
    });
    await click(btnByText(container, '保存'));
    await click(btnByText(container, '确认修改'));
    expect(container.textContent).toContain('[conflict] 存在未收敛的恢复意图');
    // 取得新基准前：仅有草稿 PATCH + 恢复 {}，没有第二次草稿提交
    expect(updateTaskMock).toHaveBeenCalledTimes(2);
    expect(updateTaskMock.mock.calls[0][1]).toEqual({ name: '旧任务名', branch_slug: 'feature/X' });
    expect(updateTaskMock.mock.calls[1]).toEqual(['t1', {}]);
    expect(getTaskMock).toHaveBeenCalledTimes(1);
    expect(container.textContent).toContain('已恢复任务状态');

    // 恢复后再次保存：危险确认以历史恢复后的真实分支 ocdeck/feature/Y 为旧名，
    // 新目标按其前缀换算为 ocdeck/feature/feature/X（绝不按失效 baseline ocdeck/old-slug 呈现）
    await click(btnByText(container, '保存'));
    const modal = container.querySelector('.modal-backdrop');
    expect(modal).not.toBeNull();
    expect(modal!.textContent).toContain('ocdeck/feature/Y');
    expect(modal!.textContent).toContain('ocdeck/feature/feature/X');
    expect(modal!.textContent).not.toContain('ocdeck/old-slug');
  });

  it('git_error（git 结果未知、意图保留）：恢复门禁 + 空 PATCH 先收敛再 GET', async () => {
    // 服务端改名可能已生效、意图待收敛：恢复的空 PATCH {} 触发 R1 收敛后再读
    const committed = makeTask({ branch: 'ocdeck/feature/X' });
    updateTaskMock.mockRejectedValueOnce(new ApiError(500, 'git_error', '分支改名结果未知'));
    updateTaskMock.mockResolvedValue(committed);
    getTaskMock.mockResolvedValue(committed);

    const { container } = renderCard();
    await enterEdit(container);
    await act(async () => {
      setInput(slugInput(container)!, 'feature/X');
    });
    await click(btnByText(container, '保存'));
    await click(btnByText(container, '确认修改'));
    expect(updateTaskMock).toHaveBeenCalledTimes(2);
    expect(updateTaskMock.mock.calls[1]).toEqual(['t1', {}]);
    expect(getTaskMock).toHaveBeenCalledTimes(1);
    expect(updateTaskMock.mock.invocationCallOrder[1]).toBeLessThan(
      getTaskMock.mock.invocationCallOrder[0],
    );
    expect(container.textContent).toContain('保存结果不确定，已恢复任务状态');
    // 草稿 PATCH 未在恢复前重发
    expect(updateTaskMock.mock.calls.filter((c) => Object.keys(c[1]).length > 0)).toHaveLength(1);
  });

  it('恢复后草稿换算与当前分支同值：再次保存无需确认（同值跳过）', async () => {
    // 服务端已提交：分支已是 ocdeck/new-slug
    const committed = makeTask({ branch: 'ocdeck/new-slug' });
    updateTaskMock.mockRejectedValueOnce(new ApiError(0, 'network_error', '无法连接服务端'));
    updateTaskMock.mockResolvedValue(committed);
    getTaskMock.mockResolvedValue(committed);

    const { container } = renderCard();
    await enterEdit(container);
    await act(async () => {
      setInput(slugInput(container)!, 'new-slug');
    });
    await click(btnByText(container, '保存'));
    await click(btnByText(container, '确认修改'));
    expect(getTaskMock).toHaveBeenCalledTimes(1);
    // 恢复后当前分支 slug = new-slug，与草稿同值 → 直接保存，不再弹确认
    await click(btnByText(container, '保存'));
    expect(container.querySelector('.modal-backdrop')).toBeNull();
    expect(updateTaskMock).toHaveBeenCalledTimes(3);
  });

  it('恢复失败：保存禁用并提供重试恢复，重试成功后解禁', async () => {
    updateTaskMock.mockRejectedValueOnce(new ApiError(0, 'network_error', '无法连接服务端'));
    updateTaskMock.mockRejectedValueOnce(new ApiError(0, 'network_error', '无法连接服务端')); // 恢复的 {} 也失败

    const { container } = renderCard();
    await enterEdit(container);
    await click(btnByText(container, '保存'));
    // 原请求 + 恢复 {} 各一次；{} 失败即中止，GET 未发起
    expect(updateTaskMock).toHaveBeenCalledTimes(2);
    expect(getTaskMock).not.toHaveBeenCalled();
    expect(container.textContent).toContain('恢复失败');
    expect(btnByText(container, '保存').disabled).toBe(true);

    // 重试恢复成功 → 解禁
    updateTaskMock.mockResolvedValue(makeTask());
    getTaskMock.mockResolvedValue(makeTask());
    await click(btnByText(container, '重试恢复'));
    expect(updateTaskMock).toHaveBeenCalledTimes(3);
    expect(updateTaskMock.mock.calls[2]).toEqual(['t1', {}]);
    expect(getTaskMock).toHaveBeenCalledTimes(1);
    expect(btnByText(container, '保存').disabled).toBe(false);
  });

  it('门禁绕过防护（P4-2）：网络失败→恢复失败→取消→重新编辑，恢复成功前发不出草稿 PATCH', async () => {
    updateTaskMock.mockRejectedValueOnce(new ApiError(0, 'network_error', '无法连接服务端'));
    updateTaskMock.mockRejectedValueOnce(new ApiError(0, 'network_error', '无法连接服务端')); // 恢复 {} 失败

    const { container } = renderCard();
    await enterEdit(container);
    await act(async () => {
      setInput(nameInput(container)!, '新任务名');
    });
    await click(btnByText(container, '保存'));
    expect(updateTaskMock).toHaveBeenCalledTimes(2);
    expect(container.textContent).toContain('恢复失败');

    // 取消 → 重新编辑：门禁 MUST NOT 被解除（取消只放弃草稿）
    await click(btnByText(container, '取消'));
    await click(btnByText(container, '编辑'));
    const saveBtn = btnByText(container, '保存');
    expect(saveBtn.disabled).toBe(true);
    await click(saveBtn); // 禁用态点击无效
    // 恢复成功前无任何草稿 PATCH 发出（仍只有原请求 + 恢复 {}）
    expect(updateTaskMock).toHaveBeenCalledTimes(2);
    expect(getTaskMock).not.toHaveBeenCalled();

    // 恢复成功后解禁，可正常保存
    updateTaskMock.mockResolvedValue(makeTask({ name: '旧任务名' }));
    getTaskMock.mockResolvedValue(makeTask());
    await click(btnByText(container, '重试恢复'));
    expect(btnByText(container, '保存').disabled).toBe(false);
  });
});

describe('权限模式（查看态/编辑态与名称/分支一致）', () => {
  it('查看态：无选择器，只读展示当前已保存模式文案', () => {
    const { container } = renderCard(makeTask({ permission_mode: 'all-approve' }));
    expect(container.querySelector('#task-info-permission-mode')).toBeNull();
    expect(container.textContent).toContain('全部批准');
  });

  it('进编辑态出现三档选择器，初始值为当前已保存值；取消还原未保存选择', async () => {
    const { container } = renderCard();
    await enterEdit(container);
    const sel = modeSelect(container);
    expect(sel).not.toBeNull();
    expect(sel.value).toBe('ask');
    expect(Array.from(sel.options).map((o) => o.value)).toEqual(['ask', 'all-approve', 'ai-auto']);
    // 未保存的选择随取消放弃：回到查看态仍展示原值，再进编辑态选择器重置为已保存值
    await selectMode(sel, 'ai-auto');
    await click(btnByText(container, '取消'));
    expect(container.querySelector('#task-info-permission-mode')).toBeNull();
    expect(container.textContent).toContain('人工批准');
    expect(updatePermissionModeMock).not.toHaveBeenCalled();
    expect(updateTaskMock).not.toHaveBeenCalled();
    await enterEdit(container);
    expect(modeSelect(container).value).toBe('ask');
  });

  it('保存时模式值未变：不调模式端点，名称/分支通用 PATCH 照常', async () => {
    updateTaskMock.mockResolvedValue(makeTask({ name: '新任务名' }));
    const { container } = renderCard();
    await enterEdit(container);
    await act(async () => {
      setInput(nameInput(container)!, '新任务名');
    });
    await click(btnByText(container, '保存'));
    expect(updateTaskMock).toHaveBeenCalledTimes(1);
    expect(updateTaskMock.mock.calls[0][1]).toEqual({ name: '新任务名', branch_slug: 'old-slug' });
    expect(updatePermissionModeMock).not.toHaveBeenCalled();
  });

  it('保存时模式变更：先通用 PATCH 后专用模式端点，模式不经通用 PATCH 字段', async () => {
    updateTaskMock.mockResolvedValue(makeTask());
    updatePermissionModeMock.mockResolvedValue({
      permission_mode: 'all-approve',
      effective_permission_mode: 'all-approve',
    });
    const { container } = renderCard();
    await enterEdit(container);
    await selectMode(modeSelect(container), 'all-approve');
    await click(btnByText(container, '保存'));
    expect(updateTaskMock).toHaveBeenCalledTimes(1);
    expect(updateTaskMock.mock.calls[0][1]).toEqual({ name: '旧任务名', branch_slug: 'old-slug' });
    expect('permission_mode' in updateTaskMock.mock.calls[0][1]).toBe(false);
    expect(updatePermissionModeMock).toHaveBeenCalledTimes(1);
    expect(updatePermissionModeMock).toHaveBeenCalledWith('t1', 'all-approve');
    // 顺序契约：通用 PATCH 先于模式端点
    expect(updateTaskMock.mock.invocationCallOrder[0]).toBeLessThan(
      updatePermissionModeMock.mock.invocationCallOrder[0],
    );
    // 成功以响应收敛（两值一致）：回到查看态展示新模式、无提示
    expect(container.querySelector('#task-info-permission-mode')).toBeNull();
    expect(container.textContent).toContain('全部批准');
    expect(container.textContent).not.toContain('下次激活生效');
  });

  it('两值不一致：常驻提示在查看态与编辑态都展示；一致：无提示', async () => {
    const { container } = renderCard(
      makeTask({ permission_mode: 'all-approve', effective_permission_mode: 'ask' }),
    );
    expect(container.textContent).toContain(
      '当前运行进程仍按「人工批准」处理，新模式将在下次激活生效',
    );
    await enterEdit(container);
    expect(container.textContent).toContain(
      '当前运行进程仍按「人工批准」处理，新模式将在下次激活生效',
    );

    const { container: c2 } = renderCard(
      makeTask({ permission_mode: 'all-approve', effective_permission_mode: 'all-approve' }),
    );
    expect(c2.textContent).not.toContain('下次激活生效');
    await enterEdit(c2);
    expect(c2.textContent).not.toContain('下次激活生效');
  });

  it('保存成功且无法立即生效：瞬时确认展示；两值一致保存：无瞬时确认', async () => {
    updateTaskMock.mockResolvedValue(makeTask());
    // 响应两值不一致（all-approve 变更下次激活生效）→ 瞬时确认（回到查看态仍可见）
    updatePermissionModeMock.mockResolvedValue({
      permission_mode: 'all-approve',
      effective_permission_mode: 'ask',
    });
    const { container } = renderCard();
    await enterEdit(container);
    await selectMode(modeSelect(container), 'all-approve');
    await click(btnByText(container, '保存'));
    expect(container.textContent).toContain('已保存，将在下次激活后生效。');
    expect(container.textContent).toContain(
      '当前运行进程仍按「人工批准」处理，新模式将在下次激活生效',
    );

    // 响应两值一致（ask↔ai-auto 即时生效）→ 无瞬时确认
    updateTaskMock.mockResolvedValue(makeTask());
    updatePermissionModeMock.mockResolvedValue({
      permission_mode: 'ai-auto',
      effective_permission_mode: 'ai-auto',
    });
    const { container: c2 } = renderCard();
    await enterEdit(c2);
    await selectMode(modeSelect(c2), 'ai-auto');
    await click(btnByText(c2, '保存'));
    expect(c2.textContent).not.toContain('已保存，将在下次激活后生效。');
  });

  it('保存期间禁用（I4）：模式保存+复验全程「编辑」按钮禁用，收敛后可进且草稿按新基准初始化', async () => {
    updateTaskMock.mockResolvedValue(makeTask());
    let resolveMode!: (v: TaskPermissionModeView) => void;
    updatePermissionModeMock.mockImplementation(
      () =>
        new Promise<TaskPermissionModeView>((res) => {
          resolveMode = res;
        }),
    );
    const { container } = renderCard();
    await enterEdit(container);
    await selectMode(modeSelect(container), 'all-approve');
    await click(btnByText(container, '保存'));
    // 通用 PATCH 已完成、模式端点在途：已退出编辑态，但「编辑」按钮禁用（I4）——
    // 禁止在专用请求收敛前重新进入（否则 draftMode 按旧已保存值初始化，会改回刚保存的模式）
    expect(updatePermissionModeMock).toHaveBeenCalledTimes(1);
    const editBtn = btnByText(container, '编辑');
    expect(editBtn.disabled).toBe(true);
    await click(editBtn); // 禁用态点击无效
    expect(container.querySelector('#task-info-permission-mode')).toBeNull();
    // 收敛后解禁；再进编辑态草稿按新已保存值初始化
    await act(async () => {
      resolveMode({ permission_mode: 'all-approve', effective_permission_mode: 'all-approve' });
    });
    await flushUI();
    expect(btnByText(container, '编辑').disabled).toBe(false);
    await enterEdit(container);
    expect(modeSelect(container).value).toBe('all-approve');
    expect(modeSelect(container).disabled).toBe(false);
    expect(saveBtn(container).disabled).toBe(false);
    // 端点未因期间操作重发
    expect(updatePermissionModeMock).toHaveBeenCalledTimes(1);
  });

  it('模式保存失败（网络错误）：结果不确定 → GET 复验以服务端当前状态收敛后解禁', async () => {
    updateTaskMock.mockResolvedValue(makeTask());
    updatePermissionModeMock.mockRejectedValueOnce(
      new ApiError(0, 'network_error', '无法连接服务端'),
    );
    // 服务端在失败响应前已提交（saved=all-approve），运行中 effective 仍为 ask
    getTaskMock.mockResolvedValue(
      makeTask({ permission_mode: 'all-approve', effective_permission_mode: 'ask' }),
    );
    const { container } = renderCard();
    await enterEdit(container);
    await selectMode(modeSelect(container), 'all-approve');
    await click(btnByText(container, '保存'));
    expect(updatePermissionModeMock).toHaveBeenCalledTimes(1); // MUST NOT 自动重发
    expect(getTaskMock).toHaveBeenCalledTimes(1);
    expect(getTaskMock).toHaveBeenCalledWith('t1');
    // 复验收敛后回到查看态展示服务端当前值，常驻提示与收敛说明可见
    expect(container.querySelector('#task-info-permission-mode')).toBeNull();
    expect(container.textContent).toContain('全部批准');
    expect(container.textContent).toContain(
      '当前运行进程仍按「人工批准」处理，新模式将在下次激活生效',
    );
    expect(container.textContent).toContain('已按服务端当前状态收敛');
    // 再进编辑态：选择器按收敛后的已保存值初始化、已解禁
    await enterEdit(container);
    const sel = modeSelect(container);
    expect(sel.disabled).toBe(false);
    expect(sel.value).toBe('all-approve');
  });

  it('复验失败：选择器保持禁用并提供重试复验；重试成功后收敛解禁', async () => {
    updateTaskMock.mockResolvedValue(makeTask());
    updatePermissionModeMock.mockRejectedValueOnce(
      new ApiError(0, 'network_error', '无法连接服务端'),
    );
    getTaskMock.mockRejectedValueOnce(new ApiError(0, 'network_error', '无法连接服务端'));
    const { container } = renderCard();
    await enterEdit(container);
    await selectMode(modeSelect(container), 'all-approve');
    await click(btnByText(container, '保存'));
    expect(container.textContent).toContain('复验失败');
    // 结果不确定门禁（I4）：复验未收敛前「编辑」按钮禁用，禁止重新进入编辑态
    const editBtn = btnByText(container, '编辑');
    expect(editBtn.disabled).toBe(true);
    await click(editBtn); // 禁用态点击无效
    expect(container.querySelector('#task-info-permission-mode')).toBeNull();
    expect(updatePermissionModeMock).toHaveBeenCalledTimes(1); // MUST NOT 自动重发
    // 重试复验成功 → 收敛解禁，再进编辑态选择器按收敛后的已保存值初始化
    getTaskMock.mockResolvedValue(makeTask());
    await click(btnByText(container, '重试复验'));
    expect(getTaskMock).toHaveBeenCalledTimes(2);
    expect(container.textContent).toContain('已按服务端当前状态收敛');
    await enterEdit(container);
    const sel = modeSelect(container);
    expect(sel.disabled).toBe(false);
    expect(sel.value).toBe('ask'); // 服务端仍是原值
  });

  it('重叠复验：新复验作废旧在途流——旧复验响应迟到 MUST NOT 覆盖收敛结果（序列令牌）', async () => {
    updateTaskMock.mockResolvedValue(makeTask());
    updatePermissionModeMock.mockRejectedValueOnce(
      new ApiError(0, 'network_error', '无法连接服务端'),
    );
    getTaskMock.mockRejectedValueOnce(new ApiError(0, 'network_error', '无法连接服务端')); // 自动复验失败
    const { container } = renderCard();
    await enterEdit(container);
    await selectMode(modeSelect(container), 'all-approve');
    await click(btnByText(container, '保存'));
    expect(container.textContent).toContain('复验失败');

    let resolveOld!: (t: TaskDetail) => void;
    getTaskMock
      .mockImplementationOnce(
        () =>
          new Promise<TaskDetail>((res) => {
            resolveOld = res; // 重试1：慢
          }),
      )
      .mockResolvedValueOnce(makeTask()); // 重试2：快（服务端仍为 ask）
    const retry = btnByText(container, '重试复验');
    // 同一渲染闭包内连击两次（禁用态尚未生效）：第二次重试超越第一次在途流
    await act(async () => {
      retry.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
      retry.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
    });
    await flushUI();
    expect(getTaskMock).toHaveBeenCalledTimes(3);
    expect(container.textContent).toContain('已按服务端当前状态收敛');
    expect(container.textContent).toContain('人工批准');
    // 旧复验迟到：不得覆盖新收敛结果（旧流谎报 all-approve）
    await act(async () => {
      resolveOld(makeTask({ permission_mode: 'all-approve' }));
    });
    await flushUI();
    expect(container.textContent).not.toContain('全部批准');
    await enterEdit(container);
    expect(modeSelect(container).value).toBe('ask'); // 新复验的收敛结果保留
  });

  it('I5：编辑期间 SSE 更新服务端模式，用户未动选择器——保存名称不调模式端点', async () => {
    updateTaskMock.mockResolvedValue(makeTask({ name: '新任务名' }));
    const pushRef = { current: null as ((t: TaskDetail) => void) | null };
    const { container } = renderCard(makeTask(), pushRef);
    await enterEdit(container);
    // 编辑期间 SSE 把服务端模式从 ask 更新为 ai-auto（用户未动选择器，草稿跟随基准 ask）
    await act(async () => {
      pushRef.current!(
        makeTask({ permission_mode: 'ai-auto', effective_permission_mode: 'ai-auto' }),
      );
    });
    await act(async () => {
      setInput(nameInput(container)!, '新任务名');
    });
    await click(btnByText(container, '保存'));
    expect(updateTaskMock).toHaveBeenCalledTimes(1);
    expect(updateTaskMock.mock.calls[0][1]).toEqual({ name: '新任务名', branch_slug: 'old-slug' });
    // 草稿相对编辑基准未变 → MUST NOT 调模式端点（不得把外部更新改回 ask）
    expect(updatePermissionModeMock).not.toHaveBeenCalled();
  });

  it('I5：编辑期间 SSE 更新服务端模式，用户动过选择器——仍按草稿提交专用端点', async () => {
    updateTaskMock.mockResolvedValue(makeTask());
    updatePermissionModeMock.mockResolvedValue({
      permission_mode: 'all-approve',
      effective_permission_mode: 'all-approve',
    });
    const pushRef = { current: null as ((t: TaskDetail) => void) | null };
    const { container } = renderCard(makeTask(), pushRef);
    await enterEdit(container);
    await selectMode(modeSelect(container), 'all-approve'); // 用户主动改模式
    // 编辑期间 SSE 把服务端模式更新为 ai-auto：不影响用户草稿的提交语义
    await act(async () => {
      pushRef.current!(
        makeTask({ permission_mode: 'ai-auto', effective_permission_mode: 'ai-auto' }),
      );
    });
    await click(btnByText(container, '保存'));
    expect(updatePermissionModeMock).toHaveBeenCalledTimes(1);
    expect(updatePermissionModeMock).toHaveBeenCalledWith('t1', 'all-approve');
  });

  it('I6：瞬时确认在下一次保存开始（仅改名称、不调模式端点）时清除', async () => {
    updateTaskMock.mockResolvedValue(makeTask());
    updatePermissionModeMock.mockResolvedValue({
      permission_mode: 'all-approve',
      effective_permission_mode: 'ask',
    });
    const { container } = renderCard();
    await enterEdit(container);
    await selectMode(modeSelect(container), 'all-approve');
    await click(btnByText(container, '保存'));
    expect(container.textContent).toContain('已保存，将在下次激活后生效。');
    // 下一次保存只改名称（模式相对新基准未变，不进模式端点）：瞬时确认仍按约定清除
    updateTaskMock.mockResolvedValue(
      makeTask({ name: '新任务名', permission_mode: 'all-approve', effective_permission_mode: 'ask' }),
    );
    await enterEdit(container);
    await act(async () => {
      setInput(nameInput(container)!, '新任务名');
    });
    await click(btnByText(container, '保存'));
    expect(updatePermissionModeMock).toHaveBeenCalledTimes(1); // 未再调模式端点
    expect(container.textContent).not.toContain('已保存，将在下次激活后生效。');
    // 常驻提示（两值仍不一致）不受瞬时确认清除影响
    expect(container.textContent).toContain(
      '当前运行进程仍按「人工批准」处理，新模式将在下次激活生效',
    );
  });
});

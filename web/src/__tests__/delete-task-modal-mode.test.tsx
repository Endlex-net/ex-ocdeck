// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { act } from 'react';
import { DeleteTaskModal } from '../components/DeleteTaskModal';
import { api, ApiError } from '../api';
import { mount, flushUI } from './cm-test-env';
import type { Task } from '../types';

/* ==================== 删除确认弹窗文案分叉（add-local-path-task-mode tasks 5.7） ====================
 * 覆盖 task-lifecycle delta 弹窗场景：
 * - dir 任务：「…不会删除项目目录及其内容」；repo local-path 任务另注明「不改动 git 状态」；
 * - normal 且配置 pre_delete script 时提示脚本仍会执行；
 * - 两者均不出现 worktree/dirty 删除确认项（409 拒绝后亦然）；
 * - repo worktree 任务保持既有文案与 dirty 确认项（回归基线）。 */

vi.mock('../api', () => ({
  api: {
    getLifecycleConfig: vi.fn(),
    deleteTask: vi.fn(),
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

function makeTask(over: Partial<Task>): Task {
  return {
    id: 't1',
    project_id: 'p1',
    project_kind: 'repo',
    name: 'demo-task',
    branch: '',
    status: 'suspended',
    worktree_path: '/tmp/wt',
    mode: 'worktree',
    permission_mode: 'ask',
    init_status: 'none',
    created_at: 1,
    updated_at: 2,
    ...over,
  };
}

/** dir 任务落库恒为 mode=local-path（design D3）。 */
const DIR_TASK = makeTask({ project_kind: 'dir', mode: 'local-path' });
const LOCAL_PATH_TASK = makeTask({ project_kind: 'repo', mode: 'local-path', branch: '' });

function confirmBtn(container: HTMLElement) {
  return [...container.querySelectorAll<HTMLButtonElement>('.modal-actions button')].find(
    (b) => b.textContent === '删除',
  )!;
}

const NO_PRE_DELETE = { inherit_patterns: '', init_script: '', pre_delete_script: '' };

beforeEach(() => {
  vi.mocked(api.getLifecycleConfig).mockReset();
  vi.mocked(api.deleteTask).mockReset();
});

describe('DeleteTaskModal 删除弹窗文案分叉（add-local-path-task-mode 5.7）', () => {
  it('dir 任务：文案注明仅删记录与会话数据、不删项目目录，无 git 表述与 pre-delete 提示', async () => {
    vi.mocked(api.getLifecycleConfig).mockResolvedValue(NO_PRE_DELETE);
    const { container, unmount } = mount(<DeleteTaskModal task={DIR_TASK} onClose={() => {}} onDeleted={() => {}} />);
    await flushUI();

    expect(api.getLifecycleConfig).toHaveBeenCalledWith('p1');
    expect(container.textContent).toContain(
      '确认删除任务 demo-task？仅删除任务记录与 opencode 会话数据，不会删除项目目录及其内容。',
    );
    expect(container.textContent).not.toContain('不改动 git 状态');
    expect(container.textContent).not.toContain('该操作会删除对应 worktree，不可恢复');
    expect(container.textContent).not.toContain('该项目配置了 pre-delete 脚本');
    unmount();
  });

  it('repo local-path 任务：文案另注明不改动 git 状态', async () => {
    vi.mocked(api.getLifecycleConfig).mockResolvedValue(NO_PRE_DELETE);
    const { container, unmount } = mount(
      <DeleteTaskModal task={LOCAL_PATH_TASK} onClose={() => {}} onDeleted={() => {}} />,
    );
    await flushUI();

    expect(container.textContent).toContain(
      '确认删除任务 demo-task？仅删除任务记录与 opencode 会话数据，不会删除项目目录及其内容、不改动 git 状态。',
    );
    expect(container.textContent).not.toContain('该操作会删除对应 worktree，不可恢复');
    unmount();
  });

  it('normal 且配置 pre-delete script：dir 与 repo local-path 均提示脚本仍会执行', async () => {
    vi.mocked(api.getLifecycleConfig).mockResolvedValue({
      inherit_patterns: '',
      init_script: '',
      pre_delete_script: 'cleanup.sh',
    });
    for (const task of [DIR_TASK, LOCAL_PATH_TASK]) {
      const { container, unmount } = mount(
        <DeleteTaskModal task={task} onClose={() => {}} onDeleted={() => {}} />,
      );
      await flushUI();
      expect(container.textContent).toContain(
        '该项目配置了 pre-delete 脚本，删除时仍会在项目目录下执行。',
      );
      unmount();
    }
  });

  it('in-place 任务 409 拒绝后不出现 dirty/worktree 确认项', async () => {
    vi.mocked(api.getLifecycleConfig).mockResolvedValue(NO_PRE_DELETE);
    vi.mocked(api.deleteTask).mockRejectedValue(new ApiError(409, 'conflict', '存在未提交改动'));
    for (const task of [DIR_TASK, LOCAL_PATH_TASK]) {
      const { container, unmount } = mount(
        <DeleteTaskModal task={task} onClose={() => {}} onDeleted={() => {}} />,
      );
      // fail-closed：pre-delete 配置确认前（loading 态）按钮禁用
      expect(container.textContent).toContain('正在确认项目 pre-delete 脚本配置…');
      expect(confirmBtn(container).disabled).toBe(true);
      await flushUI();
      expect(confirmBtn(container).disabled).toBe(false);
      await act(async () => {
        confirmBtn(container).click();
      });
      await flushUI();
      expect(api.deleteTask).toHaveBeenCalledWith('t1', 'normal', false);
      expect(container.textContent).not.toContain('我已了解 worktree 存在未提交改动');
      expect(container.textContent).toContain('存在未提交改动'); // 错误原因仍展示
      unmount();
    }
  });

  it('回归基线：repo worktree 任务保持既有文案与 409 后 dirty 确认项，不查 lifecycle 配置', async () => {
    const task = makeTask({ branch: 'ocdeck/demo' });
    const { container, unmount } = mount(<DeleteTaskModal task={task} onClose={() => {}} onDeleted={() => {}} />);
    await flushUI();

    expect(api.getLifecycleConfig).not.toHaveBeenCalled();
    expect(container.textContent).toContain('（分支 ocdeck/demo）');
    expect(container.textContent).toContain('该操作会删除对应 worktree，不可恢复。');

    vi.mocked(api.deleteTask).mockRejectedValue(new ApiError(409, 'conflict', 'dirty worktree'));
    await act(async () => {
      confirmBtn(container).click();
    });
    await flushUI();
    expect(container.textContent).toContain('我已了解 worktree 存在未提交改动');
    unmount();
  });
});

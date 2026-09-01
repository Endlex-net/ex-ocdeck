// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { TaskWorkbenchPage } from '../pages/TaskWorkbenchPage';
import { subscribeTask } from '../sse';
import { mount } from './cm-test-env';
import type { Project, Task } from '../types';

/* ============================ TaskWorkbenchPage「等待人工」数据链路实证 ============================
 * 用户实测反馈「task 页面没有生效」——本文件构造 pending question 场景走真实页面渲染路径，
 * 验证：详情流 attention.questions 非空 → 页头徽标变蓝；共享 store attention_count>0 → 切换器行蓝点。 */

type TaskSubOpts = {
  onData: (t: Task) => void;
  onError: (m: string) => void;
  onGone: () => void;
};
let taskSub: TaskSubOpts | null = null;
/** 共享 store 快照（useProjects mock 读取，用例间替换）。 */
let storeProjects: Project[] = [];
/** 窄屏（≤767px）开关：任务切换器仅 mobile 渲染（design D12 裁决 3）。 */
let mobile = false;

vi.mock('../sse', () => ({
  subscribeTask: vi.fn((_id: string, opts: TaskSubOpts) => {
    taskSub = opts;
    return { close: vi.fn() };
  }),
}));

vi.mock('../api', () => ({
  api: { listTerminals: vi.fn(async () => []) },
  ApiError: class ApiError extends Error {
    constructor(
      public code: string,
      message: string,
    ) {
      super(message);
    }
  },
}));

vi.mock('../hooks', () => ({
  useMediaQuery: (q: string) => (q.includes('767') ? mobile : false),
  useProjects: () => ({ projects: storeProjects }),
  useProjectsRefresh: () => vi.fn(async () => {}),
}));

/* 终端/面板重依赖打桩：本测试只关心页头徽标与切换器，不加载 xterm/git 面板。 */
vi.mock('../terminal/TerminalView', () => ({ TerminalView: () => null }));
vi.mock('../components/GitPanel', () => ({ GitPanel: () => null }));
vi.mock('../components/EnvEditor', () => ({ EnvEditor: () => null }));

function makeTask(over: Partial<Task>): Task {
  return {
    id: 't1',
    project_id: 'p1',
    project_kind: 'repo',
    name: 'demo-task',
    branch: 'main',
    status: 'active',
    worktree_path: '/tmp/wt',
    init_status: 'none',
    created_at: 1,
    updated_at: 2,
    ...over,
  };
}

function makeProjects(attentionCount: number): Project[] {
  return [
    {
      id: 'p1',
      name: 'proj',
      path: '/tmp/proj',
      kind: 'repo',
      default_branch: 'main',
      created_at: 1,
      task_count: 1,
      tasks_by_status: { active: 1 },
      tasks: [
        {
          id: 't1',
          name: 'demo-task',
          status: 'active',
          init_status: 'none',
          branch: 'main',
          worktree_path: '/tmp/wt',
          updated_at: 2,
          agentStatus: 'busy',
          attention_count: attentionCount,
        },
      ],
    },
  ];
}

const PENDING_QUESTION = {
  permissions: [],
  questions: [{ id: 'q1', questions: [{ header: '确认', question: '继续吗？' }], since: 1 }],
};

beforeEach(() => {
  taskSub = null;
  storeProjects = [];
  mobile = false;
  vi.mocked(subscribeTask).mockClear();
});

describe('TaskWorkbenchPage 等待人工数据链路', () => {
  it('详情流 attention.questions 非空：页头徽标变蓝（覆盖 busy 绿点）', () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    expect(taskSub).not.toBeNull();
    act(() =>
      taskSub!.onData(makeTask({ agentStatus: 'busy', attention: PENDING_QUESTION })),
    );
    const badge = container.querySelector('.od-agent.od-agent-attention');
    expect(badge).not.toBeNull();
    expect(badge?.textContent).toContain('等待人工');
    expect(badge?.getAttribute('title')).toBe('等待人工处理：1 个待答问题');
    unmount();
  });

  it('详情流 attention 空数组：页头徽标保持运行态（busy 绿点）', () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() =>
      taskSub!.onData(
        makeTask({ agentStatus: 'busy', attention: { permissions: [], questions: [] } }),
      ),
    );
    expect(container.querySelector('.od-agent-attention')).toBeNull();
    expect(container.querySelector('.od-agent-busy')?.textContent).toContain('工作中');
    unmount();
  });

  it('流推送更新（question 出现 → 再清空）：徽标随帧切换 蓝 → 绿', () => {
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask({ agentStatus: 'busy', attention: PENDING_QUESTION })));
    expect(container.querySelector('.od-agent-attention')).not.toBeNull();
    act(() =>
      taskSub!.onData(
        makeTask({ agentStatus: 'busy', attention: { permissions: [], questions: [] } }),
      ),
    );
    expect(container.querySelector('.od-agent-attention')).toBeNull();
    expect(container.querySelector('.od-agent-busy')).not.toBeNull();
    unmount();
  });

  it('任务切换器行（mobile）：共享 store attention_count>0 → 蓝点 + 计数 title', () => {
    mobile = true;
    storeProjects = makeProjects(2);
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask({ agentStatus: 'busy' })));
    const btn = container.querySelector<HTMLButtonElement>('.wb-switcher-btn');
    expect(btn).not.toBeNull();
    act(() => btn!.dispatchEvent(new MouseEvent('click', { bubbles: true })));
    const dot = container.querySelector('.wb-sw-item .od-agent.od-agent-attention');
    expect(dot).not.toBeNull();
    expect(dot?.getAttribute('title')).toBe('等待人工处理：2 个待处理请求');
    unmount();
  });

  it('任务切换器行（mobile）：attention_count=0 → 保持原运行态点', () => {
    mobile = true;
    storeProjects = makeProjects(0);
    const { container, unmount } = mount(<TaskWorkbenchPage taskID="t1" />);
    act(() => taskSub!.onData(makeTask({ agentStatus: 'busy' })));
    act(() =>
      container
        .querySelector<HTMLButtonElement>('.wb-switcher-btn')!
        .dispatchEvent(new MouseEvent('click', { bubbles: true })),
    );
    expect(container.querySelector('.wb-sw-item .od-agent-attention')).toBeNull();
    expect(container.querySelector('.wb-sw-item .od-agent-busy')).not.toBeNull();
    unmount();
  });
});

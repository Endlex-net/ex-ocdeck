// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { CommandCenterPage } from '../pages/CommandCenterPage';
import { subscribeActiveSessions } from '../sse';
import { mount } from './cm-test-env';
import type { ActiveSessionItem, Project } from '../types';

/* ============================ 指挥中心「等待人工」蓝点覆盖 ============================
 * 「需要关注」行（attention 区）与「其余活跃任务」行的 AgentStatusBadge 同一套设计语言：
 * pending question/permission → 蓝点「等待人工」，覆盖 busy/retry 运行态。 */

type SessionsSubOpts = {
  onData: (items: ActiveSessionItem[]) => void;
  onError: (m: string) => void;
};
let sessionsSub: SessionsSubOpts | null = null;
let storeProjects: Project[] = [];

vi.mock('../sse', () => ({
  subscribeActiveSessions: vi.fn((opts: SessionsSubOpts) => {
    sessionsSub = opts;
    return { close: vi.fn() };
  }),
}));

vi.mock('../api', () => ({
  api: {
    refreshBranches: vi.fn(async () => []),
    createTask: vi.fn(),
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

vi.mock('../hooks', () => ({
  useProjects: () => ({ projects: storeProjects, initialized: true, error: '' }),
  useProjectsRefresh: () => vi.fn(async () => {}),
  createErrorMessage: (_prefix: string, err: unknown) =>
    err instanceof Error ? err.message : String(err),
}));

function makeProjects(agentStatus = 'busy'): Project[] {
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
          agentStatus,
          attention_count: 2,
        },
      ],
    },
  ];
}

function makeSession(attention: ActiveSessionItem['attention']): ActiveSessionItem {
  return {
    task_id: 't1',
    project_id: 'p1',
    project_name: 'proj',
    name: 'demo-task',
    branch: 'main',
    worktree_path: '/tmp/wt',
    last_active_at: 100,
    agentStatus: 'busy',
    attention,
  };
}

beforeEach(() => {
  sessionsSub = null;
  storeProjects = makeProjects();
  vi.mocked(subscribeActiveSessions).mockClear();
});

describe('CommandCenterPage 等待人工', () => {
  it('「需要关注」行：attention.questions 非空 → 蓝点等待人工（覆盖 busy）', () => {
    const { container, unmount } = mount(<CommandCenterPage />);
    act(() =>
      sessionsSub!.onData([
        makeSession({
          permissions: [],
          questions: [{ id: 'q1', questions: [{ header: 'h', question: 'w' }], since: 1 }],
        }),
      ]),
    );
    const badge = container.querySelector('.cc-attention .od-agent.od-agent-attention');
    expect(badge).not.toBeNull();
    expect(badge?.textContent).toContain('等待人工');
    unmount();
  });

  it('「需要关注」行：attention 空 → 保持原运行态徽标', () => {
    storeProjects = makeProjects('idle');
    const { container, unmount } = mount(<CommandCenterPage />);
    // idle 活跃任务进需要关注（AgentIdle 类），无 question/permission → 不变蓝
    act(() => {
      const s = makeSession({ permissions: [], questions: [] });
      s.agentStatus = 'idle';
      sessionsSub!.onData([s]);
    });
    expect(container.querySelector('.cc-attention .od-agent-attention')).toBeNull();
    expect(container.querySelector('.cc-attention .od-agent-idle')).not.toBeNull();
    unmount();
  });
});

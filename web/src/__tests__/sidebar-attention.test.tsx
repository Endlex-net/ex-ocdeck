// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { AppShell } from '../components/AppShell';
import { mount } from './cm-test-env';
import type { Project } from '../types';

/* ============================ 侧栏任务行「等待人工」蓝点 ============================
 * 同一套 od-agent 状态语言：attention_count>0 → 蓝点（状态职责），
 * 右侧 od-nav-attention 计数丸保留计数职责，两者共存不冲突。 */

let storeProjects: Project[] = [];

vi.mock('../hooks', () => ({
  useProjects: () => ({ projects: storeProjects }),
}));

vi.mock('../components/ServerStatusBanner', () => ({ ServerStatusBanner: () => null }));

function makeProjects(attentionCount: number, agentStatus = 'busy'): Project[] {
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
          attention_count: attentionCount,
        },
      ],
    },
  ];
}

function renderShell() {
  return mount(
    <AppShell onOpenPalette={() => {}} onToggleTheme={() => {}} themePref="system">
      <div />
    </AppShell>,
  );
}

beforeEach(() => {
  storeProjects = [];
});

describe('侧栏任务行 等待人工', () => {
  it('attention_count>0：状态点变蓝（覆盖 busy 绿点），计数丸保留计数', () => {
    storeProjects = makeProjects(2);
    const { container, unmount } = renderShell();
    const dot = container.querySelector('.od-nav-task .od-agent.od-agent-attention');
    expect(dot).not.toBeNull();
    // 计数丸仍在，职责分工：圆点=状态，计数丸=数量
    const pill = container.querySelector('.od-nav-task .od-nav-attention');
    expect(pill?.textContent).toBe('2');
    expect(container.querySelector('.od-nav-task .od-agent-busy')).toBeNull();
    unmount();
  });

  it('attention_count=0：保持原运行态点，无计数丸', () => {
    storeProjects = makeProjects(0);
    const { container, unmount } = renderShell();
    expect(container.querySelector('.od-nav-task .od-agent-attention')).toBeNull();
    expect(container.querySelector('.od-nav-task .od-agent-busy')).not.toBeNull();
    expect(container.querySelector('.od-nav-attention')).toBeNull();
    unmount();
  });
});

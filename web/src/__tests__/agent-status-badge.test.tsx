// @vitest-environment jsdom
import { describe, it, expect } from 'vitest';
import { AgentStatusBadge } from '../components/AgentStatusBadge';
import type { Attention } from '../types';
import { mount } from './cm-test-env';

/* ============================ AgentStatusBadge「等待人工」态 ============================
 * 优先级：attention pending（questions/permissions 非空或 attention_count > 0）> busy > retry > idle。 */

function attention(over: Partial<Attention>): Attention {
  return { permissions: [], questions: [], ...over };
}

describe('AgentStatusBadge 等待人工', () => {
  it('attention.questions 非空：覆盖 busy，蓝点 + 分类计数 title', () => {
    const { container, unmount } = mount(
      <AgentStatusBadge
        agentStatus="busy"
        attention={attention({ questions: [{ id: 'q1', questions: [{ header: 'h', question: 'w' }], since: 1 }] })}
      />,
    );
    const el = container.querySelector('.od-agent-attention');
    expect(el).not.toBeNull();
    expect(el?.textContent).toContain('等待人工');
    expect(el?.getAttribute('title')).toBe('等待人工处理：1 个待答问题');
    expect(container.querySelector('.od-agent-busy')).toBeNull();
    unmount();
  });

  it('questions + permissions 同时非空：title 带双分类计数', () => {
    const { container, unmount } = mount(
      <AgentStatusBadge
        agentStatus="retry"
        attention={attention({
          permissions: [
            { id: 'r1', permission: 'Edit', patterns: [], since: 1 },
            { id: 'r2', permission: 'Bash', patterns: [], since: 2 },
          ],
          questions: [{ id: 'q1', questions: [{ header: 'h', question: 'w' }], since: 1 }],
        })}
      />,
    );
    const el = container.querySelector('.od-agent-attention');
    expect(el?.getAttribute('title')).toBe('等待人工处理：1 个待答问题，2 个待授权限');
    unmount();
  });

  it('仅 attention_count（无明细）：覆盖 idle，title 退化为总数', () => {
    const { container, unmount } = mount(<AgentStatusBadge agentStatus="idle" attentionCount={3} />);
    const el = container.querySelector('.od-agent-attention');
    expect(el).not.toBeNull();
    expect(el?.getAttribute('title')).toBe('等待人工处理：3 个待处理请求');
    unmount();
  });

  it('attention 空数组：回退原运行态', () => {
    const { container, unmount } = mount(<AgentStatusBadge agentStatus="busy" attention={attention({})} attentionCount={0} />);
    expect(container.querySelector('.od-agent-attention')).toBeNull();
    expect(container.querySelector('.od-agent-busy')?.textContent).toContain('工作中');
    unmount();
  });

  it('无 agentStatus 且无 attention：不渲染', () => {
    const { container, unmount } = mount(<AgentStatusBadge agentStatus="" attentionCount={0} />);
    expect(container.querySelector('.od-agent')).toBeNull();
    unmount();
  });

  it('agentStatus 缺失但有待处理请求：仍渲染等待人工', () => {
    const { container, unmount } = mount(<AgentStatusBadge attentionCount={1} />);
    expect(container.querySelector('.od-agent-attention')).not.toBeNull();
    unmount();
  });
});

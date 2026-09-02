import type { Attention } from '../types';

const AGENT_META: Record<string, { label: string; cls: string; title: string }> = {
  idle: { label: '空闲', cls: 'od-agent-idle', title: 'agent 空闲，等待输入' },
  busy: { label: '工作中', cls: 'od-agent-busy', title: 'agent 正在处理' },
  retry: { label: '重试中', cls: 'od-agent-retry', title: 'agent 重试中' },
};

/** 等待人工 tooltip：有 attention 明细时带分类计数，仅计数（attention_count）时退化为总数。 */
function attentionTitle(questions: number, permissions: number, pending: number): string {
  const parts: string[] = [];
  if (questions > 0) parts.push(`${questions} 个待答问题`);
  if (permissions > 0) parts.push(`${permissions} 个待授权限`);
  const detail = parts.length > 0 ? parts.join('，') : `${pending} 个待处理请求`;
  return `等待人工处理：${detail}`;
}

/** 任务 agent 运行态徽标（design.md 2.8，od-agent 脉冲点 + 中文文案）。空串（非 active/查询失败）不渲染。
 *  有待处理的提问/授权请求（attention 或 attentionCount > 0）时覆盖为「等待人工」蓝点，
 *  优先级：等待人工 > busy > retry > idle。 */
export function AgentStatusBadge({
  agentStatus,
  attention,
  attentionCount,
}: {
  agentStatus?: string;
  /** 注意力信号快照（task detail DTO 透出）：非空 questions/permissions 触发「等待人工」。 */
  attention?: Attention;
  /** 仅计数场景（TaskSummary.attention_count）：>0 同样触发，tooltip 无分类明细。 */
  attentionCount?: number;
}) {
  const questions = attention?.questions.length ?? 0;
  const permissions = attention?.permissions.length ?? 0;
  const pending = questions + permissions > 0 ? questions + permissions : (attentionCount ?? 0);
  if (pending > 0) {
    return (
      <span className="od-agent od-agent-attention" title={attentionTitle(questions, permissions, pending)}>
        <span className="od-agent-dot" aria-hidden />
        等待人工
      </span>
    );
  }
  if (!agentStatus) return null;
  const meta = AGENT_META[agentStatus];
  if (!meta) return null;
  return (
    <span className={`od-agent ${meta.cls}`} title={meta.title}>
      <span className="od-agent-dot" aria-hidden />
      {meta.label}
    </span>
  );
}

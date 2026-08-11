const AGENT_META: Record<string, { label: string; cls: string; title: string }> = {
  idle: { label: '空闲', cls: 'od-agent-idle', title: 'agent 空闲，等待输入' },
  busy: { label: '工作中', cls: 'od-agent-busy', title: 'agent 正在处理' },
  retry: { label: '重试中', cls: 'od-agent-retry', title: 'agent 重试中' },
};

/** 任务 agent 运行态徽标（design.md 2.8，od-agent 脉冲点 + 中文文案）。空串（非 active/查询失败）不渲染。 */
export function AgentStatusBadge({ agentStatus }: { agentStatus?: string }) {
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

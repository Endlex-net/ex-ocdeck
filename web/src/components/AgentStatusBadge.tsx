const AGENT_META: Record<string, { label: string; cls: string; title: string }> = {
  idle: { label: 'agent idle', cls: 'agent-idle', title: 'agent 空闲，等待输入' },
  busy: { label: 'agent busy', cls: 'agent-busy', title: 'agent 正在处理' },
  retry: { label: 'agent retry', cls: 'agent-retry', title: 'agent 重试中' },
};

/** 任务 agent 运行态徽标（design.md 2.8）。空串（非 active/查询失败）不渲染。 */
export function AgentStatusBadge({ agentStatus }: { agentStatus?: string }) {
  if (!agentStatus) return null;
  const meta = AGENT_META[agentStatus];
  if (!meta) return null;
  return (
    <span className={`badge ${meta.cls}`} title={meta.title}>
      <span className="agent-dot" aria-hidden />
      {meta.label}
    </span>
  );
}

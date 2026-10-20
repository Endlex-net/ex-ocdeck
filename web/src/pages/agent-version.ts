import type { ServerStatus } from '../types';

/** 设置页「Agent 配置」子项的版本标识取值。 */
export type AgentVersionLabel = 'v1' | 'v2' | '未知';

/**
 * 从 GET /api/v1/server/status 的 opencodeVersion 判定 agent 版本标识：
 * 主版本 1 → 'v1'，2 → 'v2'，其余（空值/非数字主版本/其他主版本）→ '未知'。
 * 注：capabilities 快照空闲时 session 恒为 null，不含 provider 身份，
 * 不能作为版本信号；opencodeVersion 是检测到的 opencode CLI 版本，直接对应 v1/v2。
 */
export function detectAgentVersion(status: ServerStatus | null | undefined): AgentVersionLabel {
  const raw = status?.opencodeVersion?.trim();
  if (!raw) return '未知';
  const majorStr = raw.replace(/^v/i, '').split('.')[0];
  const major = Number.parseInt(majorStr, 10);
  if (major === 1) return 'v1';
  if (major === 2) return 'v2';
  return '未知';
}

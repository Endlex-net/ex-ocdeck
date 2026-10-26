import { describe, expect, it } from 'vitest';
import { detectAgentVersion, shouldShowOpenCodeRestart } from '../pages/agent-version';
import type { ServerStatus } from '../types';

function statusWith(opencodeVersion: string): ServerStatus {
  return {
    opencodeVersion,
    tmuxVersion: '3.4',
    shutdownPolicy: 'persist',
    watchdogState: 'off',
    contractMinVersion: '1.18.14',
    contractBaseline: '1.18.26',
    versionVerified: true,
  };
}

describe('detectAgentVersion', () => {
  it('status 本身为 null/undefined → 未知', () => {
    expect(detectAgentVersion(null)).toBe('未知');
    expect(detectAgentVersion(undefined)).toBe('未知');
  });

  it('opencodeVersion 为空/空白 → 未知', () => {
    expect(detectAgentVersion(statusWith(''))).toBe('未知');
    expect(detectAgentVersion(statusWith('   '))).toBe('未知');
  });

  it('主版本 1 → v1', () => {
    expect(detectAgentVersion(statusWith('1.18.32'))).toBe('v1');
    expect(detectAgentVersion(statusWith('1.18.14'))).toBe('v1');
  });

  it('主版本 2 → v2', () => {
    expect(detectAgentVersion(statusWith('2.0.23'))).toBe('v2');
  });

  it('带 v 前缀的版本串可解析', () => {
    expect(detectAgentVersion(statusWith('v1.18.32'))).toBe('v1');
  });

  it('非数字主版本 → 未知', () => {
    expect(detectAgentVersion(statusWith('dev'))).toBe('未知');
    expect(detectAgentVersion(statusWith('unknown'))).toBe('未知');
  });

  it('其他主版本（3.x/0.x）→ 未知', () => {
    expect(detectAgentVersion(statusWith('3.0.0'))).toBe('未知');
    expect(detectAgentVersion(statusWith('0.9.9'))).toBe('未知');
  });

  it('`opencode ` 可执行名前缀的 probe 输出按空白分词解析', () => {
    expect(detectAgentVersion(statusWith('opencode v2.1.4'))).toBe('v2');
    expect(detectAgentVersion(statusWith('opencode v2.0.28'))).toBe('v2');
    expect(detectAgentVersion(statusWith('opencode v1.18.26'))).toBe('v1');
    expect(detectAgentVersion(statusWith('opencode 2.0.28'))).toBe('v2');
  });

  it('版本段非全数字（Go Atoi 会失败）→ 未知，不继续后续 token', () => {
    expect(detectAgentVersion(statusWith('opencode v2junk.1.4'))).toBe('未知');
  });

  it('优先使用 opencodeMajor，忽略 opencodeVersion 的怪异形态', () => {
    const s: ServerStatus = { ...statusWith('weird'), opencodeMajor: 2 };
    expect(detectAgentVersion(s)).toBe('v2');
  });

  it('opencodeMajor 为 0 / 3 → 未知', () => {
    const s0: ServerStatus = { ...statusWith('1.18.26'), opencodeMajor: 0 };
    expect(detectAgentVersion(s0)).toBe('未知');
    const s3: ServerStatus = { ...statusWith('2.0.28'), opencodeMajor: 3 };
    expect(detectAgentVersion(s3)).toBe('未知');
  });
});

describe('shouldShowOpenCodeRestart', () => {
  it('v2 显示重启按钮', () => {
    expect(shouldShowOpenCodeRestart('v2')).toBe(true);
  });

  it('v1 不显示（v1 无共享 serve 进程）', () => {
    expect(shouldShowOpenCodeRestart('v1')).toBe(false);
  });

  it('未知不显示（保守策略，未确认能力前不暴露破坏性操作）', () => {
    expect(shouldShowOpenCodeRestart('未知')).toBe(false);
  });
});

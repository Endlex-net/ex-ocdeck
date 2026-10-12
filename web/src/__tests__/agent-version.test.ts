import { describe, expect, it } from 'vitest';
import { detectAgentVersion } from '../pages/agent-version';
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
});

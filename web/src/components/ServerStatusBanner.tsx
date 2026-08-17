import { useState } from 'react';
import { api } from '../api';
import { usePoll } from '../hooks';
import type { ServerStatus } from '../types';
import { WarnIcon, InfoIcon } from '../icons';

/**
 * 应用级服务端状态告警 banner：
 * - watchdogState === "degraded" → 高警示（kill_immediate 模式下服务端异常死亡
 *   不再有 watchdog 兜底杀 oc 进程）；
 * - versionVerified === false → 次级警示（opencode 版本未验证 / 超出已验证契约区间）。
 * 进入应用拉取一次 + 30s 低频刷新；拉取失败静默（页面自身错误通道已覆盖）。
 */
export function ServerStatusBanner() {
  const [status, setStatus] = useState<ServerStatus | null>(null);

  usePoll(() => {
    api
      .serverStatus()
      .then(setStatus)
      .catch(() => {
        /* 静默：banner 不覆盖页面级错误提示 */
      });
  }, 30_000);

  if (!status) return null;

  const degraded = status.watchdogState === 'degraded';
  const unverified = status.versionVerified === false;
  if (!degraded && !unverified) return null;

  return (
    <div className="server-banners">
      {degraded && (
        <div className="od-alert od-alert-danger">
          <WarnIcon title="watchdog 降级" />
          <span className="od-alert-body mono">
            watchdog 已降级（degraded）：kill_immediate 模式下服务端异常死亡时将无法兜底清理
            opencode 进程，建议尽快重启 ocdeck-server。
          </span>
        </div>
      )}
      {unverified && (
        <div className="od-alert od-alert-info">
          <InfoIcon title="版本未验证" />
          <span className="od-alert-body mono">
            opencode 版本未验证：当前 {status.opencodeVersion || '未知'}，已验证区间 [{status.contractMinVersion || '未知'}, {status.contractBaseline || '未知'}]，TUI 行为可能与预期不符。
          </span>
        </div>
      )}
    </div>
  );
}

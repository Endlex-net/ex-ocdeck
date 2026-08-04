import { useState } from 'react';
import { api, ApiError } from '../api';
import type { Task } from '../types';

/** Re-run 入口渲染门禁（tasks.md 5.3，与后端门禁一致）：仅 suspended 且 failed|succeeded。 */
export function canRerunInit(task: Task): boolean {
  return (
    task.status === 'suspended' &&
    (task.init_status === 'failed' || task.init_status === 'succeeded')
  );
}

interface RerunInitButtonProps {
  task: Task;
  onDone: () => void;
  onError?: (msg: string) => void;
}

/**
 * Re-run init 按钮：仅在 canRerunInit 满足时渲染（archived+failed 不出现必然 422 的按钮）。
 * 成功返回最新任务 DTO（异步执行已登记，非同步完成），成功后不自动激活。
 */
export function RerunInitButton({ task, onDone, onError }: RerunInitButtonProps) {
  const [busy, setBusy] = useState(false);
  if (!canRerunInit(task)) return null;
  return (
    <button
      className="btn btn-small"
      disabled={busy}
      onClick={(e) => {
        e.stopPropagation();
        if (busy) return;
        setBusy(true);
        void (async () => {
          try {
            await api.rerunInit(task.id);
            onDone();
          } catch (err) {
            const msg =
              err instanceof ApiError
                ? err.status === 409
                  ? `操作冲突（任务忙或状态已变化）：${err.message}`
                  : `[${err.code}] ${err.message}`
                : 'Re-run 失败';
            onError?.(msg);
            onDone(); // 刷新以拿到最新状态
          } finally {
            setBusy(false);
          }
        })();
      }}
    >
      {busy ? '…' : 'Re-run init'}
    </button>
  );
}

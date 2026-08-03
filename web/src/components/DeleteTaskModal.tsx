import { useState } from 'react';
import { api, ApiError } from '../api';
import type { Task } from '../types';

interface DeleteTaskModalProps {
  task: Task;
  onClose: () => void;
  onDeleted: () => void;
}

/**
 * 删除任务二次确认对话框：
 * - 默认 mode=normal；
 * - deletion_failed 时提供强制删除（mode=force）选项；
 * - 首次尝试被 409（如 dirty worktree）拒绝后，展示 dirty 警告并要求勾选确认
 *   → 重新提交时带 confirmDirty=true。
 */
export function DeleteTaskModal({ task, onClose, onDeleted }: DeleteTaskModalProps) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [confirmDirty, setConfirmDirty] = useState(false);
  const [force, setForce] = useState(false);
  const [dirtyRejected, setDirtyRejected] = useState(false);

  const canForce = task.status === 'deletion_failed';

  const submit = async () => {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      await api.deleteTask(task.id, force ? 'force' : 'normal', confirmDirty);
      onDeleted();
    } catch (err) {
      const ae =
        err instanceof ApiError ? err : new ApiError(0, 'unknown', '删除失败');
      setError(ae);
      // 409 冲突（dirty / 分支占用 / 任务忙）→ 展示 dirty 确认勾选项
      if (ae.status === 409) setDirtyRejected(true);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-title">删除任务</div>
        <div className="modal-body">
          <p>
            确认删除任务 <strong>{task.name}</strong>
            {task.branch && (
              <>
                （分支 <code>{task.branch}</code>）
              </>
            )}
            ？该操作会删除对应 worktree，不可恢复。
          </p>

          {canForce && (
            <label className="check-line">
              <input
                type="checkbox"
                checked={force}
                onChange={(e) => setForce(e.target.checked)}
              />
              强制删除（mode=force，跳过 opencode session 清理）
            </label>
          )}

          {dirtyRejected && (
            <div className="warn-box">
              <p>服务端拒绝了删除（存在未提交改动或占用冲突）。</p>
              <label className="check-line">
                <input
                  type="checkbox"
                  checked={confirmDirty}
                  onChange={(e) => setConfirmDirty(e.target.checked)}
                />
                我已了解 worktree 存在未提交改动，确认一并删除（confirmDirty）
              </label>
            </div>
          )}

          {error && (
            <div className="error-line">
              [{error.code}] {error.message}
            </div>
          )}
        </div>
        <div className="modal-actions">
          <button className="btn" onClick={onClose} disabled={busy}>
            取消
          </button>
          <button
            className="btn btn-danger"
            onClick={submit}
            disabled={busy || (dirtyRejected && !confirmDirty)}
          >
            {busy ? '删除中…' : force ? '强制删除' : '删除'}
          </button>
        </div>
      </div>
    </div>
  );
}

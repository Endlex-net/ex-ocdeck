import { useState } from 'react';
import { api, ApiError } from '../api';
import { isTransitional, type Task } from '../types';

type Action = 'activate' | 'suspend' | 'archive' | 'restore' | 'retry';

const ACTION_LABEL: Record<Action, string> = {
  activate: '激活',
  suspend: '挂起',
  archive: '归档',
  restore: '恢复',
  retry: '重试',
};

/** 按状态机给出可用操作（非法操作服务端会 409，前端尽量前置隐藏）。 */
export function actionsFor(status: string): Action[] {
  switch (status) {
    case 'suspended':
      return ['activate', 'archive'];
    case 'active':
      return ['suspend'];
    case 'archived':
      return ['restore'];
    case 'creation_failed':
    case 'deletion_failed':
      return ['retry'];
    default:
      return [];
  }
}

interface TaskActionsProps {
  task: Task;
  onDone: () => void;
  onError?: (msg: string) => void;
}

export function TaskActions({ task, onDone, onError }: TaskActionsProps) {
  const [pending, setPending] = useState<Action | null>(null);
  // deletion_failed 的 retry 被 409（dirty 未确认）拒绝后弹出确认对话框
  const [dirtyPrompt, setDirtyPrompt] = useState(false);
  const [confirmDirty, setConfirmDirty] = useState(false);
  const [dirtyBusy, setDirtyBusy] = useState(false);
  const [dirtyError, setDirtyError] = useState('');
  const disabled = isTransitional(task.status) || pending !== null;

  const run = async (action: Action) => {
    if (disabled) return;
    setPending(action);
    try {
      await api.taskAction(task.id, action);
      onDone();
    } catch (err) {
      // deletion_failed 的 retry 因 worktree dirty 被 409 拒绝 → 弹确认对话框（confirmDirty）
      if (
        action === 'retry' &&
        task.status === 'deletion_failed' &&
        err instanceof ApiError &&
        err.status === 409 &&
        /dirty|confirmDirty/i.test(err.message)
      ) {
        setConfirmDirty(false);
        setDirtyError('');
        setDirtyPrompt(true);
        return;
      }
      const msg =
        err instanceof ApiError
          ? err.status === 409
            ? `操作冲突（任务忙或状态已变化）：${err.message}`
            : `[${err.code}] ${err.message}`
          : '操作失败';
      onError?.(msg);
      onDone(); // 刷新以拿到最新状态
    } finally {
      setPending(null);
    }
  };

  const confirmRetry = async () => {
    if (dirtyBusy || !confirmDirty) return;
    setDirtyBusy(true);
    setDirtyError('');
    try {
      await api.retryTask(task.id, true);
      setDirtyPrompt(false);
      onDone();
    } catch (err) {
      setDirtyError(
        err instanceof ApiError ? `[${err.code}] ${err.message}` : '重试失败',
      );
    } finally {
      setDirtyBusy(false);
    }
  };

  return (
    <span className="task-actions">
      {actionsFor(task.status).map((a) => (
        <button
          key={a}
          className="btn btn-small"
          disabled={disabled}
          onClick={(e) => {
            e.stopPropagation();
            void run(a);
          }}
        >
          {pending === a ? '…' : ACTION_LABEL[a]}
        </button>
      ))}

      {dirtyPrompt && (
        <div
          className="modal-backdrop"
          onClick={(e) => {
            e.stopPropagation();
            setDirtyPrompt(false);
            onDone();
          }}
        >
          <div className="modal" onClick={(e) => e.stopPropagation()}>
            <div className="modal-title">重试删除</div>
            <div className="modal-body">
              <div className="warn-box">
                <p>服务端拒绝了重试（worktree 存在未提交改动）。</p>
                <label className="check-line">
                  <input
                    type="checkbox"
                    checked={confirmDirty}
                    onChange={(e) => setConfirmDirty(e.target.checked)}
                  />
                  我已了解 worktree 存在未提交改动，确认一并删除（confirmDirty）
                </label>
              </div>
              {dirtyError && <div className="error-line">{dirtyError}</div>}
            </div>
            <div className="modal-actions">
              <button
                className="btn"
                disabled={dirtyBusy}
                onClick={() => {
                  setDirtyPrompt(false);
                  onDone();
                }}
              >
                取消
              </button>
              <button
                className="btn btn-danger"
                disabled={dirtyBusy || !confirmDirty}
                onClick={() => void confirmRetry()}
              >
                {dirtyBusy ? '重试中…' : '确认重试'}
              </button>
            </div>
          </div>
        </div>
      )}
    </span>
  );
}

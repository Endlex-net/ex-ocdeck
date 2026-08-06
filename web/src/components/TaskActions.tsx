import { useState } from 'react';
import { api, ApiError } from '../api';
import { initActivateBlockReason, isTransitional, type Task } from '../types';
import { RerunInitButton } from './RerunInitButton';

type Action = 'activate' | 'suspend' | 'archive' | 'restore' | 'retry';

const ACTION_LABEL: Record<Action, string> = {
  activate: '激活',
  suspend: '挂起',
  archive: '归档',
  restore: '恢复',
  retry: '重试',
};

/** 窄屏 header 图标化（design D3）：glyph 只用于 compact，aria/title 仍给完整中文标签 */
const ACTION_ICON: Record<Action, string> = {
  activate: '▶',
  suspend: '⏸',
  archive: '▼',
  restore: '▲',
  retry: '⟳',
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
  /** 窄屏工作台 header：按钮渲染为图标（design D3 主操作图标化），默认 false 保持桌面样式 */
  compact?: boolean;
}

export function TaskActions({ task, onDone, onError, compact = false }: TaskActionsProps) {
  const [pending, setPending] = useState<Action | null>(null);
  // deletion_failed 的 retry 被 409（dirty 未确认）拒绝后弹出确认对话框
  const [dirtyPrompt, setDirtyPrompt] = useState(false);
  const [confirmDirty, setConfirmDirty] = useState(false);
  const [dirtyBusy, setDirtyBusy] = useState(false);
  const [dirtyError, setDirtyError] = useState('');
  const disabled = isTransitional(task.status) || pending !== null;
  // init 非 none|succeeded 时激活入口禁用并提示原因（tasks.md 5.3）
  const activateBlock = initActivateBlockReason(task);

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
          disabled={disabled || (a === 'activate' && activateBlock !== '')}
          title={
            a === 'activate' && activateBlock
              ? activateBlock
              : compact
                ? ACTION_LABEL[a]
                : undefined
          }
          aria-label={compact ? ACTION_LABEL[a] : undefined}
          onClick={(e) => {
            e.stopPropagation();
            void run(a);
          }}
        >
          {pending === a ? '…' : compact ? ACTION_ICON[a] : ACTION_LABEL[a]}
        </button>
      ))}

      <RerunInitButton task={task} onDone={onDone} onError={onError} compact={compact} />

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

import { useCallback, useEffect, useState } from 'react';
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
 * - repo 任务首次尝试被 409（如 dirty worktree）拒绝后，展示 dirty 警告并要求勾选确认
 *   → 重新提交时带 confirmDirty=true；
 * - dir 任务（project_kind=dir，D7）：不涉及 worktree/分支，文案明确不删除项目目录；
 *   normal 模式且项目配置了 pre-delete 脚本时提示脚本仍会执行；不展示 dirty 确认项。
 *   pre-delete 提示 fail-closed：lifecycle-config 三态（loading/success/error），
 *   配置加载完成前禁用确认按钮；加载失败显示错误与重试，不允许提交。
 */
export function DeleteTaskModal({ task, onClose, onDeleted }: DeleteTaskModalProps) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [confirmDirty, setConfirmDirty] = useState(false);
  const [force, setForce] = useState(false);
  const [dirtyRejected, setDirtyRejected] = useState(false);
  // dir 任务：lifecycle-config 三态（fail-closed）——加载完成前/失败时禁止提交删除
  const [lcState, setLcState] = useState<'loading' | 'success' | 'error'>('loading');
  const [preDeleteConfigured, setPreDeleteConfigured] = useState(false);

  const canForce = task.status === 'deletion_failed';
  const isDir = task.project_kind === 'dir';

  const loadLifecycleConfig = useCallback(() => {
    if (!isDir) return;
    setLcState('loading');
    api
      .getLifecycleConfig(task.project_id)
      .then((c) => {
        setPreDeleteConfigured(c.pre_delete_script.trim() !== '');
        setLcState('success');
      })
      .catch(() => setLcState('error'));
  }, [isDir, task.project_id]);

  useEffect(() => {
    loadLifecycleConfig();
  }, [loadLifecycleConfig]);

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
      // 409 冲突（dirty / 分支占用 / 任务忙）→ 展示 dirty 确认勾选项（仅 repo 任务）
      if (ae.status === 409 && !isDir) setDirtyRejected(true);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-title">删除任务</div>
        <div className="modal-body">
          {isDir ? (
            <p>
              确认删除任务 <strong>{task.name}</strong>
              ？仅删除任务记录与 opencode 会话数据，不会删除项目目录及其内容。
            </p>
          ) : (
            <p>
              确认删除任务 <strong>{task.name}</strong>
              {task.branch && (
                <>
                  （分支 <code>{task.branch}</code>）
                </>
              )}
              ？该操作会删除对应 worktree，不可恢复。
            </p>
          )}

          {canForce && (
            <label className="check-line">
              <input
                type="checkbox"
                checked={force}
                onChange={(e) => setForce(e.target.checked)}
              />
              强制删除（mode=force，跳过 opencode session 清理，同时跳过 pre-delete 脚本）
            </label>
          )}

          {isDir && lcState === 'loading' && (
            <div className="warn-box">
              <p>正在确认项目 pre-delete 脚本配置…</p>
            </div>
          )}

          {isDir && lcState === 'error' && (
            <div className="warn-box">
              <p>获取项目配置失败，无法确认 pre-delete 脚本是否执行，请重试。</p>
              <button className="btn btn-small" onClick={loadLifecycleConfig}>
                重试
              </button>
            </div>
          )}

          {isDir && lcState === 'success' && preDeleteConfigured && !force && (
            <div className="warn-box">
              <p>该项目配置了 pre-delete 脚本，删除时仍会在项目目录下执行。</p>
            </div>
          )}

          {!isDir && dirtyRejected && (
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
            disabled={
              busy ||
              (dirtyRejected && !confirmDirty) ||
              // dir 任务 fail-closed：pre-delete 配置未确认前不得提交
              (isDir && lcState !== 'success')
            }
          >
            {busy ? '删除中…' : force ? '强制删除' : '删除'}
          </button>
        </div>
      </div>
    </div>
  );
}

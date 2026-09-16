import { useRef, useState } from 'react';
import { api, ApiError } from '../api';
import { baseRefShortName } from '../pages/workbench-branch';
import { StatusBadge } from './StatusBadge';
import { isGitlessTask, type Task, type TaskInfoPatch } from '../types';

/** 由当前分支名拆出「原前缀 + slug」：前缀 = 最后一个 `/` 之前部分；
 *  分支无 `/` 时整支视为 slug、前缀为空（新分支名即 slug 本身，task-lifecycle spec）。 */
export function splitBranch(branch: string): { prefix: string; slug: string } {
  const idx = branch.lastIndexOf('/');
  if (idx === -1) return { prefix: '', slug: branch };
  return { prefix: branch.slice(0, idx), slug: branch.slice(idx + 1) };
}

/** 预览/提交共用的最终分支名换算：沿用 baseline 分支原前缀。 */
function resolveBranchName(prefix: string, slug: string): string {
  return prefix === '' ? slug : `${prefix}/${slug}`;
}

/**
 * 任务信息卡片（task-info-editable，挂 TaskWorkbenchPage settings pane）：
 * 展示态连续列出 项目/状态/基础分支（gitless 显示路径）/worktree 路径/创建时间/任务名称/分支
 * （gitless 不渲染分支行）；卡片头部单「编辑」按钮进入编辑态——任务名称与分支 slug
 * （仅 worktree 任务）可编辑，slug 实时预览最终分支名（沿用该任务当前分支原前缀）。
 *
 * 保存语义：
 * - 名称与 slug 同一次 PATCH 提交（presence：名称恒携带；slug trim 后非空才携带）；
 * - slug 实际变更时先弹危险确认 modal（复用 DeleteTaskModal shell 与 .btn-danger），
 *   仅名称变化无需确认；两项均未修改为幂等同值写；
 * - 任何 PATCH 错误（无论错误码）统一进入结果不确定恢复门禁（P4-4）：后端两阶段语义下
 *   错误可能发生在历史意图 R1 收敛之后（含 invalid_input/invalid_state/conflict），本地
 *   baseline 可能已失效。保留原始错误展示，先空 PATCH {}（仅 R1 收敛）+ GET 刷新，
 *   以收敛后的当前分支重置基准并重新确认 slug 后才允许再次提交，MUST NOT 自动重发草稿；
 *   恢复失败保持门禁。取消/重新编辑只放弃草稿，MUST NOT 解除该门禁（P4-2）。
 */
export function TaskInfoCard({
  task,
  projectName,
  onSaved,
  onRefreshed,
}: {
  task: Task;
  projectName: string;
  /** 保存成功：updated 为服务端返回的最新 DTO（调用方同步任务态并刷新共享 store）。 */
  onSaved: (updated: Task) => void;
  /** 不确定结果后的强制刷新结果回传（调用方同步任务态，不改变编辑态）。 */
  onRefreshed?: (fresh: Task) => void;
}) {
  // gitless（dir / repo local-path）：无分支概念，分支行与 slug 输入均不渲染
  const gitless = isGitlessTask(task.project_kind, task.mode);

  const [editing, setEditing] = useState(false);
  // 编辑基准：进入编辑态时捕获；不确定结果刷新成功后更新为最新任务值——
  // slug 变更判定与危险确认一律以 baseline 为准，MUST NOT 直接信任编辑起始值
  const [baseline, setBaseline] = useState({ name: '', branch: '' });
  const [draftName, setDraftName] = useState('');
  const [draftSlug, setDraftSlug] = useState('');
  const [busy, setBusy] = useState(false);
  // 最近一次保存的服务端原始拒绝（[code] message）：与恢复状态分离展示（P4-4）
  const [saveError, setSaveError] = useState('');
  const [error, setError] = useState('');
  const [confirmOpen, setConfirmOpen] = useState(false);
  // 结果不确定门禁：true 期间禁止再次保存，必须先刷新成功
  const [uncertain, setUncertain] = useState(false);
  const [refreshing, setRefreshing] = useState(false);

  const baseParts = splitBranch(baseline.branch);
  const trimmedSlug = draftSlug.trim();
  // slug 实际变更：trim 后非空且换算出的新分支名 ≠ 当前分支（同值跳过，与后端语义对齐）
  const newBranch = resolveBranchName(baseParts.prefix, trimmedSlug);
  const slugChanged = !gitless && trimmedSlug !== '' && newBranch !== baseline.branch;

  const startEdit = () => {
    setBaseline({ name: task.name, branch: task.branch });
    setDraftName(task.name);
    setDraftSlug(gitless ? '' : splitBranch(task.branch).slug);
    // 门禁与草稿分离（P4-2）：再次编辑 MUST NOT 解除结果不确定门禁——
    // 恢复成功取得可信新基准前，保存保持禁用
    if (!uncertain) {
      setError('');
      setSaveError('');
    }
    setConfirmOpen(false);
    setEditing(true);
  };

  const cancel = () => {
    setEditing(false);
    setConfirmOpen(false);
    // 取消仅放弃草稿；结果不确定门禁（uncertain）不受取消影响（P4-2）
    if (!uncertain) {
      setError('');
      setSaveError('');
    }
  };

  // 恢复代际：取消→重新编辑→再次恢复时，旧恢复的回写被丢弃，避免与新一轮竞态（P4-2）
  const recoverGenRef = useRef(0);

  /**
   * 结果不确定后的恢复（P4-1）：先空 PATCH {}（后端空 body 幂等路径，仅执行 R1 收敛——
   * 若上次保存已留下未收敛恢复意图，在此补齐/清除），再 GET 读取收敛后的最新任务并重置基准；
   * 两步全部成功才解禁保存，任一步失败保持门禁、禁止携带旧草稿重试。
   */
  const recoverAfterUncertainty = async () => {
    const gen = ++recoverGenRef.current;
    setRefreshing(true);
    try {
      await api.updateTask(task.id, {});
      const fresh = await api.getTask(task.id);
      if (gen !== recoverGenRef.current) return;
      onRefreshed?.(fresh);
      setBaseline({ name: fresh.name, branch: fresh.branch });
      setUncertain(false);
      setError('保存结果不确定，已恢复任务状态。请核对当前名称与分支，重新确认后再保存。');
    } catch {
      if (gen !== recoverGenRef.current) return;
      setError('保存结果不确定，且恢复失败。请点击「重试恢复」后再保存。');
    } finally {
      if (gen === recoverGenRef.current) setRefreshing(false);
    }
  };

  const submit = async () => {
    if (busy || uncertain || refreshing) return;
    const patch: TaskInfoPatch = { name: draftName };
    if (!gitless && trimmedSlug !== '') patch.branch_slug = trimmedSlug;
    setBusy(true);
    setError('');
    setSaveError('');
    try {
      const updated = await api.updateTask(task.id, patch);
      setConfirmOpen(false);
      setEditing(false);
      onSaved(updated);
    } catch (err) {
      setConfirmOpen(false);
      // 所有 PATCH 错误（无论错误码）统一进入结果不确定恢复门禁（P4-4）：
      // 后端两阶段语义下，任何错误都可能发生在历史意图 R1 收敛之后——R1 收敛失败返回
      // conflict 时意图仍在；R1 收敛成功后再返回的 invalid_input/invalid_state/conflict，
      // 意味着历史恢复可能已改变分支，本地 baseline 已失效，按旧基准确认重试会产生嵌套 slug。
      // 因此不做错误码区分：保留原始错误展示 + 空 PATCH {} 收敛 + GET 刷新后才允许重新确认。
      setSaveError(
        err instanceof ApiError ? `[${err.code}] ${err.message}` : '保存失败（网络异常）',
      );
      setUncertain(true);
      setError('保存结果不确定，正在恢复任务状态…');
      await recoverAfterUncertainty();
    } finally {
      setBusy(false);
    }
  };

  const onSaveClick = () => {
    if (busy || uncertain || refreshing) return;
    if (slugChanged) {
      setConfirmOpen(true);
      return;
    }
    void submit();
  };

  const createdAt = task.created_at
    ? new Date(task.created_at * 1000).toLocaleString()
    : '';
  const baseShort = baseRefShortName(task.base_ref ?? '');

  return (
    <div className="settings-section task-info-card">
      <div className="settings-title task-info-head">
        <span>任务信息</span>
        {!editing && (
          <button className="btn btn-small" onClick={startEdit}>
            编辑
          </button>
        )}
      </div>

      <div className="task-info-rows">
        <div className="task-info-row">
          <span className="task-info-label">项目</span>
          <span className="task-info-value">{projectName || task.project_id}</span>
        </div>
        <div className="task-info-row">
          <span className="task-info-label">状态</span>
          <span className="task-info-value">
            <StatusBadge status={task.status} />
          </span>
        </div>
        <div className="task-info-row">
          <span className="task-info-label">{gitless ? '项目路径' : '基础分支'}</span>
          <span className="task-info-value mono">
            {gitless ? task.worktree_path : baseShort || '（无）'}
          </span>
        </div>
        <div className="task-info-row">
          <span className="task-info-label">worktree 路径</span>
          <span className="task-info-value mono">{task.worktree_path}</span>
        </div>
        <div className="task-info-row">
          <span className="task-info-label">创建时间</span>
          <span className="task-info-value">{createdAt}</span>
        </div>
        <div className="task-info-row">
          <span className="task-info-label">任务名称</span>
          {editing ? (
            <span className="task-info-value">
              <input
                className="od-input"
                id="task-info-name"
                value={draftName}
                onChange={(e) => setDraftName(e.target.value)}
                disabled={busy}
              />
            </span>
          ) : (
            <span className="task-info-value">{task.name}</span>
          )}
        </div>
        {!gitless && (
          <div className="task-info-row">
            <span className="task-info-label">分支</span>
            {editing ? (
              <span className="task-info-value">
                <input
                  className="od-input mono"
                  id="task-info-branch-slug"
                  spellCheck={false}
                  value={draftSlug}
                  onChange={(e) => setDraftSlug(e.target.value)}
                  disabled={busy}
                />
                <span className="task-info-preview mono">
                  {trimmedSlug === ''
                    ? '留空则不修改分支'
                    : `最终分支名：${newBranch}`}
                </span>
              </span>
            ) : (
              <span className="task-info-value mono">{task.branch}</span>
            )}
          </div>
        )}
      </div>

      {editing && (
        <>
          {saveError && <div className="error-line task-info-error">{saveError}</div>}
          {error && (
            <div className="error-line task-info-error">
              {error}{' '}
              {uncertain && (
                <button
                  className="btn btn-small"
                  disabled={refreshing}
                  onClick={() => void recoverAfterUncertainty()}
                >
                  {refreshing ? '恢复中…' : '重试恢复'}
                </button>
              )}
            </div>
          )}
          <div className="task-info-actions">
            <button className="btn btn-small" onClick={cancel} disabled={busy}>
              取消
            </button>
            <button
              className="btn btn-primary btn-small"
              onClick={onSaveClick}
              disabled={busy || uncertain || refreshing}
            >
              {busy ? '保存中…' : '保存'}
            </button>
          </div>
        </>
      )}

      {/* 分支改名危险确认（复用 DeleteTaskModal modal shell 与 .btn-danger 模式）：
          仅 slug 实际变更时弹出；取消确认留在编辑态 */}
      {confirmOpen && (
        <div className="modal-backdrop" onClick={() => !busy && setConfirmOpen(false)}>
          <div className="modal" onClick={(e) => e.stopPropagation()}>
            <div className="modal-title">确认修改分支名</div>
            <div className="modal-body">
              <p>
                分支 <code>{baseline.branch}</code> 将改名为 <code>{newBranch}</code>
                。旧分支名将废弃，worktree 路径与提交内容不变。
              </p>
            </div>
            <div className="modal-actions">
              <button className="btn" onClick={() => setConfirmOpen(false)} disabled={busy}>
                取消
              </button>
              <button className="btn btn-danger" onClick={() => void submit()} disabled={busy}>
                {busy ? '保存中…' : '确认修改'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

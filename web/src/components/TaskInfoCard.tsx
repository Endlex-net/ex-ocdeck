import { useRef, useState } from 'react';
import { api, ApiError } from '../api';
import { baseRefShortName } from '../pages/workbench-branch';
import { StatusBadge } from './StatusBadge';
import {
  isGitlessTask,
  PERMISSION_MODE_LABELS,
  type Task,
  type TaskDetail,
  type TaskInfoPatch,
  type TaskPermissionMode,
  type TaskPermissionModeView,
} from '../types';

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
 *
 * 权限模式（查看态/编辑态与名称/分支一致）：查看态只读展示已保存模式，无选择器；
 * 点「编辑」进入编辑态才出现选择器（当前已保存值为初始值），「取消」还原未保存选择，
 * 「保存」时仅当模式值变更才调专用模式端点（MUST NOT 并入通用 PATCH；同次编辑中
 * 名称/分支通用 PATCH 与模式端点各自提交——先通用 PATCH 后模式端点——各自保留既有
 * 错误/结果不确定门禁）：保存期间禁用选择器、禁止重叠请求、序列令牌旧响应不覆盖；
 * 失败（网络/结果不确定）先 GET 复验服务端当前状态后才解禁，复验失败「重试复验」。
 * 已保存模式与生效模式不一致时常驻提示「新模式将在下次激活生效」（查看态/编辑态都展示）；
 * 保存成功且无法立即生效时另给瞬时确认「已保存，将在下次激活后生效。」（下一次模式
 * 保存开始或两值一致后消失）。
 */
export function TaskInfoCard({
  task,
  projectName,
  onSaved,
  onPermissionModeSaved,
  onRefreshed,
}: {
  task: TaskDetail;
  projectName: string;
  /** 保存成功：updated 为服务端返回的最新 DTO（调用方同步任务态并刷新共享 store）。
   *  通用 PATCH 响应不含 effective_permission_mode，调用方 MUST 合并保留（D8）。 */
  onSaved: (updated: Task) => void;
  /** 权限模式保存成功：仅两模式字段视图（调用方合并进页面任务，D8）。 */
  onPermissionModeSaved: (view: TaskPermissionModeView) => void;
  /** 不确定结果后的强制刷新结果回传（调用方同步任务态，不改变编辑态）。 */
  onRefreshed?: (fresh: TaskDetail) => void;
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
    setDraftMode(task.permission_mode); // 选择器以当前已保存值为初始值
    setModeBaseline(task.permission_mode);
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

  // —— 权限模式随编辑态「保存」提交（专用模式端点，MUST NOT 并入通用 PATCH）。
  // draftMode 为编辑态草稿（进编辑态时以当前已保存值初始化，取消即放弃）；模式端点
  // 竞态门禁沿用既有保存竞态约束：modeSaving 覆盖「保存+复验」全程（期间选择器禁用、
  // 保存按钮 busy 禁止重叠请求）；新保存流即刻作废任何在途保存/复验流（序列令牌）：
  // 旧响应/旧复验 MUST NOT 覆盖更新状态。失败按结果不确定处理：先 GET 复验服务端
  // 当前状态再解禁（服务端可能在失败响应前已提交，MUST NOT 直接放行重发）。
  const [draftMode, setDraftMode] = useState<TaskPermissionMode>('ask');
  // 模式编辑基准（I5）：进入编辑态时捕获的已保存模式——「用户是否改了模式」一律以
  // draftMode 与 modeBaseline 比较判定；编辑期间 SSE/复验更新 task.permission_mode
  // MUST NOT 改变未修改草稿的提交语义（用户未动选择器则不产生模式端点调用）
  const [modeBaseline, setModeBaseline] = useState<TaskPermissionMode>('ask');
  const [modeSaving, setModeSaving] = useState(false);
  const [modeRechecking, setModeRechecking] = useState(false);
  const [modeUncertain, setModeUncertain] = useState(false);
  const [modeError, setModeError] = useState('');
  // 保存后瞬时确认（无法立即生效场景）：保存成功且响应两值不一致时展示，
  // 下一次保存开始或两值转为一致（下次激活）时消失；常驻提示独立保留。
  const [modeNotice, setModeNotice] = useState('');
  const modeSaveSeqRef = useRef(0);

  /** 结果不确定后的复验（与名称/分支恢复门禁同型）：GET 详情以服务端当前两模式字段收敛，
   *  收敛成功才解禁选择器；失败保持门禁，由「重试复验」再收敛。 */
  const recheckMode = async (seq: number) => {
    setModeRechecking(true);
    try {
      const fresh = await api.getTask(task.id);
      if (seq !== modeSaveSeqRef.current) return;
      onRefreshed?.(fresh);
      setModeUncertain(false);
      setModeError('权限模式保存结果不确定，已按服务端当前状态收敛。');
      setModeSaving(false);
    } catch {
      if (seq !== modeSaveSeqRef.current) return;
      setModeError('权限模式保存结果不确定，且复验失败。请点击「重试复验」后再修改。');
    } finally {
      if (seq === modeSaveSeqRef.current) setModeRechecking(false);
    }
  };

  const saveMode = async (next: TaskPermissionMode) => {
    const seq = ++modeSaveSeqRef.current; // 超越在途流：旧响应 MUST NOT 覆盖更新状态
    setModeSaving(true);
    setModeError('');
    setModeUncertain(false);
    setModeNotice('');
    try {
      const view = await api.updateTaskPermissionMode(task.id, next);
      if (seq !== modeSaveSeqRef.current) return; // 旧响应不覆盖
      onPermissionModeSaved(view);
      if (view.permission_mode !== view.effective_permission_mode) {
        setModeNotice('已保存，将在下次激活后生效。');
      }
      setModeSaving(false);
    } catch {
      if (seq !== modeSaveSeqRef.current) return;
      setModeUncertain(true);
      setModeError('权限模式保存结果不确定，正在复验服务端状态…');
      await recheckMode(seq);
    }
  };

  const retryModeRecheck = async () => {
    if (!modeUncertain || modeRechecking) return;
    const seq = ++modeSaveSeqRef.current;
    setModeError('权限模式保存结果不确定，正在复验服务端状态…');
    await recheckMode(seq);
  };

  const submit = async () => {
    if (busy || uncertain || refreshing) return;
    const patch: TaskInfoPatch = { name: draftName };
    if (!gitless && trimmedSlug !== '') patch.branch_slug = trimmedSlug;
    // 模式草稿相对「编辑开始时的基准」实际变更且模式门禁未激活时，才随本次保存调专用
    // 模式端点（I5：与实时 task.permission_mode 比较会被编辑期间 SSE 更新污染；模式值
    // 未变不调；modeUncertain 期间 MUST NOT 重发模式请求，先复验收敛）
    const modeChanged = !modeUncertain && draftMode !== modeBaseline;
    setBusy(true);
    setError('');
    setSaveError('');
    // I6：瞬时确认随「下一次保存开始」消失（不限于模式保存——仅改名称/分支的保存也清除）
    setModeNotice('');
    try {
      const updated = await api.updateTask(task.id, patch);
      setConfirmOpen(false);
      setEditing(false);
      onSaved(updated);
      // 通用 PATCH 成功后再提交模式端点（两路径各自保留错误/结果不确定门禁）；
      // 模式保存失败时已退出编辑态，错误与「重试复验」在展示态常驻错误行可见
      if (modeChanged) await saveMode(draftMode);
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
          // I4：模式保存/复验进行中禁止重新进入编辑态（与选择器/保存按钮同一 busy
          // 覆盖）——否则 draftMode 会按旧 task.permission_mode 初始化，专用请求收敛后
          // 草稿不同步，再保存可能把刚保存的模式改回旧值
          <button
            className="btn btn-small"
            onClick={startEdit}
            disabled={busy || modeSaving || modeRechecking || modeUncertain}
          >
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
        <div className="task-info-row">
          <span className="task-info-label">权限模式</span>
          <span className="task-info-value">
            {/* 查看态只读展示已保存模式；编辑态才出现选择器（草稿 draftMode，
                随「保存」走专用模式端点提交，「取消」还原）；结果不确定门禁期间禁用 */}
            {editing ? (
              <select
                className="od-input"
                id="task-info-permission-mode"
                value={draftMode}
                disabled={busy || modeSaving || modeRechecking || modeUncertain}
                onChange={(e) => setDraftMode(e.target.value as TaskPermissionMode)}
              >
                {(['ask', 'all-approve', 'ai-auto'] as const).map((m) => (
                  <option key={m} value={m}>
                    {PERMISSION_MODE_LABELS[m]}
                  </option>
                ))}
              </select>
            ) : (
              PERMISSION_MODE_LABELS[task.permission_mode]
            )}
            {/* 常驻提示（非 toast，D6）：已保存模式与运行进程实际生效模式不一致时展示，
                查看态与编辑态均展示 */}
            {task.permission_mode !== task.effective_permission_mode && (
              <span className="task-info-preview">
                当前运行进程仍按「{PERMISSION_MODE_LABELS[task.effective_permission_mode]}
                」处理，新模式将在下次激活生效
              </span>
            )}
            {/* 保存后瞬时确认（D6）：保存成功且无法立即生效时展示，两值一致后消失 */}
            {modeNotice && task.permission_mode !== task.effective_permission_mode && (
              <span className="task-info-preview">{modeNotice}</span>
            )}
          </span>
        </div>
      </div>

      {/* 权限模式保存/复验错误：独立于名称分支编辑态，展示态也可见 */}
      {modeError && (
        <div className="error-line task-info-error">
          {modeError}{' '}
          {modeUncertain && (
            <button
              className="btn btn-small"
              disabled={modeRechecking}
              onClick={() => void retryModeRecheck()}
            >
              {modeRechecking ? '复验中…' : '重试复验'}
            </button>
          )}
        </div>
      )}

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

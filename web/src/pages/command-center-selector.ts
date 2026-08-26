/**
 * 指挥中心「需要关注」推导 selector（design.md D7 + spec.md command-center）。
 *
 * 全部为纯函数：无 React、无 IO，可单测驱动。输入为两个快照（projects 摘要树 +
 * tasks/active 列表），输出为三分区任务视图与「需要关注」注意力项。
 *
 * 双快照 join 规则（字段级来源，spec.md「双快照 join 规则」）：
 * - projects 快照为准：status/init_status/branch/worktree_path/last_error/notice/updated_at
 *   及身份字段（name/project_id/project_name）。
 * - tasks/active 快照为准：last_active_at/agentStatus/attention。
 * - projects 摘要的 agentStatus 仅在该任务缺席 tasks/active 时使用（projects-only 活跃任务）。
 * - 不存在两端同字段优先级冲突；MUST NOT 在请求内做合并修复（下一轮轮询自然收敛）。
 */

import {
  FAILED_STATUS,
  isTransitional,
  parseNotice,
  type ActiveSessionItem,
  type Attention,
  type NoticeItem,
  type PermissionSignal,
  type Project,
  type QuestionSignal,
  type TaskSummary,
} from '../types';

/** 注意力项类别（按 D7 优先级降序）。
 *  数字越小优先级越高（1 = 失败态，最高）。
 *  用普通 enum 而非 const enum（isolatedModules + 跨文件 re-export 兼容）。 */
export enum AttentionKind {
  Failed = 1, // creation_failed / deletion_failed
  InitFailed = 2, // suspended + init_status=failed
  PermissionPending = 3, // attention.permissions 非空
  QuestionPending = 4, // attention.questions 非空
  Notice = 5, // 携带 notice
  AgentIdle = 6, // agentStatus=idle 的活跃任务
}

/** 合并后的任务视图（字段级来源 join 结果）。 */
export interface MergedTask {
  task: TaskSummary;
  /** 来自 projects 快照的 identity（含 project_name）。 */
  project_id: string;
  project_name: string;
  /** 项目类型（来自 projects 快照；sessions-only 任务缺省 repo）。 */
  project_kind: 'repo' | 'dir';
  /** 活跃时间：tasks/active 提供 last_active_at，缺席时回退 updated_at。 */
  last_active_at: number;
  /** agent 状态：tasks/active 为准，projects-only 活跃任务回退 summary.agentStatus。 */
  agentStatus?: string;
  /** 注意力：tasks/active 为准；projects-only 活跃任务为空。 */
  attention: Attention;
  /** 是否仅存在于 projects 快照（无对应 tasks/active 行）。 */
  projectsOnly: boolean;
}

/** 「需要关注」单任务去重后的注意力项。
 *  一个任务只出现一行：主类别为最高优先级命中，secondary 为其余命中标记。 */
export interface AttentionItem {
  task: TaskSummary;
  project_id: string;
  project_name: string;
  /** 项目类型（来自 projects 快照；sessions-only 缺省 repo）。 */
  project_kind: 'repo' | 'dir';
  last_active_at: number;
  /** 主呈现类别（最高优先级命中）。 */
  kind: AttentionKind;
  /** 其余命中类别（已按优先级降序）。 */
  secondary: AttentionKind[];
  /** 该任务所有命中类别集合（含主类别）。 */
  kinds: Set<AttentionKind>;
  /** 行内 last_error 文案（失败态展示）。 */
  last_error?: string;
}

/** 三分区视图。 */
export interface CommandCenterView {
  attention: AttentionItem[];
  active: MergedTask[];
  parked: MergedTask[];
}

const EMPTY_ATTENTION: Attention = { permissions: [], questions: [] };

const ACTIVE_STATUSES = new Set(['active', 'creation_failed', 'deletion_failed']);
const TRANSITIONAL_BUT_ACTIVE = new Set(['creating', 'activating', 'suspending', 'deleting']);
const PARKED_STATUSES = new Set(['suspended', 'archived']);

/** 构造 tasks/active 索引：task_id → ActiveSessionItem。 */
export function indexSessions(sessions: ActiveSessionItem[]): Map<string, ActiveSessionItem> {
  const m = new Map<string, ActiveSessionItem>();
  for (const s of sessions) m.set(s.task_id, s);
  return m;
}

/** 从 projects 快照展开为 (project, task) 对的全部列表。 */
export function flattenProjects(
  projects: Project[],
): Array<{ task: TaskSummary; project_id: string; project_name: string; project_kind: 'repo' | 'dir' }> {
  const out: Array<{ task: TaskSummary; project_id: string; project_name: string; project_kind: 'repo' | 'dir' }> = [];
  for (const p of projects) {
    for (const t of p.tasks ?? []) {
      out.push({ task: t, project_id: p.id, project_name: p.name, project_kind: p.kind });
    }
  }
  return out;
}

/** 双快照 join：按字段级来源合并单个任务（spec.md join 规则）。
 *  身份字段（project_id/project_name）+ 状态字段以 projects 快照为准；
 *  last_active_at/agentStatus/attention 以 tasks/active 快照为准。
 *  仅存在于 projects 快照：按 projects 状态归入分区，活跃任务可推导 1/2/5/6 类。
 *  仅存在于 tasks/active 快照：归「其余活跃任务」，可推导 3/4/6 类。 */
export function mergeTask(
  entry: { task: TaskSummary; project_id: string; project_name: string; project_kind?: 'repo' | 'dir' },
  session: ActiveSessionItem | undefined,
): MergedTask {
  const task = entry.task;
  const project_kind = entry.project_kind ?? 'repo';
  if (session) {
    return {
      task,
      // 身份字段以 projects 快照为准（spec.md：身份+状态字段以 projects 为准）
      project_id: entry.project_id,
      project_name: entry.project_name,
      project_kind,
      // last_active_at/agentStatus/attention 以 tasks/active 为准
      last_active_at: session.last_active_at,
      agentStatus: session.agentStatus,
      attention: session.attention ?? EMPTY_ATTENTION,
      projectsOnly: false,
    };
  }
  // projects-only：last_active_at 回退 updated_at；attention 为空集合
  return {
    task,
    project_id: entry.project_id,
    project_name: entry.project_name,
    project_kind,
    last_active_at: task.updated_at,
    agentStatus: task.agentStatus,
    attention: EMPTY_ATTENTION,
    projectsOnly: true,
  };
}

/** 构造仅存在于 tasks/active 的任务视图（无 projects 摘要）。
 *  project_kind 缺省 repo（无 projects 快照信息；dir 降级文案以 projects 快照为准，sessions-only 不涉及）。 */
export function sessionOnlyTask(s: ActiveSessionItem): MergedTask {
  return {
    // 构造最小 TaskSummary（身份 + 状态字段缺失，按 sessions-only 归活跃分区）
    task: {
      id: s.task_id,
      name: s.name,
      status: 'active',
      init_status: '',
      branch: s.branch,
      worktree_path: s.worktree_path,
      updated_at: s.last_active_at,
      agentStatus: s.agentStatus,
      attention_count: 0,
    },
    project_id: s.project_id,
    project_name: s.project_name,
    project_kind: 'repo',
    last_active_at: s.last_active_at,
    agentStatus: s.agentStatus,
    attention: s.attention ?? EMPTY_ATTENTION,
    projectsOnly: false,
  };
}

/** 推导单个任务的全部注意力命中类别（按优先级降序）。
 *  capabilityUnsupported 为 true 时跳过 3/4 类（unsupported 仅缺这两类）。
 *  degraded 时照常推导（数据可能滞后，不标注）。 */
export function classifyAttention(
  m: MergedTask,
  capabilityUnsupported = false,
): AttentionKind[] {
  const kinds: AttentionKind[] = [];
  const status = m.task.status;

  // 1. 失败态（失败态总是进需要关注，即便处于过渡态——design.md D7「失败态除外」）
  if (FAILED_STATUS.has(status)) kinds.push(AttentionKind.Failed);
  // 2. init 失败（suspended + init_status=failed）
  if (status === 'suspended' && m.task.init_status === 'failed') {
    kinds.push(AttentionKind.InitFailed);
  }
  // 3. 等待权限确认（attention.permissions 非空）—— unsupported 时跳过
  if (!capabilityUnsupported && m.attention.permissions.length > 0) {
    kinds.push(AttentionKind.PermissionPending);
  }
  // 4. 等待回答问题（attention.questions 非空）—— unsupported 时跳过
  if (!capabilityUnsupported && m.attention.questions.length > 0) {
    kinds.push(AttentionKind.QuestionPending);
  }
  // 5. notice 残留
  if (parseNotice(m.task.notice).length > 0) kinds.push(AttentionKind.Notice);
  // 6. agent idle 的活跃任务（仅活跃任务，非活跃不归类）
  if (status === 'active' && m.agentStatus === 'idle') {
    kinds.push(AttentionKind.AgentIdle);
  }

  return kinds; // 已按优先级降序（push 顺序即类别优先级顺序）
}

/** 过渡态任务排除在「需要关注」之外（失败态除外）。
 *  spec.md：过渡态归「其余活跃任务」，失败态总是进需要关注。 */
function isAttentionEligible(m: MergedTask): boolean {
  if (FAILED_STATUS.has(m.task.status)) return true;
  return !isTransitional(m.task.status);
}

/** 「需要关注」分区推导 + 单任务去重 + 同类排序。
 *  capabilityUnsupported：unsupported 仅缺 3/4 类；degraded 照常呈现。 */
export function selectAttention(
  merged: MergedTask[],
  capabilityUnsupported = false,
): AttentionItem[] {
  const items: AttentionItem[] = [];
  for (const m of merged) {
    if (!isAttentionEligible(m)) continue;
    const kinds = classifyAttention(m, capabilityUnsupported);
    if (kinds.length === 0) continue;
    const [primary, ...secondary] = kinds;
    items.push({
      task: m.task,
      project_id: m.project_id,
      project_name: m.project_name,
      project_kind: m.project_kind,
      last_active_at: m.last_active_at,
      kind: primary,
      secondary,
      kinds: new Set(kinds),
      last_error: m.task.last_error,
    });
  }
  // 排序：类别优先级升序（kind 数字小优先）→ 同类按 last_active_at 倒序 → ID 升序 tie-break
  items.sort((a, b) => {
    if (a.kind !== b.kind) return a.kind - b.kind;
    if (a.last_active_at !== b.last_active_at) return b.last_active_at - a.last_active_at;
    return a.task.id < b.task.id ? -1 : a.task.id > b.task.id ? 1 : 0;
  });
  return items;
}

/** 「其余活跃任务」分区：active + 过渡态 + 失败态（失败态也命中需要关注，但归活跃分区；
 *  spec.md：过渡态归此区并呈现过渡徽章；归档不在此区）。
 *  MUST 排除已进入「需要关注」的任务 ID（避免同一任务在需要关注与活跃区重复呈现）。
 *  排序：last_active_at 倒序（projects-only 回退 updated_at），时间相同 ID 升序 tie-break。 */
export function selectActive(merged: MergedTask[], excludeIds?: Set<string>): MergedTask[] {
  const out = merged.filter((m) => {
    if (excludeIds?.has(m.task.id)) return false;
    return ACTIVE_STATUSES.has(m.task.status) || TRANSITIONAL_BUT_ACTIVE.has(m.task.status);
  });
  out.sort((a, b) => {
    if (a.last_active_at !== b.last_active_at) return b.last_active_at - a.last_active_at;
    return a.task.id < b.task.id ? -1 : a.task.id > b.task.id ? 1 : 0;
  });
  return out;
}

/** 「挂起与归档」分区：suspended + archived。
 *  排序（用户决策：挂起优先）：suspended 全部排在 archived 之前；
 *  各组内 updated_at 倒序，时间相同 ID 升序 tie-break（稳定）。
 *  注意：挂起与归档使用 projects 快照的 updated_at（tasks/active 不覆盖非活跃任务）。 */
export function selectParked(merged: MergedTask[]): MergedTask[] {
  const out = merged.filter((m) => PARKED_STATUSES.has(m.task.status));
  out.sort((a, b) => {
    // 挂起优先于归档
    const ra = a.task.status === 'suspended' ? 0 : 1;
    const rb = b.task.status === 'suspended' ? 0 : 1;
    if (ra !== rb) return ra - rb;
    const ta = a.task.updated_at;
    const tb = b.task.updated_at;
    if (ta !== tb) return tb - ta;
    return a.task.id < b.task.id ? -1 : a.task.id > b.task.id ? 1 : 0;
  });
  return out;
}

/** 三分区主入口：从双快照推导完整视图。
 *  capabilityUnsupported：attention 能力 unsupported（仅缺 3/4 类）；degraded 照常（默认 false）。
 *  会包含 projects-only 与 sessions-only 任务，各按各自快照归入分区。 */
export function buildCommandCenterView(
  projects: Project[],
  sessions: ActiveSessionItem[],
  capabilityUnsupported = false,
): CommandCenterView {
  const sessionIdx = indexSessions(sessions);
  const flat = flattenProjects(projects);

  // 合并 projects 快照任务
  const mergedFromProjects = flat.map((e) => mergeTask(e, sessionIdx.get(e.task.id)));

  // 补充仅存在于 tasks/active 的任务（无对应 projects 摘要）
  const seenIds = new Set(flat.map((e) => e.task.id));
  const sessionOnly = sessions.filter((s) => !seenIds.has(s.task_id)).map(sessionOnlyTask);

  const all = [...mergedFromProjects, ...sessionOnly];

  const attention = selectAttention(all, capabilityUnsupported);
  // 已进入「需要关注」的任务 ID 不重复出现在「其余活跃任务」区
  const attentionIds = new Set(attention.map((i) => i.task.id));

  return {
    attention,
    active: selectActive(all, attentionIds),
    parked: selectParked(all),
  };
}

/** 注意力类别展示标签。 */
export function attentionKindLabel(kind: AttentionKind): string {
  switch (kind) {
    case AttentionKind.Failed:
      return '失败';
    case AttentionKind.InitFailed:
      return 'init 失败';
    case AttentionKind.PermissionPending:
      return '等待权限确认';
    case AttentionKind.QuestionPending:
      return '等待回答问题';
    case AttentionKind.Notice:
      return 'notice';
    case AttentionKind.AgentIdle:
      return '空闲';
  }
}

/** 判断 deletion_failed 是否提供 pre-delete 日志入口（last_error 以 pre-delete: 前缀）。
 *  与 TaskWorkbenchPage.tsx 现状一致。 */
export function hasPreDeleteLog(last_error?: string): boolean {
  return !!last_error && last_error.startsWith('pre-delete:');
}

/** 列出某任务的 pending 权限请求（用于次要标记/跳转展示）。 */
export function pendingPermissions(att: Attention): PermissionSignal[] {
  return att.permissions;
}

/** 列出某任务的 pending 问题请求。 */
export function pendingQuestions(att: Attention): QuestionSignal[] {
  return att.questions;
}

/** notice 列表（parseNotice 包装，便于页面与测试统一形状）。 */
export function noticeItems(notice: TaskSummary['notice']): NoticeItem[] {
  return parseNotice(notice);
}
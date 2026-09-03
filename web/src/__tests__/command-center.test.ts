import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import {
  AttentionKind,
  buildCommandCenterView,
  classifyAttention,
  flattenProjects,
  indexSessions,
  mergeTask,
  selectActive,
  selectAttention,
  selectParked,
  sessionOnlyTask,
  hasPreDeleteLog,
  attentionKindLabel,
  noticeItems,
} from '../pages/command-center-selector';
import {
  shouldAcceptBranchResult,
  shouldClearRefreshing,
  resolveSessionsBootstrap,
  shouldShowSectionEmpty,
  sectionBodyMode,
  resolveNewTaskInit,
} from '../pages/CommandCenterPage';
import type { ActiveSessionItem, Project, TaskSummary } from '../types';

// ---------------- fixtures ----------------

function task(over: Partial<TaskSummary> & { id: string }): TaskSummary {
  return {
    name: over.name ?? 't-' + over.id,
    status: over.status ?? 'active',
    init_status: over.init_status ?? '',
    branch: over.branch ?? 'main',
    worktree_path: over.worktree_path ?? '/wt',
    last_error: over.last_error,
    notice: over.notice,
    updated_at: over.updated_at ?? 1000,
    agentStatus: over.agentStatus,
    attention_count: over.attention_count ?? 0,
    id: over.id,
  };
}

function project(id: string, tasks: TaskSummary[], name = id): Project {
  return {
    id,
    name,
    path: '/p/' + id,
    kind: 'repo',
    default_branch: 'main',
    created_at: 1,
    task_count: tasks.length,
    tasks_by_status: {},
    tasks,
  };
}

function session(over: Partial<ActiveSessionItem> & { task_id: string }): ActiveSessionItem {
  return {
    project_id: over.project_id ?? 'p1',
    project_name: over.project_name ?? 'proj',
    name: over.name ?? 't-' + over.task_id,
    branch: over.branch ?? 'main',
    worktree_path: over.worktree_path ?? '/wt',
    last_active_at: over.last_active_at ?? 2000,
    agentStatus: over.agentStatus,
    attention: over.attention,
    task_id: over.task_id,
  };
}

// ---------------- 基础工具 ----------------

describe('command-center selector basics', () => {
  it('indexSessions 按 task_id 建索引', () => {
    const idx = indexSessions([
      session({ task_id: 'a', last_active_at: 10 }),
      session({ task_id: 'b', last_active_at: 20 }),
    ]);
    expect(idx.get('a')?.last_active_at).toBe(10);
    expect(idx.get('b')?.last_active_at).toBe(20);
    expect(idx.get('x')).toBeUndefined();
  });

  it('flattenProjects 展开项目-任务对', () => {
    const ps = [project('p1', [task({ id: 'a' }), task({ id: 'b' })], 'A')];
    const flat = flattenProjects(ps);
    expect(flat).toHaveLength(2);
    expect(flat[0]).toEqual({
      task: expect.any(Object),
      project_id: 'p1',
      project_name: 'A',
      project_kind: 'repo',
    });
  });

  it('mergeTask: sessions/active 覆盖 last_active_at/agentStatus/attention', () => {
    const e = { task: task({ id: 'a', updated_at: 100 }), project_id: 'p1', project_name: 'A' };
    const s = session({ task_id: 'a', last_active_at: 999, agentStatus: 'busy' });
    const m = mergeTask(e, s);
    expect(m.last_active_at).toBe(999);
    expect(m.agentStatus).toBe('busy');
    expect(m.projectsOnly).toBe(false);
  });

  it('mergeTask: projects-only 回退 updated_at 与 task.agentStatus，attention 为空', () => {
    const e = { task: task({ id: 'a', updated_at: 123, agentStatus: 'idle' }), project_id: 'p1', project_name: 'A' };
    const m = mergeTask(e, undefined);
    expect(m.last_active_at).toBe(123);
    expect(m.agentStatus).toBe('idle');
    expect(m.projectsOnly).toBe(true);
    expect(m.attention.permissions).toHaveLength(0);
    expect(m.attention.questions).toHaveLength(0);
  });

  it('sessionOnlyTask 构造最小 summary（status=active）', () => {
    const s = session({ task_id: 'orphan', last_active_at: 555 });
    const m = sessionOnlyTask(s);
    expect(m.task.id).toBe('orphan');
    expect(m.task.status).toBe('active');
    expect(m.projectsOnly).toBe(false);
    expect(m.last_active_at).toBe(555);
  });
});

// ---------------- 分类 ----------------

describe('classifyAttention 六类推导', () => {
  it('失败态命中 Kind.Failed', () => {
    const m = mergeTask({ task: task({ id: 'f1', status: 'creation_failed' }), project_id: 'p', project_name: 'P' }, undefined);
    expect(classifyAttention(m)).toEqual([AttentionKind.Failed]);
  });

  it('deletion_failed 命中 Kind.Failed', () => {
    const m = mergeTask({ task: task({ id: 'f2', status: 'deletion_failed' }), project_id: 'p', project_name: 'P' }, undefined);
    expect(classifyAttention(m)).toEqual([AttentionKind.Failed]);
  });

  it('suspended + init_status=failed 命中 Kind.InitFailed', () => {
    const m = mergeTask({ task: task({ id: 'i1', status: 'suspended', init_status: 'failed' }), project_id: 'p', project_name: 'P' }, undefined);
    expect(classifyAttention(m)).toEqual([AttentionKind.InitFailed]);
  });

  it('attention.permissions 非空命中 Kind.PermissionPending', () => {
    const s = session({ task_id: 'p1', attention: { permissions: [{ id: 'r1', permission: 'Edit', patterns: [], since: 1 }], questions: [] } });
    const m = mergeTask({ task: task({ id: 'p1', status: 'active' }), project_id: 'p', project_name: 'P' }, s);
    expect(classifyAttention(m)).toEqual([AttentionKind.PermissionPending]);
  });

  it('attention.questions 非空命中 Kind.QuestionPending', () => {
    const s = session({ task_id: 'q1', attention: { permissions: [], questions: [{ id: 'q', questions: [{ header: 'h', question: 'w' }], since: 1 }] } });
    const m = mergeTask({ task: task({ id: 'q1', status: 'active' }), project_id: 'p', project_name: 'P' }, s);
    expect(classifyAttention(m)).toEqual([AttentionKind.QuestionPending]);
  });

  it('notice 非空命中 Kind.Notice', () => {
    const m = mergeTask({ task: task({ id: 'n1', status: 'suspended', notice: [{ code: 'c', message: 'm', ts: 1 }] }), project_id: 'p', project_name: 'P' }, undefined);
    expect(classifyAttention(m)).toEqual([AttentionKind.Notice]);
  });

  it('agent idle 活跃任务命中 Kind.AgentIdle', () => {
    const m = mergeTask({ task: task({ id: 'id1', status: 'active', agentStatus: 'idle' }), project_id: 'p', project_name: 'P' }, undefined);
    expect(classifyAttention(m)).toEqual([AttentionKind.AgentIdle]);
  });

  it('agent idle 非活跃任务不命中（仅活跃任务）', () => {
    const m = mergeTask({ task: task({ id: 'id2', status: 'suspended', agentStatus: 'idle' }), project_id: 'p', project_name: 'P' }, undefined);
    expect(classifyAttention(m)).toEqual([]);
  });

  it('多类命中按优先级降序排列', () => {
    const s = session({
      task_id: 'multi',
      agentStatus: 'idle',
      attention: {
        permissions: [{ id: 'r', permission: 'x', patterns: [], since: 1 }],
        questions: [{ id: 'q', questions: [{ header: 'h', question: 'w' }], since: 1 }],
      },
    });
    const m = mergeTask(
      { task: task({ id: 'multi', status: 'active', notice: [{ code: 'c', message: 'm', ts: 1 }] }), project_id: 'p', project_name: 'P' },
      s,
    );
    expect(classifyAttention(m)).toEqual([
      AttentionKind.PermissionPending,
      AttentionKind.QuestionPending,
      AttentionKind.Notice,
      AttentionKind.AgentIdle,
    ]);
  });

  it('capabilityUnsupported 时跳过 3/4 类，其余照常', () => {
    const s = session({
      task_id: 'unsup',
      agentStatus: 'idle',
      attention: {
        permissions: [{ id: 'r', permission: 'x', patterns: [], since: 1 }],
        questions: [{ id: 'q', questions: [{ header: 'h', question: 'w' }], since: 1 }],
      },
    });
    const m = mergeTask(
      { task: task({ id: 'unsup', status: 'active', notice: [{ code: 'c', message: 'm', ts: 1 }] }), project_id: 'p', project_name: 'P' },
      s,
    );
    const kinds = classifyAttention(m, true);
    expect(kinds).not.toContain(AttentionKind.PermissionPending);
    expect(kinds).not.toContain(AttentionKind.QuestionPending);
    expect(kinds).toContain(AttentionKind.Notice);
    expect(kinds).toContain(AttentionKind.AgentIdle);
  });
});

// ---------------- 需要关注：去重 + 排序 ----------------

describe('selectAttention 去重与排序', () => {
  it('单任务多类只出现一行，主类别为最高优先级，其余为次要标记', () => {
    const s = session({
      task_id: 'm',
      agentStatus: 'idle',
      attention: {
        permissions: [{ id: 'r', permission: 'x', patterns: [], since: 1 }],
        questions: [{ id: 'q', questions: [{ header: 'h', question: 'w' }], since: 1 }],
      },
    });
    const m = mergeTask(
      { task: task({ id: 'm', status: 'active', notice: [{ code: 'c', message: 'm', ts: 1 }] }), project_id: 'p', project_name: 'P' },
      s,
    );
    const items = selectAttention([m]);
    expect(items).toHaveLength(1);
    expect(items[0].kind).toBe(AttentionKind.PermissionPending);
    expect(items[0].secondary).toEqual([AttentionKind.QuestionPending, AttentionKind.Notice, AttentionKind.AgentIdle]);
    expect(items[0].kinds.size).toBe(4);
  });

  it('类别优先级降序：Failed 排在 InitFailed 之前', () => {
    const a = mergeTask({ task: task({ id: 'a', status: 'creation_failed' }), project_id: 'p', project_name: 'P' }, undefined);
    const b = mergeTask({ task: task({ id: 'b', status: 'suspended', init_status: 'failed' }), project_id: 'p', project_name: 'P' }, undefined);
    const items = selectAttention([b, a]);
    expect(items[0].task.id).toBe('a');
    expect(items[1].task.id).toBe('b');
  });

  it('同类内按 last_active_at 倒序', () => {
    const a = mergeTask({ task: task({ id: 'a', status: 'creation_failed' }), project_id: 'p', project_name: 'P' }, session({ task_id: 'a', last_active_at: 100 }));
    const b = mergeTask({ task: task({ id: 'b', status: 'creation_failed' }), project_id: 'p', project_name: 'P' }, session({ task_id: 'b', last_active_at: 300 }));
    const items = selectAttention([a, b]);
    expect(items.map((i) => i.task.id)).toEqual(['b', 'a']);
  });

  it('时间相同以任务 ID 升序 tie-break', () => {
    const a = mergeTask({ task: task({ id: 'b', status: 'creation_failed' }), project_id: 'p', project_name: 'P' }, session({ task_id: 'b', last_active_at: 100 }));
    const b = mergeTask({ task: task({ id: 'a', status: 'creation_failed' }), project_id: 'p', project_name: 'P' }, session({ task_id: 'a', last_active_at: 100 }));
    const items = selectAttention([a, b]);
    expect(items.map((i) => i.task.id)).toEqual(['a', 'b']);
  });

  it('过渡态排除在需要关注之外（失败态除外）', () => {
    const a = mergeTask({ task: task({ id: 'a', status: 'activating' }), project_id: 'p', project_name: 'P' }, session({ task_id: 'a' }));
    const items = selectAttention([a]);
    expect(items).toHaveLength(0);
  });

  it('失败态即便在过渡态也进需要关注（creation_failed/deletion_failed 非过渡态，但失败态总是进）', () => {
    const a = mergeTask({ task: task({ id: 'a', status: 'creation_failed' }), project_id: 'p', project_name: 'P' }, undefined);
    expect(selectAttention([a])).toHaveLength(1);
  });

  it('无任何命中的任务不进需要关注', () => {
    const a = mergeTask({ task: task({ id: 'a', status: 'active', agentStatus: 'busy' }), project_id: 'p', project_name: 'P' }, session({ task_id: 'a' }));
    expect(selectAttention([a])).toHaveLength(0);
  });
});

// ---------------- 其余活跃任务：分区 + 排序 ----------------

describe('selectActive 分区与排序', () => {
  it('仅含 active 与过渡态，排除挂起/归档/失败态', () => {
    const active = mergeTask({ task: task({ id: 'a', status: 'active' }), project_id: 'p', project_name: 'P' }, session({ task_id: 'a' }));
    const trans = mergeTask({ task: task({ id: 't', status: 'activating' }), project_id: 'p', project_name: 'P' }, session({ task_id: 't' }));
    const susp = mergeTask({ task: task({ id: 's', status: 'suspended' }), project_id: 'p', project_name: 'P' }, undefined);
    const arch = mergeTask({ task: task({ id: 'ar', status: 'archived' }), project_id: 'p', project_name: 'P' }, undefined);
    const failed = mergeTask({ task: task({ id: 'f', status: 'creation_failed' }), project_id: 'p', project_name: 'P' }, undefined);
    const out = selectActive([active, trans, susp, arch, failed]);
    expect(out.map((m) => m.task.id).sort()).toEqual(['a', 'f', 't']);
  });

  it('按 last_active_at 倒序', () => {
    const a = mergeTask({ task: task({ id: 'a', status: 'active' }), project_id: 'p', project_name: 'P' }, session({ task_id: 'a', last_active_at: 100 }));
    const b = mergeTask({ task: task({ id: 'b', status: 'active' }), project_id: 'p', project_name: 'P' }, session({ task_id: 'b', last_active_at: 300 }));
    const c = mergeTask({ task: task({ id: 'c', status: 'active' }), project_id: 'p', project_name: 'P' }, session({ task_id: 'c', last_active_at: 200 }));
    expect(selectActive([a, b, c]).map((m) => m.task.id)).toEqual(['b', 'c', 'a']);
  });

  it('projects-only 活跃任务回退 updated_at 排序', () => {
    const a = mergeTask({ task: task({ id: 'a', status: 'active', updated_at: 100 }), project_id: 'p', project_name: 'P' }, undefined);
    const b = mergeTask({ task: task({ id: 'b', status: 'active', updated_at: 300 }), project_id: 'p', project_name: 'P' }, undefined);
    expect(selectActive([a, b]).map((m) => m.task.id)).toEqual(['b', 'a']);
  });

  it('时间相同 ID 升序 tie-break', () => {
    const a = mergeTask({ task: task({ id: 'b', status: 'active' }), project_id: 'p', project_name: 'P' }, session({ task_id: 'b', last_active_at: 100 }));
    const b = mergeTask({ task: task({ id: 'a', status: 'active' }), project_id: 'p', project_name: 'P' }, session({ task_id: 'a', last_active_at: 100 }));
    expect(selectActive([a, b]).map((m) => m.task.id)).toEqual(['a', 'b']);
  });
});

// ---------------- 挂起与归档：分区 + 排序 ----------------

describe('selectParked 分区与排序', () => {
  it('仅含 suspended + archived', () => {
    const susp = mergeTask({ task: task({ id: 's', status: 'suspended', updated_at: 10 }), project_id: 'p', project_name: 'P' }, undefined);
    const arch = mergeTask({ task: task({ id: 'ar', status: 'archived', updated_at: 20 }), project_id: 'p', project_name: 'P' }, undefined);
    const active = mergeTask({ task: task({ id: 'a', status: 'active' }), project_id: 'p', project_name: 'P' }, session({ task_id: 'a' }));
    expect(selectParked([susp, arch, active]).map((m) => m.task.id).sort()).toEqual(['ar', 's']);
  });

  it('挂起优先：交错输入 → suspended 全在前、各组内 updated_at 倒序', () => {
    const a = mergeTask({ task: task({ id: 'a', status: 'suspended', updated_at: 10 }), project_id: 'p', project_name: 'P' }, undefined);
    const b = mergeTask({ task: task({ id: 'b', status: 'archived', updated_at: 50 }), project_id: 'p', project_name: 'P' }, undefined);
    const c = mergeTask({ task: task({ id: 'c', status: 'suspended', updated_at: 30 }), project_id: 'p', project_name: 'P' }, undefined);
    const d = mergeTask({ task: task({ id: 'd', status: 'archived', updated_at: 40 }), project_id: 'p', project_name: 'P' }, undefined);
    // 挂起组：c(30) > a(10)；归档组：b(50) > d(40)；挂起整体在前（尽管 b/d 更新）
    expect(selectParked([a, b, c, d]).map((m) => m.task.id)).toEqual(['c', 'a', 'b', 'd']);
  });

  it('时间相同 ID 升序 tie-break', () => {
    const a = mergeTask({ task: task({ id: 'b', status: 'suspended', updated_at: 10 }), project_id: 'p', project_name: 'P' }, undefined);
    const b = mergeTask({ task: task({ id: 'a', status: 'suspended', updated_at: 10 }), project_id: 'p', project_name: 'P' }, undefined);
    expect(selectParked([a, b]).map((m) => m.task.id)).toEqual(['a', 'b']);
  });
});

// ---------------- buildCommandCenterView：双快照 join ----------------

describe('buildCommandCenterView 双快照 join', () => {
  it('字段级来源：身份/status/branch 来自 projects，last_active_at/agentStatus/attention 来自 sessions', () => {
    // 两侧身份完全不同（project_id + project_name 均冲突）：MUST 取 projects 侧两者
    const ps = [project('p-proj', [task({ id: 'a', name: 'from-projects', status: 'active', branch: 'dev' })], 'ProjA')];
    const ss = [session({ task_id: 'a', project_id: 'STALE-session-id', project_name: 'STALE-session-name', last_active_at: 555, agentStatus: 'busy', branch: 'STALE-from-session' })];
    const view = buildCommandCenterView(ps, ss);
    const a = view.active[0];
    expect(a.task.name).toBe('from-projects'); // projects
    expect(a.task.branch).toBe('dev'); // projects
    expect(a.task.status).toBe('active'); // projects
    expect(a.last_active_at).toBe(555); // sessions
    expect(a.agentStatus).toBe('busy'); // sessions
    expect(a.project_id).toBe('p-proj'); // projects（冲突时取 projects 侧）
    expect(a.project_name).toBe('ProjA'); // projects（冲突时取 projects 侧）
  });

  it('单侧存在：仅 projects 快照 → 按 projects 状态归入分区', () => {
    const ps = [project('p1', [task({ id: 'a', status: 'active' })], 'A')];
    const view = buildCommandCenterView(ps, []);
    expect(view.active.map((m) => m.task.id)).toEqual(['a']);
    expect(view.active[0].projectsOnly).toBe(true);
    expect(view.active[0].last_active_at).toBe(1000); // updated_at 回退
  });

  it('单侧存在：仅 sessions/active（无注意力）→ 归活跃分区', () => {
    const ss = [session({ task_id: 'orphan', last_active_at: 99 })];
    const view = buildCommandCenterView([], ss);
    expect(view.active.map((m) => m.task.id)).toEqual(['orphan']);
    expect(view.attention).toHaveLength(0);
  });

  it('单侧存在：仅 sessions/active 有 permission → 归需要关注，不重复在活跃分区', () => {
    const ss = [session({ task_id: 'orphan', last_active_at: 99, attention: { permissions: [{ id: 'r', permission: 'x', patterns: [], since: 1 }], questions: [] } })];
    const view = buildCommandCenterView([], ss);
    expect(view.attention.map((i) => i.task.id)).toEqual(['orphan']);
    expect(view.attention[0].kind).toBe(AttentionKind.PermissionPending);
    // 进入需要关注的任务不重复在活跃分区
    expect(view.active).toHaveLength(0);
  });

  it('快照不一致各自呈现（不合并修复）', () => {
    // projects 有 a；sessions 有 b；无交集
    const ps = [project('p1', [task({ id: 'a', status: 'active' })], 'A')];
    const ss = [session({ task_id: 'b', last_active_at: 1 })];
    const view = buildCommandCenterView(ps, ss);
    expect(view.active.map((m) => m.task.id).sort()).toEqual(['a', 'b']);
  });

  it('capabilityUnsupported 仅缺 3/4 类，其余类照常', () => {
    const ss = [session({
      task_id: 'x',
      attention: { permissions: [{ id: 'r', permission: 'x', patterns: [], since: 1 }], questions: [] },
    })];
    const ps = [project('p1', [
      task({ id: 'f', status: 'creation_failed' }),
      task({ id: 'n', status: 'suspended', notice: [{ code: 'c', message: 'm', ts: 1 }] }),
      task({ id: 'i', status: 'suspended', init_status: 'failed' }),
      task({ id: 'idle', status: 'active', agentStatus: 'idle' }),
    ], 'A')];
    const view = buildCommandCenterView(ps, ss, true);
    const kinds = view.attention.map((i) => i.kind);
    expect(kinds).toContain(AttentionKind.Failed);
    expect(kinds).toContain(AttentionKind.InitFailed);
    expect(kinds).toContain(AttentionKind.Notice);
    expect(kinds).toContain(AttentionKind.AgentIdle);
    // 3/4 类缺失（permission 项因 unsupported 跳过）
    expect(kinds).not.toContain(AttentionKind.PermissionPending);
    expect(kinds).not.toContain(AttentionKind.QuestionPending);
  });

  it('Scenario 首页分区呈现：需要关注 / 其余活跃 / 挂起', () => {
    const ps = [project('p1', [
      task({ id: 'perm', status: 'active' }),
      task({ id: 'act1', status: 'active' }),
      task({ id: 'act2', status: 'active' }),
      task({ id: 'park', status: 'suspended', updated_at: 5 }),
    ], 'A')];
    const ss = [session({ task_id: 'perm', attention: { permissions: [{ id: 'r', permission: 'x', patterns: [], since: 1 }], questions: [] } })];
    const view = buildCommandCenterView(ps, ss);
    expect(view.attention.map((i) => i.task.id)).toEqual(['perm']);
    // 权限等待任务不重复出现在「其余活跃任务」区
    expect(view.active.map((m) => m.task.id).sort()).toEqual(['act1', 'act2']);
    expect(view.parked.map((m) => m.task.id)).toEqual(['park']);
  });

  it('需要关注的活跃任务不重复出现在其余活跃任务', () => {
    const ps = [project('p1', [
      task({ id: 'failed', status: 'creation_failed' }),
      task({ id: 'normal', status: 'active' }),
    ], 'A')];
    const view = buildCommandCenterView(ps, []);
    expect(view.attention.map((i) => i.task.id)).toEqual(['failed']);
    // failed 任务已在需要关注，不重复出现在 active
    expect(view.active.map((m) => m.task.id)).toEqual(['normal']);
  });
});

// ---------------- 辅助 ----------------

describe('辅助函数', () => {
  it('hasPreDeleteLog: last_error 以 pre-delete: 前缀', () => {
    expect(hasPreDeleteLog('pre-delete: script failed')).toBe(true);
    expect(hasPreDeleteLog('other error')).toBe(false);
    expect(hasPreDeleteLog(undefined)).toBe(false);
  });

  it('attentionKindLabel 返回中文标签', () => {
    expect(attentionKindLabel(AttentionKind.Failed)).toBe('失败');
    expect(attentionKindLabel(AttentionKind.InitFailed)).toBe('init 失败');
    expect(attentionKindLabel(AttentionKind.PermissionPending)).toBe('等待权限确认');
    expect(attentionKindLabel(AttentionKind.AgentIdle)).toBe('空闲');
  });

  it('noticeItems 过滤非法条目', () => {
    expect(noticeItems([{ code: 'c', message: 'm', ts: 1 }])).toHaveLength(1);
    expect(noticeItems(undefined)).toHaveLength(0);
    expect(noticeItems([{ code: 'c', message: '', ts: 1 }])).toHaveLength(1); // message 空串仍合法
    expect(noticeItems([{ code: '', ts: 1 }] as unknown as TaskSummary['notice'])).toHaveLength(0); // message 缺失非法
  });
});

// ---------------- 分支刷新代际/项目 ID 校验（纯函数） ----------------

describe('shouldAcceptBranchResult 代际+项目 ID 校验', () => {
  it('代际未变且项目一致 → 接受', () => {
    expect(shouldAcceptBranchResult(3, 3, 'A', 'A')).toBe(true);
  });

  it('代际已推进（项目切换）→ 拒绝', () => {
    expect(shouldAcceptBranchResult(3, 4, 'A', 'A')).toBe(false);
  });

  it('当前选择已切到不同项目 → 拒绝', () => {
    expect(shouldAcceptBranchResult(3, 3, 'A', 'B')).toBe(false);
  });

  it('当前选择已清空 → 拒绝', () => {
    expect(shouldAcceptBranchResult(3, 3, 'A', null)).toBe(false);
  });

  it('代际与项目均变 → 拒绝', () => {
    expect(shouldAcceptBranchResult(3, 5, 'A', 'B')).toBe(false);
  });
});

describe('shouldClearRefreshing finally 释放刷新所有权（项目+代际）', () => {
  it('本次刷新仍持有所有权（项目与代际均匹配）→ 释放', () => {
    expect(shouldClearRefreshing('A', 3, 'A', 3)).toBe(true);
  });

  it('已切到新项目（所有权属新刷新/被 effect 重置）→ 不释放', () => {
    expect(shouldClearRefreshing('B', 5, 'A', 3)).toBe(false);
  });

  it('同项目但代际已推进（所有权被新一轮持有）→ 不释放', () => {
    expect(shouldClearRefreshing('A', 4, 'A', 3)).toBe(false);
  });

  it('当前无刷新所有权（已被 effect 清空）→ 不释放', () => {
    expect(shouldClearRefreshing(null, 0, 'A', 3)).toBe(false);
  });
});

// ---------------- sessions 首屏状态机（loading / connecting / error / empty / ready） ----------------

describe('resolveSessionsBootstrap sessions 首屏状态机（SSE 版）', () => {
  const base = {
    projectsInit: false,
    projectsLen: 0,
    sessionsAttempted: false,
    sessionsInitialized: false,
    sessionsLen: 0,
    sessionsError: '',
    attentionLen: 0,
    activeLen: 0,
    parkedLen: 0,
  };

  it('projects 未初始化且无数据 → loading（sessions 各阶段均不改变整页 loading 判定）', () => {
    expect(resolveSessionsBootstrap(base)).toBe('loading');
    expect(resolveSessionsBootstrap({ ...base, sessionsAttempted: true })).toBe('loading');
    expect(
      resolveSessionsBootstrap({ ...base, sessionsAttempted: true, sessionsInitialized: true }),
    ).toBe('loading');
  });

  it('projects 初始化为空 + sessions 首帧未到 → connecting（不整页 loading、不空态）', () => {
    const phase = resolveSessionsBootstrap({ ...base, projectsInit: true });
    expect(phase).toBe('connecting');
    expect(phase).not.toBe('loading');
    expect(phase).not.toBe('empty');
  });

  it('projects 有数据 + sessions 首帧未到 → connecting（不升级整页 loading，projects-only 照常渲染）', () => {
    const phase = resolveSessionsBootstrap({
      ...base,
      projectsInit: true,
      projectsLen: 1,
      activeLen: 2,
    });
    expect(phase).toBe('connecting');
    expect(phase).not.toBe('loading');
    // 分区有条目仍渲染列表（projects-only 数据不隐藏）
    expect(sectionBodyMode(phase, 2)).toBe('list');
    // 空分区不显示「暂无…」占位
    expect(sectionBodyMode(phase, 0)).toBe('none');
  });

  it('首次连接失败 → error（不与 connecting/loading/empty 并存）', () => {
    const phase = resolveSessionsBootstrap({
      ...base,
      projectsInit: true,
      sessionsAttempted: true,
      sessionsInitialized: false,
      sessionsError: '无法连接服务端（ocdeck-server 未运行？）',
    });
    expect(phase).toBe('error');
    expect(phase).not.toBe('connecting');
    expect(phase).not.toBe('loading');
    expect(phase).not.toBe('empty');
  });

  it('projects 非空 + sessions 首帧为空数组 → ready（继续 projects-only 渲染，不显示全局空态）', () => {
    const phase = resolveSessionsBootstrap({
      ...base,
      projectsInit: true,
      projectsLen: 1,
      sessionsAttempted: true,
      sessionsInitialized: true,
      activeLen: 1,
    });
    expect(phase).toBe('ready');
    expect(phase).not.toBe('empty');
  });

  it('两侧均成功初始化且三区皆空 → empty', () => {
    expect(
      resolveSessionsBootstrap({
        ...base,
        projectsInit: true,
        sessionsAttempted: true,
        sessionsInitialized: true,
      }),
    ).toBe('empty');
  });
});

describe('shouldShowSectionEmpty 分区空态门禁', () => {
  it('loading / connecting / error → 不展示分区「暂无…」', () => {
    expect(shouldShowSectionEmpty('loading')).toBe(false);
    expect(shouldShowSectionEmpty('connecting')).toBe(false);
    expect(shouldShowSectionEmpty('error')).toBe(false);
  });

  it('empty / ready → 允许分区空态', () => {
    expect(shouldShowSectionEmpty('empty')).toBe(true);
    expect(shouldShowSectionEmpty('ready')).toBe(true);
  });

  it('首次连接失败链路：error 相位抑制分区空态', () => {
    const phase = resolveSessionsBootstrap({
      projectsInit: true,
      projectsLen: 0,
      sessionsAttempted: true,
      sessionsInitialized: false,
      sessionsLen: 0,
      sessionsError: '无法连接服务端（ocdeck-server 未运行？）',
      attentionLen: 0,
      activeLen: 0,
      parkedLen: 0,
    });
    expect(phase).toBe('error');
    expect(shouldShowSectionEmpty(phase)).toBe(false);
  });
});

describe('sectionBodyMode 列表始终渲染 / 空态门禁', () => {
  it('itemCount>0 时任意 phase 均为 list（sessions 连接中/失败不隐藏 projects 数据）', () => {
    for (const phase of ['loading', 'connecting', 'error', 'empty', 'ready'] as const) {
      expect(sectionBodyMode(phase, 2)).toBe('list');
      expect(sectionBodyMode(phase, 1)).toBe('list');
    }
  });

  it('itemCount=0 时 loading/connecting/error → none（不显示暂无…）', () => {
    expect(sectionBodyMode('loading', 0)).toBe('none');
    expect(sectionBodyMode('connecting', 0)).toBe('none');
    expect(sectionBodyMode('error', 0)).toBe('none');
  });

  it('itemCount=0 时 empty/ready → empty（显示暂无…）', () => {
    expect(sectionBodyMode('empty', 0)).toBe('empty');
    expect(sectionBodyMode('ready', 0)).toBe('empty');
  });

  it('sessions 连接中 + projects 有活跃/挂起任务：列表渲染、空态不显示', () => {
    expect(sectionBodyMode('connecting', 3)).toBe('list'); // active
    expect(sectionBodyMode('connecting', 1)).toBe('list'); // parked
    expect(sectionBodyMode('connecting', 0)).toBe('none'); // 无条目才抑制
  });
});

// ---------------- 页面接线契约（SSE 订阅替代 5s 轮询，design D5 / tasks P2.7.2） ----------------

describe('CommandCenterPage SSE 接线契约（源码断言）', () => {
  const src = readFileSync(
    fileURLToPath(new URL('../pages/CommandCenterPage.tsx', import.meta.url)),
    'utf8',
  );

  it('不再以 usePoll/interval 轮询 sessions/active，也不直接调用 REST listActiveSessions', () => {
    expect(src).not.toMatch(/usePoll/);
    expect(src).not.toMatch(/pollSessions/);
    expect(src).not.toMatch(/listActiveSessions/);
    expect(src).not.toMatch(/setInterval/);
  });

  it('mount 订阅 SSE、unmount 关闭（effect cleanup 调 sub.close()）', () => {
    expect(src).toMatch(/subscribeActiveSessions\(\{/);
    expect(src).toMatch(/return \(\) => sub\.close\(\)/);
  });

  it('用户可见文案不再出现「每 5 秒」刷新表述（改为连接状态中性文案）', () => {
    expect(src).not.toContain('每 5 秒');
  });
});

describe('resolveNewTaskInit 命令面板初始化信号', () => {
  const ocdeck = project('p1', [], 'ocdeck');
  const other = project('p2', [], 'other');
  const ocdeckTwin = project('p3', [], 'ocdeck');
  const foobar = project('p4', [], 'foobar');

  it('无 payload 保持 keep', () => {
    expect(resolveNewTaskInit({}, [ocdeck], 'exact-then-substring')).toEqual({ action: 'keep' });
  });

  it('空字符串 payload 清空项目选择/过滤词', () => {
    expect(resolveNewTaskInit({ projectName: '' }, [ocdeck], 'exact-then-substring')).toEqual({
      action: 'apply',
      selected: null,
      projectQuery: '',
    });
  });

  it('唯一精确匹配预选', () => {
    const r = resolveNewTaskInit({ projectName: 'ocdeck' }, [ocdeck, other], 'exact');
    expect(r).toEqual({ action: 'apply', selected: ocdeck, projectQuery: 'ocdeck' });
  });

  it('唯一子串匹配在 exact-then-substring 预选', () => {
    const r = resolveNewTaskInit({ projectName: 'deck' }, [ocdeck, other], 'exact-then-substring');
    expect(r).toEqual({ action: 'apply', selected: ocdeck, projectQuery: 'ocdeck' });
  });

  it('matchMode=exact 时子串不预选，填过滤词', () => {
    expect(resolveNewTaskInit({ projectName: 'deck' }, [ocdeck, other], 'exact')).toEqual({
      action: 'apply',
      selected: null,
      projectQuery: 'deck',
    });
  });

  it('零命中填过滤词不预选', () => {
    expect(resolveNewTaskInit({ projectName: 'zzzz' }, [ocdeck, other], 'exact-then-substring')).toEqual({
      action: 'apply',
      selected: null,
      projectQuery: 'zzzz',
    });
  });

  it('多命中不预选填过滤词', () => {
    expect(resolveNewTaskInit({ projectName: 'ocdeck' }, [ocdeck, ocdeckTwin], 'exact-then-substring')).toEqual({
      action: 'apply',
      selected: null,
      projectQuery: 'ocdeck',
    });
  });

  it('有效 projectID 直接选中，不走文本推断', () => {
    const r = resolveNewTaskInit(
      { projectName: 'ocdeck', projectID: 'p3' },
      [ocdeck, ocdeckTwin],
      'exact-then-substring',
    );
    expect(r).toEqual({ action: 'apply', selected: ocdeckTwin, projectQuery: 'ocdeck' });
  });

  it('失效 projectID 回退唯一文本匹配', () => {
    const r = resolveNewTaskInit(
      { projectName: 'foobar', projectID: 'gone' },
      [ocdeck, foobar],
      'exact-then-substring',
    );
    expect(r).toEqual({ action: 'apply', selected: foobar, projectQuery: 'foobar' });
  });

  it('失效 projectID 且文本匹配失败则填过滤词', () => {
    expect(
      resolveNewTaskInit({ projectName: 'zzzz', projectID: 'gone' }, [ocdeck], 'exact-then-substring'),
    ).toEqual({ action: 'apply', selected: null, projectQuery: 'zzzz' });
  });
});
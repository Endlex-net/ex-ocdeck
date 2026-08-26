// 与 internal/api DTO 对齐的前端类型（design.md §21）。

/** 项目类型（add-plain-dir-project D1/D6）：repo=git 仓库，dir=纯目录（无 git 功能）。 */
export type ProjectKind = 'repo' | 'dir';

export interface Project {
  id: string;
  name: string;
  path: string;
  /** 项目类型：repo（缺省）| dir。UI 降级判断的唯一依据，MUST NOT 前端自行推断。 */
  kind: ProjectKind;
  default_branch: string;
  created_at: number;
  /** 任务总数与状态分布（列表与详情均返回，字段一致）。 */
  task_count: number;
  tasks_by_status: Record<string, number>;
  /** 项目任务摘要数组（design.md D4，GET /projects 与 /projects/:id 透出）。
   *  11 字段 = 10 存储字段 + attention_count；agentStatus 水合失败省略。无任务为 []。 */
  tasks: TaskSummary[];
}

export interface ProjectDetail extends Project {}

/** 项目任务摘要（GET /projects 与 /projects/:id 的 tasks[] 元素）。
 *  字段名与 internal/api projectTaskSummaryDTO 对齐。 */
export interface TaskSummary {
  id: string;
  name: string;
  status: string;
  init_status: string;
  branch: string;
  worktree_path: string;
  last_error?: string;
  /** json.RawMessage 原样透传：客户端收到的是数组而非字符串。 */
  notice?: NoticeItem[];
  updated_at: number;
  /** agent 运行态（idle/busy/retry）；非 active 或水合失败省略。 */
  agentStatus?: string;
  attention_count: number;
}

/** 权限请求信号（attentionDTO.permissions[] 元素）。 */
export interface PermissionSignal {
  id: string;
  permission: string;
  patterns: string[];
  since: number;
}

/** 提问信号单个条目。 */
export interface QuestionItem {
  header: string;
  question: string;
}

/** 提问请求信号（attentionDTO.questions[] 元素）。 */
export interface QuestionSignal {
  id: string;
  questions: QuestionItem[];
  since: number;
}

/** 注意力信号快照（design.md D6 GET /tasks/:id 与 /tasks/active 透出）。
 *  空集合为非 nil 空数组（后端保证）；unsupported 透出空数组。 */
export interface Attention {
  permissions: PermissionSignal[];
  questions: QuestionSignal[];
}

export interface TaskSession {
  session_id: string;
  last_seen_at: number;
}

export interface NoticeItem {
  code: string;
  message: string;
  ts: number;
}

export interface Task {
  id: string;
  project_id: string;
  /** 所属项目类型（add-plain-dir-project D6）：UI 降级判断的唯一依据。 */
  project_kind: ProjectKind;
  name: string;
  branch: string;
  status: string;
  worktree_path: string;
  last_port?: number;
  last_error?: string;
  /** 服务端以 json.RawMessage 原样输出，客户端收到的是数组而非字符串。 */
  notice?: NoticeItem[];
  delete_mode?: string;
  /** init 脚本状态（project-lifecycle-config）：none|pending|running|succeeded|failed。后端 DTO 始终返回。 */
  init_status: string;
  /** init 失败原因（权威信息，日志仅辅助）。 */
  init_error?: string;
  created_at: number;
  updated_at: number;
  sessions?: TaskSession[];
  /** agent 运行态（design.md 2.8）：idle/busy/retry；非 active 或查询失败为空串。 */
  agentStatus?: string;
  /** 注意力信号快照（design.md D6 GET /tasks/:id 透出）。空数组非 null；unsupported 为空数组。 */
  attention?: Attention;
}

export interface TerminalInfo {
  terminal_id: string;
}

/** GET /tasks/active 响应元素（历史路径 sessions/active，见 cross-project-active-sessions design.md D3）。
 *  agentStatus 水合失败/超时时省略，渲染按未知降级处理。 */
export interface ActiveSessionItem {
  task_id: string;
  project_id: string;
  project_name: string;
  name: string;
  branch: string;
  worktree_path: string;
  /** 最近活跃时间（Unix 秒）：task_sessions.last_seen_at 的 MAX，无会话行回退 tasks.updated_at。 */
  last_active_at: number;
  agentStatus?: string;
  /** 注意力信号快照（design.md D6 GET /tasks/active 透出）。空数组非 null；unsupported 为空数组。 */
  attention?: Attention;
}

/** GET /tasks/:id/git/status 文件条目。x/y 为 git status --short 双字母状态码。 */
export interface GitFileEntry {
  path: string;
  x: string;
  y: string;
  staged: boolean;
  unstaged: boolean;
  untracked: boolean;
  additions: number;
  deletions: number;
  isBinary: boolean;
}

export interface GitStatus {
  branch: string;
  files: GitFileEntry[];
}

export interface GitDiffResult {
  diff: string;
  truncated: boolean;
}

export interface EnvVar {
  key: string;
  value: string;
}

/** GET/PUT /projects/:id/lifecycle-config（project-lifecycle-config design.md §8）。 */
export interface LifecycleConfig {
  inherit_patterns: string;
  init_script: string;
  pre_delete_script: string;
}

/** GET env 响应；PUT/DELETE 响应仅含 restartRequired + warning。 */
export interface EnvResponse {
  vars?: EnvVar[];
  restartRequired: boolean;
  warning?: string;
}

/** 全局环境变量模式（design.md 2.9）：follow_host 激活时从服务端进程 env 解析。 */
export type GlobalEnvMode = 'follow_host' | 'manual';

export interface GlobalEnvVar {
  key: string;
  mode: GlobalEnvMode;
  /** manual 模式的显式值；follow_host 为空。 */
  value: string;
  /** follow_host 模式下服务端进程的当前解析值；宿主未设置为空串。 */
  resolvedValue: string;
}

/** GET /env 响应；PUT/DELETE 响应仅含 restartRequired + warning。 */
export interface GlobalEnvResponse {
  vars?: GlobalEnvVar[];
  restartRequired: boolean;
  warning?: string;
}

export interface OcConfigInfo {
  name: string;
}

export interface OcConfigContent {
  name: string;
  content: string;
  mtime: number;
  hash: string;
}

export interface OcConfigSaveResult {
  mtime: number;
  hash: string;
  affectedActiveTasks: string[];
  restartRequired: boolean;
}

/** GET /server/status 响应（internal/api handleServerStatus，map key 即字段名）。 */
export interface ServerStatus {
  opencodeVersion: string;
  tmuxVersion: string;
  shutdownPolicy: string;
  watchdogState: 'off' | 'running' | 'degraded';
  contractMinVersion: string;
  contractBaseline: string;
  versionVerified: boolean;
}

/** 全局 AI provider 配置（ai-worktree-naming design.md D6）：GET/PUT /ai/config 同形响应。 */
export interface AIConfig {
  configured: boolean;
  provider: string;
  base_url: string;
  model: string;
  api_key_masked: string;
  /** 思考强度："" = 未设置/默认（不下发参数），否则 off | low | medium | high。 */
  thinking: string;
  load_error?: string;
}

/** 过渡态：操作进行中，UI 禁用操作并显示 spinner。 */
export const TRANSITIONAL_STATUS = new Set([
  'creating',
  'activating',
  'suspending',
  'deleting',
]);

export const FAILED_STATUS = new Set(['creation_failed', 'deletion_failed']);

export function isTransitional(status: string): boolean {
  return TRANSITIONAL_STATUS.has(status);
}

/**
 * init 门禁（tasks.md 5.3 / design.md §5）：仅 none|succeeded 可激活（空串原因）；
 * 缺失/空串/未知值一律 fail-closed 阻断（同后端 Activate 门禁，防御契约漂移）。
 * init_error 为权威失败信息。
 */
export function initActivateBlockReason(task: Task): string {
  switch (task.init_status) {
    case 'none':
    case 'succeeded':
      return '';
    case 'pending':
    case 'running':
      return 'init 进行中，完成后方可激活';
    case 'failed':
      return task.init_error ? `init 失败：${task.init_error}` : 'init 失败，请查看日志';
    default:
      return `init 状态未知（${task.init_status ?? ''}），暂不可激活`;
  }
}

export function parseNotice(raw: Task['notice']): NoticeItem[] {
  if (!raw) return [];
  if (Array.isArray(raw)) {
    return raw.filter(
      (n): n is NoticeItem =>
        !!n && typeof n === 'object' && typeof (n as NoticeItem).message === 'string',
    );
  }
  return [];
}

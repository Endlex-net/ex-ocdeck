// 与 internal/api DTO 对齐的前端类型（design.md §21）。

export interface Project {
  id: string;
  name: string;
  path: string;
  default_branch: string;
  created_at: number;
  /** 任务总数与状态分布（列表与详情均返回，字段一致）。 */
  task_count: number;
  tasks_by_status: Record<string, number>;
}

export interface ProjectDetail extends Project {}

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
  name: string;
  branch: string;
  status: string;
  worktree_path: string;
  last_port?: number;
  last_error?: string;
  /** 服务端以 json.RawMessage 原样输出，客户端收到的是数组而非字符串。 */
  notice?: NoticeItem[];
  delete_mode?: string;
  created_at: number;
  updated_at: number;
  sessions?: TaskSession[];
  /** agent 运行态（design.md 2.8）：idle/busy/retry；非 active 或查询失败为空串。 */
  agentStatus?: string;
}

export interface TerminalInfo {
  terminal_id: string;
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
  contractBaseline: string;
  versionVerified: boolean;
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

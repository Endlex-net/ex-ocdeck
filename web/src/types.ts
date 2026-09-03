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

/** GET /tasks/:id/git/diff 响应（codemirror-git-diff spec「文件 diff 查看」）。
 *  八字段始终全部返回：两侧版本内容 + 存在性 + git mode + 二进制/截断标记。
 *  mode 为 git 八进制 mode 文本（100644/100755/120000 symlink/160000 gitlink），
 *  不存在侧的 content 与 mode 均为空串。
 *  isBinary=true 时两侧内容为空串；truncated 仅表示大小截断（单侧 512KB 上限），不兼任二进制含义。 */
export interface GitDiffResult {
  oldContent: string;
  newContent: string;
  oldExists: boolean;
  newExists: boolean;
  oldMode: string;
  newMode: string;
  isBinary: boolean;
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

/** 通知配置五类别开关（snake_case 与后端 DTO 对齐）。 */
export interface NotificationCategories {
  question: boolean;
  permission: boolean;
  idle: boolean;
  retry: boolean;
  error: boolean;
}

/** 通知配置 bark 渠道（GET 为 token_masked，PUT 提交 token）。 */
export interface NotificationBarkChannel {
  enabled: boolean;
  endpoint: string;
  token_masked?: string;
}

/** 通知配置 wecom 渠道（GET 为 url_masked 必填，PUT 提交 url）。
 *  完整 webhook URL 整体按密钥保护，url_masked 仅为 `***` 或空串，MUST NOT 回显原文。 */
export interface NotificationWecomChannel {
  enabled: boolean;
  url_masked: string;
}

/** 通知配置 channels 结构。 */
export interface NotificationChannels {
  web: { enabled: boolean };
  bark: NotificationBarkChannel;
  macos: { enabled: boolean };
  wecom: NotificationWecomChannel;
}

/** GET /api/v1/notification/config 响应（token_masked 形态）。
 *  load_error 只读、仅配置损坏时出现。 */
export interface NotificationConfig {
  enabled: boolean;
  categories: NotificationCategories;
  idle_timeout_seconds: number;
  channels: NotificationChannels;
  llm_summary: boolean;
  base_url: string;
  load_error?: string;
}

/** PUT /api/v1/notification/config 请求体：bark 令牌字段名为 token（非 token_masked），
 *  wecom webhook 字段名为 url（非 url_masked）。 */
export interface NotificationConfigPut {
  enabled: boolean;
  categories: NotificationCategories;
  idle_timeout_seconds: number;
  channels: {
    web: { enabled: boolean };
    bark: { enabled: boolean; endpoint: string; token: string };
    macos: { enabled: boolean };
    wecom: { enabled: boolean; url: string };
  };
  llm_summary: boolean;
  base_url: string;
}

/** 测试通知响应元素：status ∈ success | failed | skipped。 */
export interface NotificationTestResult {
  name: string;
  status: 'success' | 'failed' | 'skipped';
  error: string;
}

/** 通知 SSE 帧意图（event: notification 的 data，snake_case 七字段）。 */
export interface NotificationIntent {
  task_id: string;
  task_name: string;
  category: string;
  level: string;
  title: string;
  body: string;
  url: string;
}

// ---------- diff review 批注/提交/文件编辑（diff-review-workbench design.md D8，
// 与 internal/api/annotations.go、git.go 的 DTO 逐字对齐，字段一律 camelCase） ----------

/** 批注锚定侧（annotationDTO.side）。 */
export type DiffSide = 'old' | 'new';

/** 换行风格（diffreview.LineEnding）。 */
export type LineEnding = 'lf' | 'crlf';

/** 文件编辑不可编辑原因七值枚举（diffreview.ReadReasonCode，design.md D5）。 */
export type FileEditReasonCode =
  | 'binary'
  | 'non_utf8'
  | 'mixed_line_endings'
  | 'too_large'
  | 'not_regular'
  | 'missing'
  | 'read_only';

/** 批注（annotationDTO）。stale 由服务端 D4 惰性计算；revision 随每次实变严格 +1。 */
export interface Annotation {
  id: string;
  path: string;
  side: DiffSide;
  ref: string;
  untracked: boolean;
  startLine: number;
  endLine: number;
  snapshotStartLine: number;
  snapshotLineCount: number;
  snapshot: string;
  comment: string;
  revision: number;
  stale: boolean;
  createdAt: number;
  updatedAt: number;
}

/** 提交快照条目（submissionItemDTO）：不含 snapshotLineCount（由 snapshot 行数推导，
 *  不属于提交快照契约）。 */
export interface SubmissionItem {
  annotationId: string;
  path: string;
  side: DiffSide;
  ref: string;
  untracked: boolean;
  startLine: number;
  endLine: number;
  snapshotStartLine: number;
  snapshot: string;
  comment: string;
}

/** 提交状态（submissionDTO.status）。 */
export type SubmissionStatus = 'queued' | 'sending' | 'sent' | 'failed' | 'delivery_unknown';

/** 提交记录（submissionDTO）。sentAt 仅 status=sent 时非 null；items 始终存在（可为空数组）。 */
export interface Submission {
  id: string;
  status: SubmissionStatus;
  note: string;
  payload: string;
  truncated: boolean;
  error: string;
  createdAt: number;
  sentAt: number | null;
  items: SubmissionItem[];
}

/** 提交能力（submitCapabilityDTO）：state 不暴露 absent，前端按 unknown 降级处理。 */
export interface SubmitCapability {
  state: 'supported' | 'unsupported' | 'unknown';
  reason: string;
}

/** GET /tasks/:id/annotations 响应。 */
export interface AnnotationsListResponse {
  annotations: Annotation[];
  submitCapability: SubmitCapability;
}

/** POST /tasks/:id/annotations 请求体（createAnnotationReq）。 */
export interface AnnotationCreateInput {
  path: string;
  side: DiffSide;
  ref: string;
  untracked: boolean;
  startLine: number;
  endLine: number;
  snapshotStartLine: number;
  snapshotLineCount: number;
  snapshot: string;
  comment: string;
}

/** 提交批次条目（submissionItemReq）：id + revision（1..MaxInt64 JSON 整数）。 */
export interface SubmissionAnnotationRef {
  id: string;
  revision: number;
}

/** GET /tasks/:id/annotation-submissions 分区响应：queue=queued/sending（seq ASC）、
 *  history=sent（sent_at DESC, seq DESC）、failures=failed/delivery_unknown（created_at DESC, seq DESC）。 */
export interface SubmissionsListResponse {
  queue: Submission[];
  history: Submission[];
  failures: Submission[];
}

/** GET /tasks/:id/git/file 判别联合 editable=true 分支（fileEditEditableDTO）。 */
export interface FileEditReadEditable {
  editable: true;
  content: string;
  baseHash: string;
  lineEnding: LineEnding;
  hasBom: boolean;
  mode: string;
}

/** GET /tasks/:id/git/file 判别联合 editable=false 分支（fileEditNotEditableDTO）。 */
export interface FileEditReadNotEditable {
  editable: false;
  reasonCode: FileEditReasonCode;
  reason: string;
}

/** 文件编辑读取判别联合（design.md D5/D8）：两组分支字段互斥。 */
export type FileEditRead = FileEditReadEditable | FileEditReadNotEditable;

/** POST /tasks/:id/git/file 请求体（fileEditWriteReq）。 */
export interface FileEditWriteInput {
  path: string;
  content: string;
  baseHash: string;
  lineEnding: LineEnding;
  baseMode: string;
}

/** POST /tasks/:id/git/file 响应（fileEditWriteResp）：写回成功后的新基线 hash。 */
export interface FileEditWriteResult {
  baseHash: string;
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

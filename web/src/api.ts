import type { PaletteConfig } from './palette-focus';
import type {
  ActiveSessionItem,
  AIConfig,
  Annotation,
  AnnotationCreateInput,
  AnnotationsListResponse,
  EnvResponse,
  FileEditRead,
  FileEditWriteInput,
  FileEditWriteResult,
  GitDiffResult,
  GitStatus,
  GlobalEnvMode,
  GlobalEnvResponse,
  HostEnvResponse,
  LifecycleConfig,
  NotificationConfig,
  NotificationConfigPut,
  NotificationTestResult,
  OcConfigContent,
  OcConfigInfo,
  OcConfigSaveResult,
  Project,
  ProjectDetail,
  ProjectKind,
  ServerStatus,
  Submission,
  SubmissionAnnotationRef,
  SubmissionsListResponse,
  Task,
  TaskMode,
  TaskPermissionMode,
  TerminalInfo,
} from './types';

const TOKEN_KEY = 'ocdeck.token';
export const UNAUTHORIZED_EVENT = 'ocdeck:unauthorized';

/** 上传接口 404 固定提示（terminal-file-paste-drop：业务 404——任务已删除——与旧 server
 *  无该路由不可区分，前端按此固定文案反馈，仅终止当前上传项、不永久降级）。 */
export const UPLOAD_TARGET_UNAVAILABLE_MESSAGE = '上传目标不可用，任务可能已删除或服务端不支持该接口';

/** 文件读取失效类别（FileReader/FormData 读取路径的 DOMException 名）：
 *  size 可访问不代表读取路径可用，这类失败重试同样读取无效，须重新选择文件。
 *  uploadAttachment 的 fetch 包装据此保留原始错误（不吞成普通网络错误），
 *  文件投递状态机据此转入"重新选择文件"重试路径。 */
const FILE_READ_ERROR_NAMES = new Set(['NotAllowedError', 'NotFoundError', 'NotReadableError', 'SecurityError']);

export function isFileReadFailure(err: unknown): boolean {
  const name = (err as { name?: unknown } | null)?.name;
  return typeof name === 'string' && FILE_READ_ERROR_NAMES.has(name);
}

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? '';
}

export function setToken(token: string): void {
  localStorage.setItem(TOKEN_KEY, token);
}

export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY);
}

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  query?: Record<string, string>,
): Promise<T> {
  let url = `/api/v1${path}`;
  if (query) {
    const qs = new URLSearchParams(query).toString();
    if (qs) url += `?${qs}`;
  }
  const headers: Record<string, string> = {
    Authorization: `Bearer ${getToken()}`,
  };
  if (body !== undefined) headers['Content-Type'] = 'application/json';

  let res: Response;
  try {
    res = await fetch(url, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
  } catch {
    throw new ApiError(0, 'network_error', '无法连接服务端（ocdeck-server 未运行？）');
  }

  if (res.status === 401) {
    clearToken();
    window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
    throw new ApiError(401, 'unauthorized', '认证失败，请重新输入 token');
  }
  if (res.status === 204) return undefined as T;

  const text = await res.text();
  let data: unknown = undefined;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      /* 非 JSON 响应 */
    }
  }
  if (!res.ok) {
    const errObj = (data as { error?: { code?: string; message?: string } } | undefined)?.error;
    throw new ApiError(
      res.status,
      errObj?.code ?? 'unknown',
      errObj?.message ?? `请求失败（HTTP ${res.status}）`,
    );
  }
  return data as T;
}

/** WS 同源地址（dev 走 vite proxy，prod 同源）。 */
export function wsURL(path: string): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${location.host}${path}`;
}

/** text/plain 端点（init-log / pre-delete-log）：空 body 返回空串。 */
async function requestText(method: string, path: string): Promise<string> {
  let res: Response;
  try {
    res = await fetch(`/api/v1${path}`, {
      method,
      headers: { Authorization: `Bearer ${getToken()}` },
    });
  } catch {
    throw new ApiError(0, 'network_error', '无法连接服务端（ocdeck-server 未运行？）');
  }
  if (res.status === 401) {
    clearToken();
    window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
    throw new ApiError(401, 'unauthorized', '认证失败，请重新输入 token');
  }
  const text = await res.text();
  if (!res.ok) {
    let code = 'unknown';
    let message = `请求失败（HTTP ${res.status}）`;
    try {
      const errObj = (JSON.parse(text) as { error?: { code?: string; message?: string } }).error;
      if (errObj?.code) code = errObj.code;
      if (errObj?.message) message = errObj.message;
    } catch {
      /* 非 JSON 错误体 */
    }
    throw new ApiError(res.status, code, message);
  }
  return text;
}

export const api = {
  listProjects: () => request<Project[]>('GET', '/projects'),
  /** kind 缺省 repo；dir 仅校验路径存在（add-plain-dir-project D1/D6）。 */
  createProject: (name: string, path: string, kind: ProjectKind = 'repo') =>
    request<Project>('POST', '/projects', { name, path, kind }),
  getProject: (id: string) => request<ProjectDetail>('GET', `/projects/${id}`),
  deleteProject: (id: string) => request<void>('DELETE', `/projects/${id}`),
  /** 分支短名列表（D10）：本地在前、远端在后；dir 项目返回错误（前端不调用）。 */
  listBranches: (projectID: string) =>
    request<string[]>('GET', `/projects/${projectID}/branches`),
  /** 远端刷新（D10）：git fetch 后返回同构短名数组；秒级延迟，失败 git_error。 */
  refreshBranches: (projectID: string) =>
    request<string[]>('POST', `/projects/${projectID}/branches/refresh`),

  listTasks: (projectID: string) => request<Task[]>('GET', `/projects/${projectID}/tasks`),
  /** baseRef 仅 repo 项目 worktree 模式可选（短名，空 = 项目默认分支）；dir 项目不提供。
   *  mode 仅 repo 项目 local-path 模式显式传 'local-path'（此时 MUST NOT 携带 base_ref）；
   *  worktree/dir 不传（缺省语义，add-local-path-task-mode D6 presence 契约）。
   *  permissionMode 仅非缺省时传（task-permission-mode D2 presence 契约）：缺省 ask 不携带该字段。 */
  createTask: (projectID: string, name: string, baseRef?: string, mode?: TaskMode, permissionMode?: TaskPermissionMode) =>
    request<Task>('POST', `/projects/${projectID}/tasks`, {
      name,
      ...(baseRef ? { base_ref: baseRef } : {}),
      ...(mode ? { mode } : {}),
      ...(permissionMode ? { permission_mode: permissionMode } : {}),
    }),
  getTask: (id: string) => request<Task>('GET', `/tasks/${id}`),
  taskAction: (id: string, action: 'activate' | 'suspend' | 'archive' | 'restore' | 'retry') =>
    request<void>('POST', `/tasks/${id}/${action}`),
  /** 带 confirmDirty 的重试：deletion_failed 且 worktree dirty 时 409 拒绝后需显式确认。 */
  retryTask: (id: string, confirmDirty: boolean) =>
    request<void>('POST', `/tasks/${id}/retry`, undefined, {
      ...(confirmDirty ? { confirmDirty: 'true' } : {}),
    }),
  deleteTask: (id: string, mode: 'normal' | 'force', confirmDirty: boolean) =>
    request<void>('DELETE', `/tasks/${id}`, undefined, {
      mode,
      ...(confirmDirty ? { confirmDirty: 'true' } : {}),
    }),
  /** 用户主动操作上报（idle-reminder-user-activity D4）：fire-and-forget——忽略响应与错误
   *  （含 401，不触发登出事件），不等待、不重试，MUST NOT 阻塞交互。 */
  reportActivity: (taskID: string): void => {
    void fetch(`/api/v1/tasks/${taskID}/activity`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${getToken()}` },
    }).catch(() => {
      /* 上报失败静默忽略 */
    });
  },

  listTerminals: (taskID: string) => request<TerminalInfo[]>('GET', `/tasks/${taskID}/terminals`),
  createTerminal: (taskID: string) =>
    request<TerminalInfo>('POST', `/tasks/${taskID}/terminals`),
  closeTerminal: (tid: string) => request<void>('DELETE', `/terminals/${tid}`),

  /** 文件上传（terminal-file-paste-drop D1）：multipart FormData 顺序固定——
   *  首 part MUST 为 connId 文本字段，随后唯一 file part；不手动设置 Content-Type
   *  （multipart boundary 由浏览器生成）。201 返回服务端签发的 32hex uploadId。 */
  uploadAttachment: async (taskID: string, file: File, connId: string): Promise<{ uploadId: string }> => {
    const form = new FormData();
    form.append('connId', connId); // 顺序契约：先 connId 后 file
    form.append('file', file);
    let res: Response;
    try {
      res = await fetch(`/api/v1/tasks/${taskID}/attachments`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${getToken()}` },
        body: form,
      });
    } catch (err) {
      // FormData 序列化时才真正读取 File：文件读取失效（如原 File 已失效的
      // NotReadableError）在此以 fetch 拒绝浮出——原样上抛保留 name，供投递
      // 状态机转入"重新选择文件"路径；其余拒绝归普通网络错误。
      if (isFileReadFailure(err)) throw err;
      throw new ApiError(0, 'network_error', '无法连接服务端（ocdeck-server 未运行？）');
    }
    if (res.status === 401) {
      clearToken();
      window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
      throw new ApiError(401, 'unauthorized', '认证失败，请重新输入 token');
    }
    const text = await res.text();
    let data: unknown = undefined;
    if (text) {
      try {
        data = JSON.parse(text);
      } catch {
        /* 非 JSON 响应 */
      }
    }
    if (!res.ok) {
      const errObj = (data as { error?: { code?: string; message?: string } } | undefined)?.error;
      if (res.status === 404) {
        throw new ApiError(404, errObj?.code ?? 'not_found', UPLOAD_TARGET_UNAVAILABLE_MESSAGE);
      }
      throw new ApiError(
        res.status,
        errObj?.code ?? 'unknown',
        errObj?.message ?? `请求失败（HTTP ${res.status}）`,
      );
    }
    return data as { uploadId: string };
  },

  serverStatus: () => request<ServerStatus>('GET', '/server/status'),

  /** 跨项目活跃会话列表（design.md D3）：快照语义，agentStatus 不可用时缺省。 */
  listActiveSessions: () => request<ActiveSessionItem[]>('GET', '/tasks/active'),

  gitStatus: (taskID: string) => request<GitStatus>('GET', `/tasks/${taskID}/git/status`),
  /** 单文件两侧版本内容（八字段契约，见 GitDiffResult）；path 恒非空（空 path 由服务端拒绝）。 */
  gitDiff: (taskID: string, ref: string, path: string, untracked: boolean) =>
    request<GitDiffResult>('GET', `/tasks/${taskID}/git/diff`, undefined, {
      ...(ref ? { ref } : {}),
      ...(path ? { path } : {}),
      ...(untracked ? { untracked: '1' } : {}),
    }),
  /** paths 为空数组 = 提交全部改动。 */
  gitCommit: (taskID: string, message: string, paths: string[]) =>
    request<void>('POST', `/tasks/${taskID}/git/commit`, { message, paths }),
  gitPush: (taskID: string) => request<void>('POST', `/tasks/${taskID}/git/push`),

  /** 批注列表 + 提交能力（diff-review-workbench D8）；submitCapability.state 非 supported 禁用提交。 */
  listAnnotations: (taskID: string) =>
    request<AnnotationsListResponse>('GET', `/tasks/${taskID}/annotations`),
  /** 创建批注：1-based 闭区间；快照须构造自原始 GitDiffResult 侧内容（含行尾字符）。 */
  createAnnotation: (taskID: string, body: AnnotationCreateInput) =>
    request<Annotation>('POST', `/tasks/${taskID}/annotations`, body),
  /** 编辑评论：空白拒绝；与原值相同返回原 DTO 且 revision 不变。 */
  updateAnnotationComment: (taskID: string, annotationID: string, comment: string) =>
    request<Annotation>('PATCH', `/tasks/${taskID}/annotations/${annotationID}`, { comment }),
  deleteAnnotation: (taskID: string, annotationID: string) =>
    request<void>('DELETE', `/tasks/${taskID}/annotations/${annotationID}`),

  /** 提交批注：条目携带 id+revision（与请求时一致，复核失败 409）。 */
  createSubmission: (taskID: string, annotations: SubmissionAnnotationRef[], note: string) =>
    request<Submission>('POST', `/tasks/${taskID}/annotation-submissions`, { annotations, note }),
  /** 分区列表：queue=queued/sending、history=sent、failures=failed/delivery_unknown。 */
  listSubmissions: (taskID: string) =>
    request<SubmissionsListResponse>('GET', `/tasks/${taskID}/annotation-submissions`),
  /** 撤回：仅 queued（非 queued invalid_state）。 */
  cancelSubmission: (taskID: string, submissionID: string) =>
    request<void>('POST', `/tasks/${taskID}/annotation-submissions/${submissionID}/cancel`),
  /** 删除历史：仅终态 sent/failed/delivery_unknown（非终态 invalid_state）。 */
  deleteSubmission: (taskID: string, submissionID: string) =>
    request<void>('DELETE', `/tasks/${taskID}/annotation-submissions/${submissionID}`),

  /** 文件编辑读取判别联合：editable=true 时 content/baseHash/lineEnding/hasBom/mode；
   *  editable=false 时 reasonCode 七值枚举 + reason。 */
  gitFileRead: (taskID: string, path: string) =>
    request<FileEditRead>('GET', `/tasks/${taskID}/git/file`, undefined, { path }),
  /** 文件编辑写回：hash/换行风格/mode 不一致 409 零写盘；成功返回新 baseHash。 */
  gitFileWrite: (taskID: string, body: FileEditWriteInput) =>
    request<FileEditWriteResult>('POST', `/tasks/${taskID}/git/file`, body),

  /** base 为 `/projects/:id/env` 或 `/tasks/:id/env`，二者同构。 */
  getEnv: (base: string) => request<EnvResponse>('GET', base),
  putEnv: (base: string, key: string, value: string) =>
    request<EnvResponse>('PUT', base, { key, value }),
  deleteEnv: (base: string, key: string) =>
    request<EnvResponse>('DELETE', `${base}/${encodeURIComponent(key)}`),

  /** 项目生命周期配置（project-lifecycle-config design.md §8/§9）：PUT 整体替换。 */
  getLifecycleConfig: (projectID: string) =>
    request<LifecycleConfig>('GET', `/projects/${projectID}/lifecycle-config`),
  putLifecycleConfig: (projectID: string, config: LifecycleConfig) =>
    request<LifecycleConfig>('PUT', `/projects/${projectID}/lifecycle-config`, config),
  /** 成功返回最新任务 DTO（异步执行已登记，非同步完成）。 */
  rerunInit: (taskID: string) => request<Task>('POST', `/tasks/${taskID}/rerun-init`),
  /** text/plain：inherit 警告节 + init.log；无日志返回空串。 */
  getInitLog: (taskID: string) => requestText('GET', `/tasks/${taskID}/init-log`),
  getPreDeleteLog: (taskID: string) => requestText('GET', `/tasks/${taskID}/pre-delete-log`),

  /** 全局级 env（design.md 2.9）：follow_host 时 value 可空。保留 key 由服务端 422 拒绝。 */
  getGlobalEnv: () => request<GlobalEnvResponse>('GET', '/env'),
  putGlobalEnv: (key: string, mode: GlobalEnvMode, value: string) =>
    request<GlobalEnvResponse>('PUT', '/env', { key, mode, value }),
  deleteGlobalEnv: (key: string) =>
    request<GlobalEnvResponse>('DELETE', `/env/${encodeURIComponent(key)}`),

  /** 宿主环境变量合并视图（host-env-sync-and-display D4）：首次调用可能触发 login shell 捕获，
   *  前端需 loading 态；捕获失败时降级为仅进程环境视图（正常返回）。 */
  getHostEnv: () => request<HostEnvResponse>('GET', '/env/host'),
  /** 显式刷新宿主捕获缓存；成功返回最新合并视图，失败 500 code=internal（原视图由前端保留）。 */
  refreshHostEnv: () => request<HostEnvResponse>('POST', '/env/host/refresh'),

  listOcConfigs: () => request<{ configs: OcConfigInfo[] }>('GET', '/oc-configs'),
  getOcConfig: (name: string) =>
    request<OcConfigContent>('GET', `/oc-configs/${encodeURIComponent(name)}`),
  putOcConfig: (name: string, content: string, mtime: number, hash: string) =>
    request<OcConfigSaveResult>('PUT', `/oc-configs/${encodeURIComponent(name)}`, {
      content,
      mtime,
      hash,
    }),

  /** 全局 AI provider 配置（ai-worktree-naming design.md D6）。
   *  PUT 的 api_key 传掩码值（含 ***）或空串 = 保留原 key。 */
  getAIConfig: () => request<AIConfig>('GET', '/ai/config'),
  saveAIConfig: (body: {
    provider: string;
    api_key: string;
    base_url: string;
    model: string;
    thinking: string;
  }) => request<AIConfig>('PUT', '/ai/config', body),

  /** 通知配置（task-notifications）：GET 返回 token_masked 形态；
   *  PUT 的 token 传空串或含 *** 的掩码值 = 保留已存储原 token（服务端语义）。 */
  getNotificationConfig: () => request<NotificationConfig>('GET', '/notification/config'),
  saveNotificationConfig: (body: NotificationConfigPut) =>
    request<NotificationConfig>('PUT', '/notification/config', body),
  /** 测试通知：向全部已启用且已配置渠道投递，逐渠道报告结果。总开关关闭时 422。 */
  testNotification: () => request<{ results: NotificationTestResult[] }>('POST', '/notification/test'),

  /** 命令面板配置：GET/PUT 均为 camelCase 三键 {hotkey, triggerWord, matchMode}。 */
  getPaletteConfig: () => request<PaletteConfig>('GET', '/palette/config'),
  putPaletteConfig: (body: PaletteConfig) => request<PaletteConfig>('PUT', '/palette/config', body),
};

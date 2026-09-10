/**
 * 常用工具快捷打开偏好（add-frontend-tool-quick-open design.md D1/D3）：
 * localStorage 持久化 + 编辑器唤起 URI 构造，纯函数可测（参照 terminal/preferences.ts 模式）。
 * 读取侧遇到损坏/非法数据只回退默认，MUST NOT 改写 localStorage。
 * 写入侧 setItem 成功才派发 EDITOR_TOOLS_CHANGED，失败向上抛出、不派发。
 */

export type EditorTool = 'vscode' | 'goland' | 'cursor';

export interface EditorTools {
  vscode: boolean;
  goland: boolean;
  cursor: boolean;
}

/** 自定义编辑器工具（用户自配「名称 + 唤起 URI 模板」，存储为 JSON 数组文本）。 */
export interface CustomEditorTool {
  id: string;
  name: string;
  template: string;
}

export const VSCODE_KEY = 'ocdeck.editorTools.vscode';
export const GOLAND_KEY = 'ocdeck.editorTools.goland';
export const CURSOR_KEY = 'ocdeck.editorTools.cursor';
/** 默认工具键唯一写入口 = saveDefaultTool（仅快捷打开下拉显式选择调用）。 */
export const DEFAULT_KEY = 'ocdeck.editorTools.defaultTool';
/** 自定义唤起 URI 模板键（可选，spec「自定义唤起 URI 模板」）。 */
export const VSCODE_URI_TEMPLATE_KEY = 'ocdeck.editorTools.uriTemplate.vscode';
export const GOLAND_URI_TEMPLATE_KEY = 'ocdeck.editorTools.uriTemplate.goland';
export const CURSOR_URI_TEMPLATE_KEY = 'ocdeck.editorTools.uriTemplate.cursor';
/** 自定义工具列表键：JSON 数组文本（CustomEditorTool[]）。 */
export const CUSTOM_TOOLS_KEY = 'ocdeck.editorTools.customTools';
export const EDITOR_TOOLS_CHANGED = 'ocdeck-editor-tools-changed'; // window CustomEvent 名

/** 默认编辑器回退顺序（扩展后：VSCode → GoLand → Cursor 取首个已启用；自定义工具殿后）。 */
const TOOL_ORDER: readonly EditorTool[] = ['vscode', 'goland', 'cursor'];
const CUSTOM_PREFIX = 'custom:';

/** 各工具允许的模板 scheme 前缀（白名单同时是安全边界：防 javascript: 等危险 scheme 经 assign 执行）。
 *  cursor:// 为 Cursor 注册的 scheme（VSCode fork；无官方文档，按社区通行行为，模板字段可覆盖）。 */
const TOOL_TEMPLATE_SCHEMES: Record<EditorTool, readonly string[]> = {
  vscode: ['vscode://', 'vscode-insiders://'],
  goland: ['goland://', 'jetbrains://'],
  cursor: ['cursor://'],
};

const TOOL_KEYS: Record<EditorTool, string> = {
  vscode: VSCODE_KEY,
  goland: GOLAND_KEY,
  cursor: CURSOR_KEY,
};

const TOOL_URI_TEMPLATE_KEYS: Record<EditorTool, string> = {
  vscode: VSCODE_URI_TEMPLATE_KEY,
  goland: GOLAND_URI_TEMPLATE_KEY,
  cursor: CURSOR_URI_TEMPLATE_KEY,
};

/** 读取三个内置工具开关；仅 '1' 视为开启，其余/缺省/读取异常按关闭处理，不改写 localStorage。 */
export function loadEditorTools(): EditorTools {
  const tools: EditorTools = { vscode: false, goland: false, cursor: false };
  for (const tool of TOOL_ORDER) {
    try {
      tools[tool] = localStorage.getItem(TOOL_KEYS[tool]) === '1';
    } catch {
      /* localStorage 不可用时按关闭处理 */
    }
  }
  return tools;
}

/** 保存开关：开写 '1' 关写 '0'；只写对应开关键，MUST NOT 触碰 DEFAULT_KEY（默认回退是派生状态）。失败向上抛出、不派发。 */
export function saveEditorTool(tool: EditorTool, enabled: boolean): void {
  localStorage.setItem(TOOL_KEYS[tool], enabled ? '1' : '0');
  window.dispatchEvent(new CustomEvent(EDITOR_TOOLS_CHANGED));
}

/**
 * 读取存储默认值：合法值 = 内置名（'vscode'/'goland'/'cursor'）或 'custom:<id>'（id 非空）。
 * 非法值/读取异常返回 null 按无记录处理，不改写 localStorage、不抛至渲染层。
 */
export function loadDefaultTool(): string | null {
  try {
    const raw = localStorage.getItem(DEFAULT_KEY);
    if (
      raw !== null &&
      (raw === 'vscode' || raw === 'goland' || raw === 'cursor' || (raw.startsWith(CUSTOM_PREFIX) && raw.length > CUSTOM_PREFIX.length))
    ) {
      return raw;
    }
  } catch {
    /* localStorage 不可用时按无记录处理 */
  }
  return null;
}

/** 写默认工具（DEFAULT_KEY 唯一写入口，内置名或 custom:<id>）：setItem 成功才派发，失败向上抛出、不派发。 */
export function saveDefaultTool(tool: string): void {
  localStorage.setItem(DEFAULT_KEY, tool);
  window.dispatchEvent(new CustomEvent(EDITOR_TOOLS_CHANGED));
}

/** 解析后的工具引用：内置工具或自定义工具。 */
export type ResolvedTool =
  | { kind: 'builtin'; tool: EditorTool }
  | { kind: 'custom'; tool: CustomEditorTool };

/**
 * 默认编辑器解析（纯函数）：stored 合法且存在（内置已启用 / custom id 存在且可用）→ stored；
 * 否则按 vscode → goland → cursor 取首个已启用；再取首个可用自定义工具（存储顺序）；无则 null。
 * 不可用自定义工具（空名/模板非法）不参与解析（C1 统一谓词）。
 */
export function resolveDefaultTool(
  tools: EditorTools,
  customTools: CustomEditorTool[],
  stored: string | null,
): ResolvedTool | null {
  if (stored !== null) {
    if (stored === 'vscode' || stored === 'goland' || stored === 'cursor') {
      if (tools[stored]) return { kind: 'builtin', tool: stored };
    } else if (stored.startsWith(CUSTOM_PREFIX)) {
      const id = stored.slice(CUSTOM_PREFIX.length);
      const custom = customTools.find((t) => t.id === id && isUsableCustomTool(t));
      if (custom) return { kind: 'custom', tool: custom };
    }
  }
  for (const tool of TOOL_ORDER) {
    if (tools[tool]) return { kind: 'builtin', tool };
  }
  const firstCustom = customTools.find((t) => isUsableCustomTool(t));
  return firstCustom ? { kind: 'custom', tool: firstCustom } : null;
}

/** 读取自定义工具列表：解析失败/非数组/元素缺字段 → 跳过或回退空数组，不改写 localStorage。 */
export function loadCustomEditorTools(): CustomEditorTool[] {
  try {
    const raw = localStorage.getItem(CUSTOM_TOOLS_KEY);
    if (raw === null) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    const tools: CustomEditorTool[] = [];
    for (const item of parsed) {
      if (typeof item !== 'object' || item === null) continue;
      const { id, name, template } = item as Record<string, unknown>;
      if (typeof id !== 'string' || id === '') continue;
      if (typeof name !== 'string' || typeof template !== 'string') continue;
      tools.push({ id, name, template });
    }
    return tools;
  } catch {
    /* JSON 损坏/localStorage 不可用：整项回空数组，不改写存储 */
    return [];
  }
}

/** 保存自定义工具列表：写 JSON 数组文本；成功才派发，失败向上抛出、不派发。 */
export function saveCustomEditorTools(tools: CustomEditorTool[]): void {
  localStorage.setItem(CUSTOM_TOOLS_KEY, JSON.stringify(tools));
  window.dispatchEvent(new CustomEvent(EDITOR_TOOLS_CHANGED));
}

/**
 * 模板合法性判定（spec「自定义唤起 URI 模板」）：含至少一个 {path} 占位符，
 * 且以该工具允许的 scheme 前缀开头（vscode: vscode:// / vscode-insiders://，goland: goland:// / jetbrains://）。
 */
export function isValidUriTemplate(tool: EditorTool, template: string): boolean {
  return template.includes('{path}') && TOOL_TEMPLATE_SCHEMES[tool].some((s) => template.startsWith(s));
}

/** 读取自定义模板：缺省/空串/读取异常返回 null 按未设置处理，不改写 localStorage、不抛至渲染层。 */
export function loadEditorUriTemplate(tool: EditorTool): string | null {
  try {
    const raw = localStorage.getItem(TOOL_URI_TEMPLATE_KEYS[tool]);
    if (raw !== null && raw !== '') return raw;
  } catch {
    /* localStorage 不可用时按未设置处理 */
  }
  return null;
}

/** 保存模板：非空写原文、空串清除（removeItem）；成功才派发，失败向上抛出、不派发。不校验合法性（非法模板读取时按未设置回退）。 */
export function saveEditorUriTemplate(tool: EditorTool, template: string): void {
  if (template === '') {
    localStorage.removeItem(TOOL_URI_TEMPLATE_KEYS[tool]);
  } else {
    localStorage.setItem(TOOL_URI_TEMPLATE_KEYS[tool], template);
  }
  window.dispatchEvent(new CustomEvent(EDITOR_TOOLS_CHANGED));
}

/**
 * 模板路径注入值（design D7）：归一化路径按 / 分段逐段编码（Windows 盘符首段冒号保留字面）、
 * 以 / 连接、保留开头 /、不做尾斜杠收敛（与内置 VSCode 分支的差异仅在首尾斜杠处理）。
 */
function encodeTemplatePath(normalized: string): string {
  return normalized
    .split('/')
    .map((seg, i) => (i === 0 && /^[A-Za-z]:$/.test(seg) ? seg : encodeURIComponent(seg)))
    .join('/');
}

/**
 * 自定义工具模板合法性：MUST 含 {path}，scheme 匹配通用 URI scheme 语法
 * （`^[a-zA-Z][a-zA-Z0-9+.-]*:`）且不在危险名单（javascript:/data:/vbscript:/file:，大小写不敏感）。
 * 不合法视为不可用（存储保留原文，唤起时按不可用处理，与内置「非法模板回退」同哲学）。
 */
const CUSTOM_TEMPLATE_SCHEME_RE = /^[a-zA-Z][a-zA-Z0-9+.-]*:/;
const CUSTOM_TEMPLATE_FORBIDDEN_SCHEMES = ['javascript:', 'data:', 'vbscript:', 'file:'];

export function isValidCustomUriTemplate(template: string): boolean {
  if (!template.includes('{path}')) return false;
  const match = CUSTOM_TEMPLATE_SCHEME_RE.exec(template);
  if (match === null) return false;
  const scheme = match[0].toLowerCase();
  return !CUSTOM_TEMPLATE_FORBIDDEN_SCHEMES.includes(scheme);
}

/** 自定义工具可用性唯一谓词（C1）：名称 trim 非空 且 模板合法。
 *  菜单展示、默认解析、唤起点击时校验共用；存储保留原文不变。 */
export function isUsableCustomTool(tool: CustomEditorTool): boolean {
  return tool.name.trim() !== '' && isValidCustomUriTemplate(tool.template);
}

/**
 * 自定义工具唤起 URI 构造：合法模板 → 全部 {path} 替换为注入值（与内置模板注入同一
 * encodeTemplatePath，语义一致：反斜杠归一、分段编码、盘符冒号保留、保留开头 /）；
 * 非法模板或 path 为空返回空串（自定义无内置回退，调用侧不触发唤起）。
 */
export function buildCustomEditorUri(tool: CustomEditorTool, path: string): string {
  if (path === '' || !isValidCustomUriTemplate(tool.template)) return '';
  const normalized = path.replace(/\\/g, '/');
  return tool.template.split('{path}').join(encodeTemplatePath(normalized));
}

/**
 * 编辑器唤起 URI 构造（design.md D3/D7，spec 逐字同一契约）：
 * template 合法（含 {path} 且 scheme 前缀属于该工具族）→ 全部 {path} 替换为注入值，
 * 不再对替换结果做任何编码；非法/缺省 → 内置分支（行为不变）：
 * 公共归一化仅反斜杠转 '/'；
 * VSCode/Cursor 分支同构：按 '/' 分段编码（盘符首段冒号保留字面），去开头 '/' 拼到
 * `<tool>://file/` 后，尾部 '/' 收敛为恰好一个（该收敛仅属于这两个分支）；
 * GoLand 分支：对归一化路径整体一次 encodeURIComponent（不复用/不叠加分段编码）；
 * path 为空返回空串（调用侧不触发唤起）。
 */
export function buildEditorUri(tool: EditorTool, path: string, template?: string | null): string {
  if (path === '') return '';
  const normalized = path.replace(/\\/g, '/');
  if (template != null && isValidUriTemplate(tool, template)) {
    return template.split('{path}').join(encodeTemplatePath(normalized));
  }
  if (tool === 'goland') return `goland://open?file=${encodeURIComponent(normalized)}`;
  const encoded = normalized
    .split('/')
    .map((seg, i) => (i === 0 && /^[A-Za-z]:$/.test(seg) ? seg : encodeURIComponent(seg)))
    .join('/');
  return `${tool}://file/${encoded.replace(/^\/+/, '').replace(/\/+$/, '')}/`;
}

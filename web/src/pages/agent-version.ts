import type { ServerStatus } from '../types';

/** 设置页「Agent 配置」子项的版本标识取值。 */
export type AgentVersionLabel = 'v1' | 'v2' | '未知';

/**
 * 从 GET /api/v1/server/status 判定 agent 版本标识：
 * 主版本 1 → 'v1'，2 → 'v2'，其余（空值/非数字主版本/其他主版本）→ '未知'。
 * 注：capabilities 快照空闲时 session 恒为 null，不含 provider 身份，
 * 不能作为版本信号；opencodeVersion 是检测到的 opencode CLI 版本，直接对应 v1/v2。
 *
 * 优先使用 status.opencodeMajor（后端 ExtractOpenCodeMajor 已按空白分词 +
 * 跳过非数字 token 解析，与 probe `opencode v2.1.4` 真实输出对齐）。
 * 旧服务端无该字段时回退解析 opencodeVersion：口径与 Go 端 ExtractOpenCodeMajor
 * 一致——按空白分词，跳过 stripping v/V 后首字符非数字的 token，取首个点分段
 * 前的 leading integer 作为主版本。
 */
export function detectAgentVersion(status: ServerStatus | null | undefined): AgentVersionLabel {
  if (status) {
    const major = status.opencodeMajor;
    if (typeof major === 'number' && Number.isFinite(major)) {
      if (major === 1) return 'v1';
      if (major === 2) return 'v2';
      return '未知';
    }
  }

  const raw = status?.opencodeVersion?.trim();
  if (!raw) return '未知';
  for (const tok of raw.split(/\s+/)) {
    let t = tok.replace(/^v/, '').replace(/^V/, '');
    if (t.length === 0 || t[0] < '0' || t[0] > '9') continue;
    const dot = t.indexOf('.');
    if (dot >= 0) t = t.slice(0, dot);
    // 对齐 Go strconv.Atoi：整段必须全为数字，否则 Atoi 失败 → 函数返回 0（未知），
    // 不继续尝试下一个 token。parseInt("2junk") 会宽松返回 2，与 Go 不一致，故用全数字校验。
    if (!/^[0-9]+$/.test(t)) return '未知';
    const major = Number(t);
    if (major <= 0) return '未知';
    if (major === 1) return 'v1';
    if (major === 2) return 'v2';
    return '未知';
  }
  return '未知';
}

/** 重启按钮可见性：仅 v2 有共享 serve 进程，v1 / 未知不展示。 */
export function shouldShowOpenCodeRestart(version: AgentVersionLabel): boolean {
  return version === 'v2';
}

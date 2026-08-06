/**
 * 统一输入门禁纯逻辑（design D5/D8 测试缝）。
 *
 * 独立模块不依赖 xterm/DOM，vitest 在 Node 无 DOM 可直接单测。
 * session.ts 重新导出，保持对外接口不变。
 */

/**
 * 统一输入门禁判定（纯函数）。
 *
 * 门禁规则（design D5）：
 * - 未认证或 WS 未 OPEN → 拒绝
 * - 锁定且非合成事件 → 拒绝（锁定期只有合成手势产生的字节放行）
 * - 其余放行
 */
export function shouldSendInput(state: {
  authed: boolean;
  wsOpen: boolean;
  locked: boolean;
  syntheticInFlight: boolean;
}): boolean {
  if (!state.authed || !state.wsOpen) return false;
  if (state.locked && !state.syntheticInFlight) return false;
  return true;
}

/**
 * onBinary 字节编码（纯函数）。
 *
 * 按 charCode 取原始字节，MUST NOT 用 TextEncoder（会破坏 >127 的编码字节）。
 */
export function encodeBinaryInput(d: string): Uint8Array {
  const bytes = new Uint8Array(d.length);
  for (let i = 0; i < d.length; i++) bytes[i] = d.charCodeAt(i) & 0xff;
  return bytes;
}

/**
 * 合成事件同步标记门禁（design D4/D5）：嵌套计数 + try/finally。
 *
 * 回调执行期间 inFlight()=true → 门禁放行合成手势产生的字节；
 * 异常时 finally 复位计数，不得永久放开门禁。
 * 纯逻辑、不依赖 DOM/xterm，vitest 可直接单测。
 */
export interface SyntheticGate {
  /** 包裹 fn：执行期间 inFlight=true，异常时 finally 复位。 */
  markSynthetic<T>(fn: () => T): T;
  /** 当前是否处于合成事件回调栈内（门禁放行判定依据）。 */
  inFlight(): boolean;
}

export function createSyntheticGate(): SyntheticGate {
  let depth = 0;
  return {
    markSynthetic<T>(fn: () => T): T {
      depth++;
      try {
        return fn();
      } finally {
        depth--;
      }
    },
    inFlight(): boolean {
      return depth > 0;
    },
  };
}
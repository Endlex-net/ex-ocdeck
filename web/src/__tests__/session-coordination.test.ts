import { describe, expect, it, vi } from 'vitest';
import { createSyntheticGate, shouldSendInput, encodeBinaryInput } from '../terminal/input-gate';
import { createLockOrchestrator, type LockOrchestratorDeps } from '../terminal/session-coordination';

describe('createSyntheticGate（markSynthetic 嵌套计数 + try/finally）', () => {
  it('回调执行期间 inFlight=true，结束后复位', () => {
    const gate = createSyntheticGate();
    expect(gate.inFlight()).toBe(false);
    gate.markSynthetic(() => {
      expect(gate.inFlight()).toBe(true);
    });
    expect(gate.inFlight()).toBe(false);
  });

  it('嵌套调用深度计数正确', () => {
    const gate = createSyntheticGate();
    gate.markSynthetic(() => {
      expect(gate.inFlight()).toBe(true); // depth=1
      gate.markSynthetic(() => {
        expect(gate.inFlight()).toBe(true); // depth=2
        gate.markSynthetic(() => {
          expect(gate.inFlight()).toBe(true); // depth=3
        });
        expect(gate.inFlight()).toBe(true); // depth=2
      });
      expect(gate.inFlight()).toBe(true); // depth=1
    });
    expect(gate.inFlight()).toBe(false); // depth=0
  });

  it('回调抛异常时 finally 复位计数（不得永久放开门禁）', () => {
    const gate = createSyntheticGate();
    expect(() =>
      gate.markSynthetic(() => {
        throw new Error('boom');
      }),
    ).toThrow('boom');
    // 异常后门禁必须关闭
    expect(gate.inFlight()).toBe(false);
  });

  it('嵌套调用中内层抛异常 → 外层 finally 仍复位', () => {
    const gate = createSyntheticGate();
    expect(() => {
      gate.markSynthetic(() => {
        try {
          gate.markSynthetic(() => {
            throw new Error('inner');
          });
        } catch {
          /* swallow */
        }
        // 内层异常后 inFlight 应仍为 true（外层未退出）
        expect(gate.inFlight()).toBe(true);
      });
    }).not.toThrow();
    expect(gate.inFlight()).toBe(false);
  });

  it('门禁与 gate 联动：inFlight 时锁定态放行', () => {
    const gate = createSyntheticGate();
    // 锁定 + 非合成 → 拒绝
    expect(shouldSendInput({ authed: true, wsOpen: true, locked: true, syntheticInFlight: gate.inFlight() })).toBe(false);
    gate.markSynthetic(() => {
      // 锁定 + 合成中 → 放行
      expect(shouldSendInput({ authed: true, wsOpen: true, locked: true, syntheticInFlight: gate.inFlight() })).toBe(true);
    });
    // 退出后恢复拒绝
    expect(shouldSendInput({ authed: true, wsOpen: true, locked: true, syntheticInFlight: gate.inFlight() })).toBe(false);
  });

  it('markSynthetic 返回值透传', () => {
    const gate = createSyntheticGate();
    const result = gate.markSynthetic(() => 42);
    expect(result).toBe(42);
  });
});

/** 构造 spy deps，记录调用顺序。 */
function spyDeps(): LockOrchestratorDeps & { calls: string[] } {
  const calls: string[] = [];
  return {
    calls,
    lock: vi.fn(() => calls.push('lock')),
    blur: vi.fn(() => calls.push('blur')),
    unlockSilently: vi.fn(() => calls.push('unlockSilently')),
    focus: vi.fn(() => calls.push('focus')),
    attachGestures: vi.fn(() => calls.push('attachGestures')),
    detachGestures: vi.fn(() => calls.push('detachGestures')),
  };
}

describe('createLockOrchestrator（session 状态机协调）', () => {
  it('lock()：先门禁置位（lock）再 blur，防 \x1b[O focus-loss 序列在门禁生效前泄漏', () => {
    const deps = spyDeps();
    const o = createLockOrchestrator(deps);
    o.lock();
    expect(deps.calls).toEqual(['lock', 'blur']);
    // 反证：blur 不能在 lock 之前
    expect(deps.calls.indexOf('lock')).toBeLessThan(deps.calls.indexOf('blur'));
  });

  it('unlock()：移除锁（unlockSilently）再 focus（可信手势栈唤起键盘）', () => {
    const deps = spyDeps();
    const o = createLockOrchestrator(deps);
    o.unlock();
    expect(deps.calls).toEqual(['unlockSilently', 'focus']);
    expect(deps.calls.indexOf('unlockSilently')).toBeLessThan(deps.calls.indexOf('focus'));
  });

  it('onAuthOk(coarse=true)：lock 先于 authed/connected 暴露（onAuthed 回调）', () => {
    const deps = spyDeps();
    const o = createLockOrchestrator(deps);
    let lockCallsAtAuthed = -1;
    o.onAuthOk(true, () => {
      // 回调执行时门禁应已就绪：lock 已调用
      lockCallsAtAuthed = deps.calls.filter((c) => c === 'lock').length;
    });
    // lock + blur 在 authed 回调之前执行
    expect(deps.calls).toEqual(['lock', 'blur']);
    // 反证：onAuthed 回调执行时 lock 已被调用（>=1），证明 lock 先于 authed 暴露
    expect(lockCallsAtAuthed).toBeGreaterThanOrEqual(1);
  });

  it('onAuthOk(coarse=false)：不 lock，直接暴露 authed', () => {
    const deps = spyDeps();
    const o = createLockOrchestrator(deps);
    const authedCalls: string[] = [];
    o.onAuthOk(false, () => authedCalls.push('authed'));
    expect(deps.calls).toEqual([]); // 非 coarse 不 lock/blur
    expect(authedCalls).toEqual(['authed']);
  });

  it('onPointerChange(true)：lock + blur + attach 手势层', () => {
    const deps = spyDeps();
    const o = createLockOrchestrator(deps);
    o.onPointerChange(true);
    expect(deps.calls).toEqual(['lock', 'blur', 'attachGestures']);
  });

  it('onPointerChange(false)：detach 手势层 + unlockSilently，MUST NOT focus', () => {
    const deps = spyDeps();
    const o = createLockOrchestrator(deps);
    o.onPointerChange(false);
    expect(deps.calls).toEqual(['detachGestures', 'unlockSilently']);
    // 反证：转 fine 不能调 focus（主动 focus 会注入 \x1b[I focus-in 序列）
    expect(deps.focus).not.toHaveBeenCalled();
  });

  it('dispose 后所有方法 no-op', () => {
    const deps = spyDeps();
    const o = createLockOrchestrator(deps);
    o.dispose();
    o.lock();
    o.unlock();
    o.onAuthOk(true, () => {});
    o.onPointerChange(true);
    expect(deps.calls).toEqual([]);
  });

  it('coarse attach/fine detach 累计调用：转 coarse attach、转 fine detach', () => {
    const deps = spyDeps();
    const o = createLockOrchestrator(deps);
    o.onPointerChange(true);
    o.onPointerChange(false);
    o.onPointerChange(true);
    expect(deps.attachGestures).toHaveBeenCalledTimes(2);
    expect(deps.detachGestures).toHaveBeenCalledTimes(1);
  });
});

describe('门禁纯函数矩阵（shouldSendInput + encodeBinaryInput）', () => {
  it('onData/onBinary 双出口矩阵：locked × synthetic × authed/wsOpen 组合', () => {
    // 全组合断言（覆盖 design D5 门禁语义）
    const matrix = [
      { authed: false, wsOpen: true, locked: false, syntheticInFlight: false, expect: false },
      { authed: true, wsOpen: false, locked: false, syntheticInFlight: false, expect: false },
      { authed: true, wsOpen: true, locked: true, syntheticInFlight: false, expect: false }, // 锁定+非合成拒绝
      { authed: true, wsOpen: true, locked: true, syntheticInFlight: true, expect: true }, // 锁定+合成放行
      { authed: true, wsOpen: true, locked: false, syntheticInFlight: false, expect: true },
      { authed: true, wsOpen: true, locked: false, syntheticInFlight: true, expect: true }, // 合成标志不影响解锁态
    ];
    for (const c of matrix) {
      expect(shouldSendInput(c)).toBe(c.expect);
    }
  });

  it('onBinary 字节按 charCode & 0xFF 原始值发送（>127 不被 UTF-8 破坏）', () => {
    // 模拟鼠标控制序列字节
    const d = String.fromCharCode(0xe1, 0x20, 0xff, 0x80);
    const bytes = encodeBinaryInput(d);
    expect(Array.from(bytes)).toEqual([0xe1, 0x20, 0xff, 0x80]);
    // 反证：若用 TextEncoder，>127 会被编码为多字节 UTF-8，长度 >4
    expect(new TextEncoder().encode(d).length).toBeGreaterThan(4);
  });
});
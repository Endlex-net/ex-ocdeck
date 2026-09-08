// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { act } from 'react';
import { mount, stubMatchMedia } from './cm-test-env';
import type * as FR from '../terminal/focus-request';
import type { TerminalView as TerminalViewType } from '../terminal/TerminalView';

/* ==================== 导航焦点请求消费方（fix-terminal-input-panel-resize D3 状态表+门禁） ====================
 * session mock 成假 TermSession（捕获 onState / focus），focus-request 单例为真实实现
 * （每用例 vi.resetModules 隔离单例）。覆盖：挂起等待 connected、订阅快照交付、
 * A 卸载不删 B 的新请求、锁定/输入区/过期取消、shell 实例不消费、重连不重新聚焦。 */

const sessionMock = vi.hoisted(() => {
  const instances: FakeTermSession[] = [];
  class FakeTermSession {
    static instances = instances;
    connect = vi.fn();
    disconnect = vi.fn();
    dispose = vi.fn();
    applyPreferences = vi.fn();
    active: unknown;
    focusCalls = 0;
    locked = false;
    private lockCbs = new Set<(v: boolean) => void>();
    private onStateCb: (s: string) => void;
    constructor(
      _host: HTMLElement,
      _wrap: HTMLElement,
      _wsPath: string,
      onState: (s: string) => void,
      _onClipboardWrite?: (text: string) => void,
    ) {
      this.onStateCb = onState;
      instances.push(this);
    }
    isLocked(): boolean {
      return this.locked;
    }
    onLockChange(cb: (v: boolean) => void): () => void {
      this.lockCbs.add(cb);
      return () => this.lockCbs.delete(cb);
    }
    focus(): void {
      this.focusCalls++;
    }
    /** 测试驱动：模拟连接状态迁移（真实由 WS 事件驱动） */
    emitState(s: string): void {
      this.onStateCb(s);
    }
    setLocked(v: boolean): void {
      this.locked = v;
      for (const cb of this.lockCbs) cb(v);
    }
  }
  return FakeTermSession;
});

vi.mock('../terminal/session', () => ({ TermSession: sessionMock }));

let TerminalView: typeof TerminalViewType;
let fr: typeof FR;

function lastSession(): InstanceType<typeof sessionMock> {
  return sessionMock.instances[sessionMock.instances.length - 1];
}

function pendingReq(): FR.TerminalFocusRequest | null {
  const got: FR.TerminalFocusRequest[] = [];
  fr.subscribeTerminalFocus((r) => got.push(r));
  return got[0] ?? null;
}

beforeEach(async () => {
  vi.resetModules();
  sessionMock.instances.length = 0;
  stubMatchMedia(false);
  fr = await import('../terminal/focus-request');
  ({ TerminalView } = await import('../terminal/TerminalView'));
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('TerminalView 焦点请求消费（TUI 实例）', () => {
  it('发布 B 请求 → A 卸载 → B 订阅（快照交付）→ B connected → 聚焦 B', () => {
    const a = mount(<TerminalView wsPath="/ws/terminal/t-a" active={false} />);
    // 请求发布时尚无 B 消费方：A 收到但 taskID 不匹配 → 暂不消费、保留
    act(() => fr.requestTerminalFocus('t-b'));
    expect(pendingReq()?.taskID).toBe('t-b');
    // A 卸载：MUST NOT 删除 pending 的 B 请求
    a.unmount();
    expect(pendingReq()?.taskID).toBe('t-b');

    // B 订阅：挂载即同步交付 pending 快照，未 connected → 挂起等待
    const b = mount(<TerminalView wsPath="/ws/terminal/t-b" active={false} />);
    expect(lastSession().focusCalls).toBe(0);
    // connected → 门禁通过（activeElement=body）→ 消费并聚焦
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(1);
    b.unmount();
    expect(pendingReq()).toBeNull();
  });

  it('就绪时已锁定 → 请求作废，不聚焦', () => {
    const { unmount } = mount(<TerminalView wsPath="/ws/terminal/t-b" active={false} />);
    act(() => fr.requestTerminalFocus('t-b'));
    act(() => lastSession().setLocked(true));
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(0);
    unmount();
    expect(pendingReq()).toBeNull();
  });

  it('等待期间用户进入输入区（focusin）→ 请求作废，connected 后不聚焦', () => {
    const { unmount } = mount(<TerminalView wsPath="/ws/terminal/t-b" active={false} />);
    act(() => fr.requestTerminalFocus('t-b'));
    const input = document.createElement('input');
    document.body.appendChild(input);
    input.dispatchEvent(new FocusEvent('focusin', { bubbles: true }));
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(0);
    unmount();
    expect(pendingReq()).toBeNull();
    input.remove();
  });

  it('connected 时 activeElement 为输入元素 → 门禁⑤拒绝，不聚焦', () => {
    const { unmount } = mount(<TerminalView wsPath="/ws/terminal/t-b" active={false} />);
    const input = document.createElement('input');
    document.body.appendChild(input);
    input.focus();
    act(() => fr.requestTerminalFocus('t-b'));
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(0);
    unmount();
    expect(pendingReq()).toBeNull();
    input.remove();
  });

  it('请求过期 → 不聚焦、作废', () => {
    const { unmount } = mount(<TerminalView wsPath="/ws/terminal/t-b" active={false} />);
    let req!: FR.TerminalFocusRequest;
    act(() => {
      req = fr.requestTerminalFocus('t-b');
    });
    vi.spyOn(Date, 'now').mockReturnValue(req.ts + fr.FOCUS_REQUEST_TTL_MS);
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(0);
    unmount();
    expect(pendingReq()).toBeNull();
  });

  it('路由不匹配暂不消费保留；同任务后续请求（seq 更新）→ 已 connected 立即聚焦', () => {
    const { unmount } = mount(<TerminalView wsPath="/ws/terminal/t-b" active={false} />);
    act(() => fr.requestTerminalFocus('t-a'));
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(0);
    // 其他任务的请求保留（未过期），等待其目标消费方
    expect(pendingReq()?.taskID).toBe('t-a');
    // 新请求覆盖（seq 更新）且匹配当前路由 → 已 connected，立即门禁后消费
    act(() => fr.requestTerminalFocus('t-b'));
    expect(lastSession().focusCalls).toBe(1);
    unmount();
    expect(pendingReq()).toBeNull();
  });

  it('shell 实例（/ws/terminal/shell/...）不订阅不消费', () => {
    const { unmount } = mount(<TerminalView wsPath="/ws/terminal/shell/t-a" active={false} />);
    act(() => fr.requestTerminalFocus('t-a'));
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(0);
    unmount();
    // 无任何消费方：请求保留
    expect(pendingReq()?.taskID).toBe('t-a');
  });

  it('重连不重新聚焦：消费一次后再次 connected 不再 focus', () => {
    const { unmount } = mount(<TerminalView wsPath="/ws/terminal/t-b" active={false} />);
    act(() => fr.requestTerminalFocus('t-b'));
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(1);
    // 普通重连：无新发布请求，状态迁移不产生二次聚焦
    act(() => lastSession().emitState('connecting'));
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(1);
    unmount();
    expect(pendingReq()).toBeNull();
  });

  it('reconnecting 期间的显式请求挂起等待本次 connected', () => {
    const { unmount } = mount(<TerminalView wsPath="/ws/terminal/t-b" active={false} />);
    act(() => lastSession().emitState('connecting'));
    act(() => lastSession().emitState('reconnecting'));
    // 重连中收到显式请求：挂起等待，不立即聚焦
    act(() => fr.requestTerminalFocus('t-b'));
    expect(lastSession().focusCalls).toBe(0);
    act(() => lastSession().emitState('connected'));
    expect(lastSession().focusCalls).toBe(1);
    unmount();
    expect(pendingReq()).toBeNull();
  });
});

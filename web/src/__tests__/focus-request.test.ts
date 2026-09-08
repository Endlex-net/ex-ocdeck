// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from 'vitest';
import type * as FR from '../terminal/focus-request';

/* ============================ 导航焦点请求信号单例（fix-terminal-input-panel-resize D3） ============================
 * 模块级内存单例：新覆盖旧、订阅同步交付快照、按 seq 清除、退订不删新请求；
 * 门禁判定 helper：过期（TTL 5s）、输入元素排除优先于区域白名单。 */

async function loadMod(): Promise<typeof FR> {
  vi.resetModules();
  return await import('../terminal/focus-request');
}

let mod: typeof FR;

beforeEach(async () => {
  mod = await loadMod();
});

describe('focus-request 单例（发布/订阅/清除语义）', () => {
  it('发布后订阅同步交付 pending 快照；seq 递增', () => {
    const r1 = mod.requestTerminalFocus('t1');
    const got: FR.TerminalFocusRequest[] = [];
    mod.subscribeTerminalFocus((r) => got.push(r));
    expect(got).toEqual([r1]);
    expect(r1.taskID).toBe('t1');

    const r2 = mod.requestTerminalFocus('t1');
    expect(r2.seq).toBe(r1.seq + 1);
  });

  it('保留最新 pending：新请求覆盖旧，新订阅者只收到最新', () => {
    const got: FR.TerminalFocusRequest[] = [];
    mod.subscribeTerminalFocus((r) => got.push(r));
    const first = mod.requestTerminalFocus('t-a');
    const latest = mod.requestTerminalFocus('t-b');
    // 订阅先于两次发布 → 逐次收到；单例只保留最新
    expect(got).toEqual([first, latest]);

    const got2: FR.TerminalFocusRequest[] = [];
    mod.subscribeTerminalFocus((r) => got2.push(r));
    expect(got2).toEqual([latest]);
  });

  it('clearTerminalFocus 按 seq 清除：仅匹配当前 seq 生效，旧 seq 清不掉新请求', () => {
    const old = mod.requestTerminalFocus('t1');
    const latest = mod.requestTerminalFocus('t1');
    mod.clearTerminalFocus(old.seq);
    const got: FR.TerminalFocusRequest[] = [];
    mod.subscribeTerminalFocus((r) => got.push(r));
    expect(got).toEqual([latest]);

    mod.clearTerminalFocus(latest.seq);
    const got2: FR.TerminalFocusRequest[] = [];
    mod.subscribeTerminalFocus((r) => got2.push(r));
    expect(got2).toEqual([]);
  });

  it('消费方退订 MUST NOT 删除更晚发布的新请求（契约场景）', () => {
    // 消费方 A 订阅并收到请求后卸载（退订）；随后发布的新请求必须仍然保留
    const unsub = mod.subscribeTerminalFocus(() => {});
    mod.requestTerminalFocus('t1');
    unsub();
    const later = mod.requestTerminalFocus('t1');
    const got: FR.TerminalFocusRequest[] = [];
    mod.subscribeTerminalFocus((r) => got.push(r));
    expect(got).toEqual([later]);
  });

  it('同步消费后：返回值恒为本次发布快照，后续监听器仍收到本次事件', () => {
    const first: FR.TerminalFocusRequest[] = [];
    mod.subscribeTerminalFocus((r) => {
      first.push(r);
      mod.clearTerminalFocus(r.seq); // 首个监听器同步消费
    });
    const second: FR.TerminalFocusRequest[] = [];
    mod.subscribeTerminalFocus((r) => second.push(r));
    const req = mod.requestTerminalFocus('t1');
    // 返回值是本次发布的快照而非 null
    expect(req.taskID).toBe('t1');
    expect(first).toEqual([req]);
    // 首个监听器同步清除不阻断后续监听器的通知（消费幂等且按 seq 守卫）
    expect(second).toEqual([req]);
    expect(mod.pendingTerminalFocus()).toBeNull();
  });

  it('重入发布：旧分发立即终止，由新发布完成分发', () => {
    const seen: string[] = [];
    let reentered = false;
    mod.subscribeTerminalFocus((r) => {
      seen.push(`a:${r.taskID}`);
      if (!reentered) {
        reentered = true;
        mod.requestTerminalFocus('t2'); // 分发期间重入发布
      }
    });
    mod.subscribeTerminalFocus((r) => seen.push(`b:${r.taskID}`));
    mod.requestTerminalFocus('t1');
    // a 收到 t1 → 重入发布 t2（a、b 都收到）→ t1 的旧分发终止（b 收不到 t1）
    expect(seen).toEqual(['a:t1', 'a:t2', 'b:t2']);
  });

  it('重入请求在分发中被同步消费后，旧分发仍终止（终止按发布代数而非 pending 存在性）', () => {
    const seen: string[] = [];
    let reentered = false;
    mod.subscribeTerminalFocus((r) => {
      seen.push(`a:${r.taskID}`);
      if (r.taskID === 't2') {
        mod.clearTerminalFocus(r.seq); // 新请求在自己的分发过程中被同步消费
        return;
      }
      if (!reentered) {
        reentered = true;
        mod.requestTerminalFocus('t2'); // t1 分发期间重入发布
      }
    });
    mod.subscribeTerminalFocus((r) => seen.push(`b:${r.taskID}`));
    mod.requestTerminalFocus('t1');
    // a 收到 t1 → 重入发布 t2 → t2 分发中被 a 同步消费（pending 变 null）
    // → t1 的旧分发仍终止（b 收不到 t1）
    expect(seen).toEqual(['a:t1', 'a:t2', 'b:t2']);
    expect(mod.pendingTerminalFocus()).toBeNull();
  });

  it('等待期输入区取消（请求层全局守卫）：进入输入区即作废，失焦不复活', () => {
    // 场景：无任何消费方挂载（如工作台加载阶段）——取消由请求层负责
    mod.requestTerminalFocus('t1');
    const input = document.createElement('input');
    document.body.appendChild(input);
    input.dispatchEvent(new FocusEvent('focusin', { bubbles: true }));
    expect(mod.pendingTerminalFocus()).toBeNull();
    // 随后焦点移回非输入区：请求不复活
    document.body.dispatchEvent(new FocusEvent('focusin', { bubbles: true }));
    expect(mod.pendingTerminalFocus()).toBeNull();
    // 作废后可正常发布新请求
    const req = mod.requestTerminalFocus('t1');
    expect(mod.pendingTerminalFocus()?.seq).toBe(req.seq);
    input.remove();
  });

  it('过期判定：TTL 固定 5s，到期边界为过期', () => {
    expect(mod.FOCUS_REQUEST_TTL_MS).toBe(5000);
    const req = { taskID: 't1', seq: 1, ts: 1000 };
    expect(mod.isTerminalFocusExpired(req, 1000 + 4999)).toBe(false);
    expect(mod.isTerminalFocusExpired(req, 1000 + 5000)).toBe(true);
  });
});

describe('焦点保护门禁 helper', () => {
  it('输入元素排除：input/textarea/contenteditable 均排除', () => {
    expect(mod.isInputElementTarget(document.createElement('input'))).toBe(true);
    expect(mod.isInputElementTarget(document.createElement('textarea'))).toBe(true);
    const ce = document.createElement('div');
    // jsdom 不实现 isContentEditable（恒 false），按真实浏览器语义桩定
    Object.defineProperty(ce, 'isContentEditable', { value: true });
    expect(mod.isInputElementTarget(ce)).toBe(true);
    expect(mod.isInputElementTarget(document.createElement('div'))).toBe(false);
    expect(mod.isInputElementTarget(null)).toBe(false);
  });

  it('白名单：body / .od-sidebar 内 / .wb-switcher 内 / .od-row-clickable 内允许', () => {
    expect(mod.isFocusRequestTargetAllowed(document.body)).toBe(true);

    const sidebar = document.createElement('aside');
    sidebar.className = 'od-sidebar';
    const inSidebar = document.createElement('span');
    sidebar.appendChild(inSidebar);
    document.body.appendChild(sidebar);
    expect(mod.isFocusRequestTargetAllowed(inSidebar)).toBe(true);

    const switcher = document.createElement('div');
    switcher.className = 'wb-switcher';
    const inSwitcher = document.createElement('span');
    switcher.appendChild(inSwitcher);
    document.body.appendChild(switcher);
    expect(mod.isFocusRequestTargetAllowed(inSwitcher)).toBe(true);

    // 指挥中心任务行：.od-row-clickable 须在 .cc-page 容器内
    const page = document.createElement('div');
    page.className = 'cc-page';
    const row = document.createElement('div');
    row.className = 'od-row od-row-clickable';
    const inRow = document.createElement('span');
    row.appendChild(inRow);
    page.appendChild(row);
    document.body.appendChild(page);
    expect(mod.isFocusRequestTargetAllowed(inRow)).toBe(true);

    // 项目管理页同名行（不在 .cc-page 内）不放行
    const pmRow = document.createElement('div');
    pmRow.className = 'od-row od-row-clickable';
    const inPmRow = document.createElement('span');
    pmRow.appendChild(inPmRow);
    document.body.appendChild(pmRow);
    expect(mod.isFocusRequestTargetAllowed(inPmRow)).toBe(false);

    const plain = document.createElement('div');
    const inPlain = document.createElement('span');
    plain.appendChild(inPlain);
    document.body.appendChild(plain);
    expect(mod.isFocusRequestTargetAllowed(inPlain)).toBe(false);
    expect(mod.isFocusRequestTargetAllowed(null)).toBe(false);
  });

  it('输入元素排除优先于区域白名单：侧栏内的 input 不允许聚焦', () => {
    const sidebar = document.createElement('aside');
    sidebar.className = 'od-sidebar';
    const input = document.createElement('input');
    sidebar.appendChild(input);
    document.body.appendChild(sidebar);
    expect(mod.isFocusRequestTargetAllowed(input)).toBe(false);
  });
});

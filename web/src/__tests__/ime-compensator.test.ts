import { describe, expect, it } from 'vitest';
import {
  createImeCompensator,
  qualifyCandidate,
  isModifierKey,
  isImeProcessKey,
  type ImeCompensatorOptions,
  type QualifyInput,
  type InputEventFields,
} from '../terminal/ime-compensator';

/**
 * IME 候选输入仲裁单测（design D7 判决表全路径）。
 *
 * 测试缝：
 * - fake clock：可控单调毫秒时钟，手动 advance。
 * - fake schedule：同步队列（push 到 microtask-free 队列，测试手动 flush），记录 setTimeout 调用。
 * - emit spy：记录补发动作；emit 同步触发模拟 onData（observeNative），验证自身发射排除。
 *
 * 断言口径：逐路径断言「原生 + 补偿」总发送次数（而非只断言补偿器 emit 次数）。
 * 反证原则：每项测试在实现回退（不裁剪/不水位/不 occurrence 消耗/不 fail-closed）时必失败。
 */

/** fake schedule handle（由 schedule 返回，cancel 标记）。 */
// 类型由 schedule 内联构造，此处无需独立声明。

/** 测试 fixture：可控时钟 + 调度队列 + emit/observeNative 记录。 */
function createFixture() {
  let now = 0;
  const queue: { fn: () => void; cancelled: boolean }[] = [];
  const emitCalls: string[] = [];
  const scheduleCalls: { delayMs: number | undefined }[] = [];
  // 模拟 term.input 同步触发 onData tap（经补偿器自身标记排除进 recentNative）
  const opts: ImeCompensatorOptions = {
    emit: (data: string) => {
      emitCalls.push(data);
      // 模拟 term.input 同步触发 onData → observeNative（补偿器内部 gate 标记排除）
      comp.observeNative(data);
    },
    now: () => now,
    schedule: (fn: () => void, delayMs?: number) => {
      scheduleCalls.push({ delayMs });
      const handle = { fn, cancelled: false, cancel() { this.cancelled = true; } };
      queue.push(handle);
      return handle;
    },
  };
  const comp = createImeCompensator(opts);
  return {
    comp,
    emitCalls,
    scheduleCalls,
    /** advance clock + 不 flush（仅时间推进）。 */
    advance(dt: number) { now += dt; },
    /** flush pending settle 队列（执行 schedule 注册的回调）。 */
    flush() {
      // 取出当前队列快照执行（settle 内可能再 schedule 但 settle 本身不会重入——scheduleSettle 守卫）
      const batch = queue.splice(0);
      for (const h of batch) if (!h.cancelled) h.fn();
    },
    /** pending 队列长度（测试观察）。 */
    pendingScheduledCount() { return queue.length; },
    /** 直接向共享 timer 队列 push 回调（模拟 xterm finalize timer 先入队，用于 FIFO 测试）。 */
    pushTimerCallback(fn: () => void) {
      const handle = { fn, cancelled: false, cancel() { this.cancelled = true; } };
      queue.push(handle);
    },
  };
}

/** 合格候选 input 事件工厂（InputEventFields，默认满足资格判决的事件字段部分；
 *  anyKeyDown/nonModKeyDown/compositionActive 由补偿器内部镜像状态提供，不在此传入）。 */
function candidateInput(data: string): InputEventFields {
  return {
    inputType: 'insertText',
    data,
    isTrusted: true,
    composed: true,
    isComposing: false,
  };
}

/** 构造完整 QualifyInput（qualifyCandidate 纯函数测试用，含镜像状态字段）。 */
function fullQualify(data: string, overrides: Partial<QualifyInput> = {}): QualifyInput {
  return {
    inputType: 'insertText',
    data,
    isTrusted: true,
    composed: true,
    isComposing: false,
    anyKeyDown: true,
    nonModKeyDown: false,
    compositionActive: false,
    ...overrides,
  };
}

/** 模拟「Shift 按住」前导：handleKeyDown(Shift) → anyKeyDown=true, nonModKeyDown=false。 */
function shiftKeyDown(comp: import('../terminal/ime-compensator').ImeCompensator) {
  comp.handleKeyDown({ key: 'Shift' });
}
function keyUp(comp: import('../terminal/ime-compensator').ImeCompensator) {
  comp.handleKeyUp();
}

describe('qualifyCandidate 资格判决纯函数', () => {
  it('全部满足 → true', () => {
    expect(qualifyCandidate(fullQualify('？'))).toBe(true);
  });
  it('inputType 非 insertText → false', () => {
    expect(qualifyCandidate(fullQualify('？', { inputType: 'insertCompositionText' }))).toBe(false);
  });
  it('data 为空 → false', () => {
    expect(qualifyCandidate(fullQualify('？', { data: '' }))).toBe(false);
  });
  it('isTrusted false → false（排除脚本合成事件）', () => {
    expect(qualifyCandidate(fullQualify('？', { isTrusted: false }))).toBe(false);
  });
  it('composed false → false（第二道闸）', () => {
    expect(qualifyCandidate(fullQualify('？', { composed: false }))).toBe(false);
  });
  it('多 code point ev.data → false（单 Unicode code point 限制）', () => {
    expect(qualifyCandidate(fullQualify('？？', { data: '？？' }))).toBe(false);
  });
  it('anyKeyDown false → false（iOS 预测/听写）', () => {
    expect(qualifyCandidate(fullQualify('？', { anyKeyDown: false }))).toBe(false);
  });
  it('nonModKeyDown true → false（普通按键重复投递）', () => {
    expect(qualifyCandidate(fullQualify('？', { nonModKeyDown: true }))).toBe(false);
  });
  it('isComposing true → false（Chrome IME 直交）', () => {
    expect(qualifyCandidate(fullQualify('？', { isComposing: true }))).toBe(false);
  });
  it('compositionActive true → false（汉字 composition 提交）', () => {
    expect(qualifyCandidate(fullQualify('？', { compositionActive: true }))).toBe(false);
  });
});

describe('isModifierKey 修饰键集合', () => {
  it('Shift/Control/Alt/Meta/CapsLock → true', () => {
    expect(isModifierKey('Shift')).toBe(true);
    expect(isModifierKey('Control')).toBe(true);
    expect(isModifierKey('Alt')).toBe(true);
    expect(isModifierKey('Meta')).toBe(true);
    expect(isModifierKey('CapsLock')).toBe(true);
  });
  it('非修饰键 → false（含 Process/keyCode 229）', () => {
    expect(isModifierKey('Process')).toBe(false);
    expect(isModifierKey('a')).toBe(false);
    expect(isModifierKey('Enter')).toBe(false);
  });
});

describe('isImeProcessKey IME 处理键双条件判定', () => {
  it("key === 'Process' → true（keyCode 任意）", () => {
    expect(isImeProcessKey('Process', undefined)).toBe(true);
    expect(isImeProcessKey('Process', 229)).toBe(true);
    expect(isImeProcessKey('Process', 0)).toBe(true);
  });
  it('keyCode === 229 → true（含 key:"Unidentified" 变体）', () => {
    expect(isImeProcessKey('Unidentified', 229)).toBe(true);
    expect(isImeProcessKey('', 229)).toBe(true);
  });
  it('key 非 Process 且 keyCode 非 229 → false', () => {
    expect(isImeProcessKey('a', undefined)).toBe(false);
    expect(isImeProcessKey('a', 65)).toBe(false);
    expect(isImeProcessKey('Enter', 13)).toBe(false);
    expect(isImeProcessKey('Shift', undefined)).toBe(false);
  });
});

describe('镜像状态（anyKeyDown/nonModKeyDown/compositionActive）', () => {
  it('修饰键 keydown 置 anyKeyDown=true、nonModKeyDown=false → 候选资格成立', () => {
    const f = createFixture();
    f.comp.handleKeyDown({ key: 'Shift' });
    f.comp.handleInput(candidateInput('？'));
    expect(f.pendingScheduledCount()).toBe(1);
  });
  it('非修饰键 keydown 置 nonModKeyDown=true → 候选资格不成立', () => {
    const f = createFixture();
    f.comp.handleKeyDown({ key: 'a' });
    f.comp.handleInput(candidateInput('？'));
    expect(f.pendingScheduledCount()).toBe(0);
  });
  it('任意 keyup 清 anyKeyDown + nonModKeyDown → 候选资格不成立', () => {
    const f = createFixture();
    f.comp.handleKeyDown({ key: 'Shift' });
    f.comp.handleKeyUp();
    f.comp.handleInput(candidateInput('？'));
    expect(f.pendingScheduledCount()).toBe(0);
  });
  it('compositionstart/end 镜像 compositionActive：end 后 + 修饰键 keydown → 资格成立', () => {
    const f = createFixture();
    f.comp.handleCompositionStart();
    f.comp.handleInput(candidateInput('？'));
    expect(f.pendingScheduledCount()).toBe(0); // compositionActive → 资格不成立
    f.comp.handleCompositionEnd();
    f.comp.handleKeyDown({ key: 'Shift' }); // 修饰键置 anyKeyDown
    f.comp.handleInput(candidateInput('？'));
    expect(f.pendingScheduledCount()).toBe(1);
  });
  it("key:'Unidentified', keyCode:229 不置位 nonModKeyDown（无 keyup）→ 后续标点候选正常补发", () => {
    // 反证：若只判 key==='Process'，'Unidentified' 变体会走 nonModKeyDown 置位分支，
    // Safari 该 keydown 无 keyup → nonModKeyDown 卡 true → 后续？候选 fail-closed 丢弃 → 断言失败。
    const f = createFixture();
    f.comp.handleKeyDown({ key: 'Shift' }); // anyKeyDown=true, nonModKeyDown=false
    f.comp.handleInput(candidateInput('？')); // 候选1
    // Unidentified + 229 变体 keydown（无 keyup）→ MUST 不置位 nonModKeyDown
    f.comp.handleKeyDown({ key: 'Unidentified', keyCode: 229 });
    f.comp.handleInput(candidateInput('？')); // 候选2：nonModKeyDown 仍 false → 资格成立
    f.flush();
    // 两候选无原生覆盖 → 各补发一次
    expect(f.emitCalls).toEqual(['？', '？']);
  });
});

describe('D7 判决表全路径（逐路径断言原生+补偿总发送）', () => {
  it('路径1：普通 ASCII 按键（nonMod 已按下）→ 资格不成立，总发送 1', () => {
    const f = createFixture();
    f.comp.handleKeyDown({ key: 'a' }); // nonModKeyDown=true
    // 普通 ASCII 按键：xterm keydown 路径发，补偿器跳过
    let nativeSent = 0;
    f.comp.observeNative('a'); // 模拟 xterm 原生发了 'a'
    nativeSent += 1;
    // input 事件资格不成立（内部 nonModKeyDown=true，由 handleKeyDown({key:'a'}) 置位）
    f.comp.handleInput(candidateInput('a'));
    f.flush();
    // 补偿器不补发
    expect(f.emitCalls).toHaveLength(0);
    expect(nativeSent + f.emitCalls.length).toBe(1);
  });

  it('路径2：Safari/iOS Shift+？（无 composition 历史）→ 补发恰好一次，总发送 1', () => {
    const f = createFixture();
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('？'));
    expect(f.pendingScheduledCount()).toBe(1);
    f.flush();
    // 无原生覆盖 → 补发一次
    expect(f.emitCalls).toEqual(['？']);
    // 原生 0 + 补偿 1 = 1
    expect(f.emitCalls.length).toBe(1);
  });

  it('路径3：汉字后立即 Shift+？（候选在 finalize 前到达）→ 原生发「你好？」，settle 观测覆盖抑制，总计各 1', () => {
    const f = createFixture();
    // composition 提交「你好」（xterm finalize 已原生发出）
    f.comp.handleCompositionEnd();
    // Shift+？候选（？抢在 finalize timer 前进 textarea）
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('？'));
    // xterm finalize 原生发出「你好？」（含？）
    let nativeSent = 0;
    f.comp.observeNative('你好？');
    nativeSent += 1;
    f.flush();
    // settle 观测 recentNative 含？→ 抑制补发
    expect(f.emitCalls).toHaveLength(0);
    // 原生 1（你好？一条）+ 补偿 0 = 1 条发送（你好？各一次）
    expect(nativeSent).toBe(1);
    expect(f.emitCalls.length).toBe(0);
  });

  it('路径4：汉字后稍后 Shift+？（finalize 已发「你好」）→ 补发恰好一次，总计各 1', () => {
    const f = createFixture();
    f.comp.handleCompositionEnd();
    // finalize 已原生发「你好」
    let nativeSent = 0;
    f.comp.observeNative('你好');
    nativeSent += 1;
    // 稍后 Shift+？候选（recentNative 不含？）
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('？'));
    f.flush();
    // 未覆盖 → 补发？一次
    expect(f.emitCalls).toEqual(['？']);
    // 原生 1（你好）+ 补偿 1（？）= 2 字符，但按「发送动作」原生 1 + 补偿 1 = 2，各字符 1 次
    expect(nativeSent + f.emitCalls.length).toBe(2);
  });

  it('路径5：快速连按两次 Shift+？→ 各补发一次（自身补发不进 recentNative），总发送 2', () => {
    const f = createFixture();
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('？'));
    f.flush();
    // 第一次补发？一次（emit 同步 observeNative 但经 gate 标记排除，不进 recentNative）
    expect(f.emitCalls).toEqual(['？']);
    // 第二次 Shift+？候选
    f.comp.handleInput(candidateInput('？'));
    f.flush();
    // 第二次未覆盖（自身第一次补发不进 recentNative）→ 补发一次
    expect(f.emitCalls).toEqual(['？', '？']);
    expect(f.emitCalls.length).toBe(2);
  });

  it('路径6：Chrome IME 直交（composition 内提交）→ 资格不成立（isComposing），总发送 1', () => {
    const f = createFixture();
    f.comp.handleCompositionStart();
    // composition 内 commit input：isComposing=true
    f.comp.handleInput({ ...candidateInput('？'), isComposing: true });
    expect(f.pendingScheduledCount()).toBe(0);
    f.comp.handleCompositionEnd();
    // xterm CompositionHelper 发一次
    f.comp.observeNative('？');
    f.flush();
    expect(f.emitCalls).toHaveLength(0);
  });

  it('路径7：Chrome 229 路径（新语义）→ 229 不置位 nonModKeyDown，候选入队；settle 观测原生 deferred diff 覆盖 → 抑制，总发送 1', () => {
    // design D7 新契约：229 keydown MUST NOT 置位 nonModKeyDown（仍置位 anyKeyDown）。
    // 该 input 现在成为候选；不双发依赖仲裁观测——xterm deferred diff 定时器在 keydown 时已注册、
    // 先于我们的 settle（setTimeout(0) FIFO）→ settle 观测到原生覆盖 → 抑制补发。
    const f = createFixture();
    f.comp.handleKeyDown({ key: 'Process' }); // keyCode 229 → 不置位 nonModKeyDown（新契约），置位 anyKeyDown
    f.comp.handleInput(candidateInput('？'));
    expect(f.pendingScheduledCount()).toBe(1); // 新契约：候选入队
    // 模拟 xterm deferred diff 定时器先于 settle 发射原生？
    f.comp.observeNative('？');
    f.flush();
    // settle 观测覆盖 → 抑制补发；总发送 = 原生 1（deferred diff 发？）+ 补偿 0 = 1
    expect(f.emitCalls).toHaveLength(0);
  });

  it('用户场景回归：Safari 按住 Shift 连打两个标点（229 keyup 缺失）→ 重排 settle 后 diff 先原生发出 → 两候选被覆盖，总发送 2', () => {
    // 反证：229 仍置位 nonModKeyDown 时第二候选被 fail-closed 丢弃 → emitCalls 仅 ['？'] → 断言失败。
    // 真实场景：keydown Shift（无 keyup）→ input('？') → keydown 229#1（无 keyup）→ input('？') → keydown 229#2（无 keyup）。
    // xterm 与补偿器都用 capture:true，xterm 先注册故先执行：229#1 时 xterm 先注册 diff#1，补偿器后 handleKeyDown 重排 settle。
    // 按重排机制：229#1 cancel settle#1 + reschedule（diff#1 在 settle#2 前入队，发 input1 的？）；
    // 229#2 cancel settle#2 + reschedule（diff#2 在 settle#3 前入队，发 input2 的？）。
    // flush：diff#1 → diff#2 原生发出 2 个？→ settle#3 观测覆盖 2 候选 → 补 0。总发送 = 原生 2 + 补偿 0 = 2。
    const f = createFixture();
    f.comp.handleKeyDown({ key: 'Shift' }); // anyKeyDown=true, nonModKeyDown=false
    // 第一个？候选（Shift 按住，composed 直交，229 未到）
    f.comp.handleInput(candidateInput('？'));
    // keydown 229#1（Safari IME 消费键，无 keyup）：xterm 先注册 diff#1，补偿器后 handleKeyDown 重排 settle
    f.pushTimerCallback(() => f.comp.observeNative('？')); // diff#1 发 input1 的？
    f.comp.handleKeyDown({ key: 'Process', keyCode: 229 }); // 229#1
    // 第二个？候选（229 已到，nonModKeyDown 仍 false → 资格成立）
    f.comp.handleInput(candidateInput('？'));
    // 再次 keydown 229#2（无 keyup）：xterm 先注册 diff#2，补偿器后 handleKeyDown 重排 settle
    f.pushTimerCallback(() => f.comp.observeNative('？')); // diff#2 发 input2 的？
    f.comp.handleKeyDown({ key: 'Process', keyCode: 229 }); // 229#2
    // 全部 flush（共享 timer 队列按注册序：diff#1 → diff#2 → settle#3）
    f.flush();
    // diff#1 + diff#2 原生发出 2 个？→ settle 观测覆盖 2 候选 → 补 0
    expect(f.emitCalls).toEqual([]);
    // 总发送 = 原生 2 + 补偿 0 = 2
    const totalSent = 2 /* diff#1 + diff#2 */ + f.emitCalls.length;
    expect(totalSent).toBe(2);
  });

  it('路径8：汉字 composition 提交（commit input 在 end 前）→ 资格不成立（compositionActive），总发送 1', () => {
    const f = createFixture();
    f.comp.handleCompositionStart();
    f.comp.handleInput(candidateInput('你'));
    expect(f.pendingScheduledCount()).toBe(0);
    f.comp.handleCompositionEnd();
    f.comp.observeNative('你');
    f.flush();
    expect(f.emitCalls).toHaveLength(0);
  });

  it('路径9：迟到重复 commit（按住修饰键，finalize 已原生发出）100ms 窗口内 → 抑制，总发送 1', () => {
    const f = createFixture();
    shiftKeyDown(f.comp);
    // finalize 已原生发出？
    f.comp.observeNative('？');
    // 迟到重复 commit 候选，回看窗口内（同一时刻）
    f.comp.handleInput(candidateInput('？'));
    f.flush();
    // settle 观测 recentNative 含？且在窗口内 → 抑制
    expect(f.emitCalls).toHaveLength(0);
  });

  it('路径9b：迟到重复 commit 超出 100ms 窗口 → 按契约补发（残余风险行为锁定），原生 1 + 补偿 1', () => {
    const f = createFixture();
    shiftKeyDown(f.comp);
    f.comp.observeNative('？'); // 原生发
    // 超窗：advance 150ms 后候选到达（c.at - 100ms = 50，原生 at=0，不在窗口 [50, now]）
    f.advance(150);
    f.comp.handleInput(candidateInput('？'));
    f.flush();
    // 超窗 → 未覆盖 → 按契约补发（残余风险 b）
    expect(f.emitCalls).toEqual(['？']);
  });

  it('路径10：iOS 预测/听写（无按键 → anyKeyDown=false）→ 资格不成立，总发送 1', () => {
    const f = createFixture();
    keyUp(f.comp); // 任意 keyup 清 anyKeyDown=false（初始已是 false，显式清）
    f.comp.handleInput(candidateInput('？'));
    expect(f.pendingScheduledCount()).toBe(0);
    f.comp.observeNative('？'); // xterm gate 通过自发
    f.flush();
    expect(f.emitCalls).toHaveLength(0);
  });

  it('路径11：未知时序变体 fail-closed → 不补发', () => {
    const f = createFixture();
    shiftKeyDown(f.comp);
    // isTrusted=false（脚本合成事件）→ 资格不成立
    f.comp.handleInput({ ...candidateInput('？'), isTrusted: false });
    f.flush();
    expect(f.emitCalls).toHaveLength(0);
  });
});

describe('occurrence 级消耗 + 匹配顺序', () => {
  it('聚合原生 onData("？？") 一条记录依次覆盖两个「？」候选 → 两候选均抑制，总发送 2（原生 1 条聚合）', () => {
    const f = createFixture();
    shiftKeyDown(f.comp);
    // 候选1
    f.comp.handleInput(candidateInput('？'));
    // 候选2（连按）
    f.comp.handleInput(candidateInput('？'));
    // 原生聚合发射「？？」一条记录（remainingText="？？"）
    f.comp.observeNative('？？');
    f.flush();
    // 两个候选均被同一记录的两次 occurrence 覆盖 → 均抑制
    expect(f.emitCalls).toHaveLength(0);
  });

  it('多条记录同时可匹配 → 取时间最早者（同时间戳取插入序），删除第一次出现，耗尽立即移出 ring', () => {
    const f = createFixture();
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('？'));
    // 两条原生记录都含？：r0 at=0，r1 at=10
    f.comp.observeNative('？'); // at=0, remainingText="？"
    f.advance(10);
    f.comp.observeNative('？x'); // at=10, remainingText="？x"（含？一次）
    f.flush();
    // 候选 at=0，窗口 [-100, settleNow]；r0 at=0 早于 r1 at=10 → 取 r0，耗尽移出
    expect(f.emitCalls).toHaveLength(0);
    // 验证 r0 被耗尽移出、r1 保留：新候选匹配 r1（？x 剩余含？）
    f.comp.handleInput(candidateInput('？'));
    f.flush();
    // r1 remainingText="？x" 含？ → 覆盖
    expect(f.emitCalls).toHaveLength(0);
  });

  it('窗口嵌套：较早候选匹配旧/新两条、较晚候选只匹配新记录 → earliest-first 把新记录留给较晚候选，两候选均覆盖（单调时钟）', () => {
    const f = createFixture();
    // 单调顺序：e1@0 → c1@10 → e2@100 → c2@110 → settle
    f.comp.observeNative('a'); // e1 at=0, remainingText="a"
    f.advance(10);
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('a')); // c1 at=10，窗口 [-90, settleNow] 覆盖 e1(0)+e2(100)
    f.advance(90); // now=100
    f.comp.observeNative('a'); // e2 at=100, remainingText="a"
    f.advance(10); // now=110
    f.comp.handleInput(candidateInput('a')); // c2 at=110，窗口 [10, settleNow] 只覆盖 e2(100)，不含 e1(0)
    f.flush();
    // earliest-first：c1(at=10) 窗口含 e1(0)+e2(100)，取时间最早=e1(0) → 消耗 e1
    // c2(at=110) 窗口 [10, settleNow] 只含 e2(100) → 消耗 e2
    // 两候选均覆盖、e2 保留给 c2（earliest-first 不抢占新记录）
    expect(f.emitCalls).toHaveLength(0);
  });
});

describe('裁剪安全 + 容量溢出', () => {
  it('settle 推迟 >150ms：回看窗口内记录 MUST NOT 被惰性裁剪，仍正确抑制', () => {
    const f = createFixture();
    shiftKeyDown(f.comp);
    f.comp.observeNative('？'); // at=0
    f.comp.handleInput(candidateInput('？')); // at=0
    // settle 被主线程暂停推迟 200ms（>150ms）
    f.advance(200);
    // 此时 trim：pending 非空，minCandidateAt=0，保护窗口起点 = 0-100 = -100；
    // 记录 at=0 >= -100（落在保护窗口内）→ MUST NOT 裁剪
    f.flush();
    // settle 时记录仍存在 → 覆盖候选 → 抑制
    expect(f.emitCalls).toHaveLength(0);
  });

  it('容量 32 溢出落在 pending 窗口内 → 该候选 fail-closed 不补发', () => {
    const f = createFixture();
    shiftKeyDown(f.comp);
    // 候选 at=10
    f.advance(10);
    f.comp.handleInput(candidateInput('？')); // at=10
    // 灌 33 条原生记录，at 从 0 递增；溢出丢最旧（at=0..某条落在候选窗口 [10-100,now]）
    // 构造：先填 32 条 at=10（与候选同时，落在窗口内），第 33 条触发溢出
    for (let i = 0; i < 33; i++) {
      f.comp.observeNative('x'); // 不含？，但溢出记录 at 在候选窗口内
    }
    // 溢出记录 at >= 10-100 = -90 → 落在候选窗口 → 候选标记不可证明
    f.flush();
    // fail-closed 不补发
    expect(f.emitCalls).toHaveLength(0);
  });

  it('先溢出后候选：t=0 observeNative 被后续 32 条溢出删除，t=50 候选回看窗口与缺口重叠 → fail-closed', () => {
    const f = createFixture();
    // t=0 一条原生记录（含？），pending 为空
    f.comp.observeNative('？'); // at=0, remainingText=？
    // 灌 32 条新记录（at 递增），溢出丢弃 at=0 的记录，记 lastCapacityEvictionAt
    for (let i = 0; i < 32; i++) {
      f.advance(1);
      f.comp.observeNative('x'); // 不含？
    }
    // lastCapacityEvictionAt ≈ 32（最后溢出时刻）
    // t=50 新候选入队，回看窗口 [50-100, now] = [-50, 50]；lastCapacityEvictionAt(32) >= -50 → 重叠
    f.advance(18); // now ≈ 50
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('？')); // at=50, unprovable=true
    f.flush();
    // fail-closed 不补发
    expect(f.emitCalls).toHaveLength(0);
  });

  it('历史缺口超过 100ms 后失效：后续候选窗口不再重叠 → 正常补发', () => {
    const f = createFixture();
    // 制造溢出水位
    for (let i = 0; i < 33; i++) {
      f.comp.observeNative('x');
    }
    // 水位时刻 ≈ 0；advance 200ms 后新候选窗口 [200-100, 200] = [100, 200]，水位 0 < 100 → 不重叠
    f.advance(200);
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('？')); // at=200
    f.flush();
    // 无原生覆盖 → 正常补发
    expect(f.emitCalls).toEqual(['？']);
  });

  it('stale 记录占满 ring 时先裁剪不触发水位（驱逐前先 trim）', () => {
    // 反证：若 observeNative 先 evictIfOverflow 再 trim，stale 记录会撑爆容量触发 lastCapacityEvictionAt，
    // 随后合法标点候选回看窗口与水位重叠 → 误 fail-closed 不补发。
    // 修复后：先 trim 清掉 stale（超 150ms 且无 pending），ring 不溢出 → 无水位 → 合法标点正常补发。
    const f = createFixture();
    // 灌 32 条 stale 记录（at=0），pending 为空。
    // 注意：每次 observeNative 内部 now=0（未 advance），stale 判定需 now - r.at > 150ms——
    // 这里先灌 32 条 at=0，再 advance 200ms 使其变 stale，再 observeNative 第 33 条触发先 trim 后 evict。
    for (let i = 0; i < 32; i++) {
      f.comp.observeNative('x'); // at=0
    }
    // advance 200ms → 32 条记录均超 150ms 变 stale（pending 为空 → trim 可丢）
    f.advance(200);
    // observeNative 第 33 条：先 trim 清掉 32 条 stale（ring 空），再 evict 无溢出 → 无水位
    f.comp.observeNative('x'); // at=200
    // 新合法标点候选 at=200，回看窗口 [100, 200]；无水位 → 不重叠 → 正常补发
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('？')); // at=200
    f.flush();
    // 反证：若先 evict 再 trim，32 条 stale 在 evict 时撑爆容量 → 记 lastCapacityEvictionAt=200
    // → 候选 at=200 回看窗口 [100,200] 与水位 200 重叠 → fail-closed 不补发 → 此测试失败
    expect(f.emitCalls).toEqual(['？']);
  });
});

describe('自身发射排除 + emit 同步', () => {
  it('补偿器 emit 经 gate 标记：emit 同步触发 observeNative 但不进 recentNative', () => {
    const f = createFixture();
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('？'));
    f.flush();
    // emit('？') 同步触发 observeNative('？')，但 gate.inFlight=true → 不记录
    expect(f.emitCalls).toEqual(['？']);
    // 验证：新候选？应不被自身补发误杀（recentNative 不含？）
    f.comp.handleInput(candidateInput('？'));
    f.flush();
    expect(f.emitCalls).toEqual(['？', '？']);
  });

  it('合格候选无原生覆盖 → 补发恰好一次', () => {
    const f = createFixture();
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('？'));
    f.flush();
    expect(f.emitCalls).toHaveLength(1);
    expect(f.emitCalls[0]).toBe('？');
  });
});

describe('粘贴路径不经补偿器', () => {
  it('粘贴 input 事件 inputType 非 insertText → 资格不成立', () => {
    const f = createFixture();
    f.comp.handleInput({ ...candidateInput('pasted text'), inputType: 'insertFromPaste', data: 'pasted text' });
    expect(f.pendingScheduledCount()).toBe(0);
    f.flush();
    expect(f.emitCalls).toHaveLength(0);
  });

  it('多字符 ev.data（如 "？？"）handleInput 资格不成立 → fail-closed 不进队列', () => {
    const f = createFixture();
    shiftKeyDown(f.comp);
    f.comp.handleInput({ ...candidateInput('？？'), data: '？？' });
    expect(f.pendingScheduledCount()).toBe(0);
    f.flush();
    expect(f.emitCalls).toHaveLength(0);
  });
});

describe('dispose 清理', () => {
  it('dispose 取消 pending settle + 清空状态', () => {
    const f = createFixture();
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('？'));
    expect(f.pendingScheduledCount()).toBe(1);
    f.comp.dispose();
    // 队列中的 handle 应被 cancel
    f.flush(); // 已 cancel → 不执行
    expect(f.emitCalls).toHaveLength(0);
    // dispose 后新事件不进队列（状态清空，anyKeyDown 已复位为 false → 资格不成立）
    f.comp.handleInput(candidateInput('？'));
    expect(f.pendingScheduledCount()).toBe(0);
  });
});

describe('共享单 settle 定时器 + FIFO 调度', () => {
  it('两个候选只调一次 schedule 且 delay=0（共享单定时器 guard）', () => {
    const f = createFixture();
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('？'));
    expect(f.scheduleCalls).toHaveLength(1);
    expect(f.scheduleCalls[0].delayMs).toBe(0);
    // 第二个候选：settleHandle 已存在 → scheduleSettle guard 不再调度
    f.comp.handleInput(candidateInput('？'));
    expect(f.scheduleCalls).toHaveLength(1); // 仍只一次
    expect(f.pendingScheduledCount()).toBe(1); // 共享同一 handle
    f.flush();
    // 两个候选都补发（自身发射排除互不误杀）
    expect(f.emitCalls).toEqual(['？', '？']);
  });

  it('finalize-before-settle FIFO（共享 timer 队列）：finalize callback 先入队、settle callback 后入队，flush 按入队序执行', () => {
    // design D7 不双发论证路径2：compositionend 后 xterm 的 finalize setTimeout(0) 先于我们的 settle setTimeout(0) 注册，
    // setTimeout(0) FIFO ⇒ finalize 先执行发「你好？」，settle 后执行观测到覆盖 ⇒ 抑制补发。
    // 同一 fake timer 队列：先 push finalize callback（含 observeNative），再 handleInput 入队 settle callback，
    // flush 按入队序执行——finalize 先、settle 后。能发现 scheduler 改 microtask 或注册顺序反转。
    const f = createFixture();
    f.comp.handleCompositionEnd();
    shiftKeyDown(f.comp);
    // 先注册 finalize callback 到共享队列（模拟 xterm finalize timer 先入队）
    f.pushTimerCallback(() => {
      // finalize 发出「你好？」（含？）
      f.comp.observeNative('你好？');
    });
    // 再 handleInput → settle 入队（在 finalize 之后）
    f.comp.handleInput(candidateInput('？'));
    expect(f.scheduleCalls).toHaveLength(1); // settle 注册一次
    expect(f.scheduleCalls[0].delayMs).toBe(0);
    // flush 按入队序执行：finalize 先发「你好？」，settle 后执行观测覆盖 → 抑制
    f.flush();
    expect(f.emitCalls).toHaveLength(0);
  });
});

/**
 * 忠实 xterm diff timer 模拟（design D7 不双发论证第 3 条）。
 *
 * xterm 在 keydown 229 时同步调用 `_handleAnyTextareaChanges`，其内部 setTimeout(0) 注册 deferred diff
 * timer（xterm.js 源码：`return 229!==e.keyCode||(this._handleAnyTextareaChanges(),!1)`）。
 * xterm 与补偿器都用 capture:true 注册 keydown 监听（xterm.js 源码：
 * `addDisposableDomListener(textarea,"keydown",...,!0)`），同 capture 阶段按 addEventListener 顺序执行——
 * xterm 先注册（Terminal 构造时）故先执行，补偿器后注册（session attachImeListeners）故后执行。
 *
 * 故 229 keydown 的模拟顺序：先 push diff timer（xterm 先执行），再调 handleKeyDown（补偿器后执行重排 settle）。
 * flush 按注册序：diff 先结算原生发出进 recentNative → settle 观测覆盖只补未覆盖候选。
 */
describe('xterm diff timer 竞争模拟（229 路径，共享 fake timer 队列按注册序 flush）', () => {
  /** 模拟「229 keydown」：先 push xterm diff timer（capture 先执行），再调补偿器 handleKeyDown（capture 后执行重排）。
   *  diffChar 为该 diff timer 执行时原生发送的字符（模拟 textarea diff 结果）；空串表示 diff 空读不写。 */
  function keydown229WithDiff(
    f: ReturnType<typeof createFixture>,
    diffChar: string,
  ) {
    // xterm _keyDown 先执行：229 → _handleAnyTextareaChanges → setTimeout 注册 diff timer
    if (diffChar) {
      f.pushTimerCallback(() => {
        f.comp.observeNative(diffChar);
      });
    }
    // 补偿器 handleKeyDown 后执行：可能重排 settle
    f.comp.handleKeyDown({ key: 'Process', keyCode: 229 });
  }

  it('a. Chrome 时序 229 → input：diff timer 先注册先结算 → 原生覆盖 → 补偿抑制，总发送 1', () => {
    // Chrome：keydown 229 先于 input。229 时 pending 为空 → 无重排；xterm diff timer 在 229 时注册（先入队）。
    // 随后 input 到达 handleInput 入队 settle（在 diff 之后）→ flush 时 diff 先发？，settle 观测覆盖 → 抑制。
    const f = createFixture();
    keydown229WithDiff(f, '？'); // 229 keydown + diff timer 入队（pending 为空 → 无重排）
    expect(f.scheduleCalls).toHaveLength(0); // pending 为空 → handleKeyDown 不重排
    f.comp.handleInput(candidateInput('？')); // 候选入队 + settle 入队（在 diff 之后）
    expect(f.scheduleCalls).toHaveLength(1);
    f.flush();
    // diff 先发？→ recentNative 含？→ settle 观测覆盖 → 抑制；总发送 = 原生 1 + 补偿 0 = 1
    expect(f.emitCalls).toHaveLength(0);
  });

  it('b. Safari 时序 input → 229（单次）：settle 重排后 diff 先结算（空 diff）→ 候选补发，总发送 1', () => {
    // Safari 单次：input 先于 229。handleInput 入队 settle#1；229 keydown：xterm 先注册 diff（空读，单次输入），
    // 补偿器后 handleKeyDown pending 非空 → cancel settle#1 + reschedule settle#2。
    // queue: [settle#1(c), diff(空), settle#2]。flush: diff(空无发) → settle#2 观测无覆盖 → 补发 1。
    // 总发送 = 原生 0 + 补偿 1 = 1。单次输入时 229 重排无行为差异（diff 空读不写）。
    const f = createFixture();
    f.comp.handleKeyDown({ key: 'Shift' }); // anyKeyDown=true, nonModKeyDown=false
    f.comp.handleInput(candidateInput('？')); // 候选入队 + settle#1 入队
    expect(f.scheduleCalls).toHaveLength(1);
    // 229 keydown：xterm diff（空读，单次输入）先入队；补偿器 handleKeyDown 重排 settle#2 后入队
    keydown229WithDiff(f, ''); // diffChar='' → 不 observeNative（空读）
    expect(f.scheduleCalls).toHaveLength(2); // settle#1 + settle#2（重排）
    f.flush();
    // diff 空读无原生 → settle#2 观测无覆盖 → 补发 1；总发送 = 原生 0 + 补偿 1 = 1
    expect(f.emitCalls).toEqual(['？']);
  });

  it('c. 批量 input1 → 229#1 → input2 → 229#2（timer 统一最后 flush）：229 重排 settle → diff 先结算原生发出 → settle 只补未覆盖 → 原生+补偿总发送恰好 2', () => {
    // 反证：无重排机制时 settle 先于 diff 结算 → 补发候选 + diff 原生发出 → 总发送 > 2 → 断言失败。
    // 有重排（xterm capture 先注册 diff，补偿器 capture 后重排 settle）：
    // - input1 → settle#1 入队
    // - 229#1：xterm diff#1 入队 → 补偿器 cancel settle#1 + reschedule settle#2 入队
    // - input2 → scheduleSettle guard（settle#2 已调度）→ 不调度
    // - 229#2：xterm diff#2 入队 → 补偿器 cancel settle#2 + reschedule settle#3 入队
    // queue: [settle#1(c), diff#1, settle#2(c), diff#2, settle#3]
    // flush：diff#1 → diff#2 → settle#3 → settle 观测 2 个原生？覆盖 2 候选 → 补 0。总发送 = 原生 2 + 补偿 0 = 2。
    const f = createFixture();
    f.comp.handleKeyDown({ key: 'Shift' }); // anyKeyDown=true, nonModKeyDown=false
    // input1 → 候选1 + settle#1 入队
    f.comp.handleInput(candidateInput('？'));
    expect(f.scheduleCalls).toHaveLength(1);
    // 229#1：xterm diff#1 入队（发 input1 的？）→ 补偿器重排 settle#2 入队
    keydown229WithDiff(f, '？'); // diff#1
    expect(f.scheduleCalls).toHaveLength(2);
    // input2 → 候选2；settle#2 已调度 → scheduleSettle guard 不再调度
    f.comp.handleInput(candidateInput('？'));
    expect(f.scheduleCalls).toHaveLength(2);
    // 229#2：xterm diff#2 入队（发 input2 的？）→ 补偿器重排 settle#3 入队
    keydown229WithDiff(f, '？'); // diff#2
    expect(f.scheduleCalls).toHaveLength(3);
    // 全部 flush（共享 timer 队列按注册序执行未 cancelled 的）
    f.flush();
    // diff#1 + diff#2 原生发出 2 个？→ settle#3 观测覆盖 2 候选 → 补 0
    expect(f.emitCalls).toHaveLength(0);
    // 总发送 = 原生 2（diff#1 + diff#2）+ 补偿 0 = 2
    // 反证：无重排时 settle#1 先执行补 2 + diff#1/diff#2 原生发 2 = 4（或 settle 补 2 + diff#1 发 1 = 3）→ > 2 → 失败
    const totalSent = 2 /* diff#1 + diff#2 原生 */ + f.emitCalls.length;
    expect(totalSent).toBe(2);
  });

  it('d. keyup 正常变体（每次 229 后有 keyup）：行为等价单发，总发送 2', () => {
    // 每次 229 后有 keyup → anyKeyDown/nonModKeyDown 清 false。229 仍不置位 nonModKeyDown，
    // keyup 后 anyKeyDown=false → 后续 input 资格不成立（anyKeyDown=false）。
    // 故需在每次 input 前重新 shiftKeyDown 置 anyKeyDown。行为等价单发：两次独立输入，各补发一次。
    const f = createFixture();
    // 第一次输入
    f.comp.handleKeyDown({ key: 'Shift' });
    f.comp.handleInput(candidateInput('？'));
    keydown229WithDiff(f, ''); // diff 空读
    f.comp.handleKeyUp(); // keyup 清 anyKeyDown + nonModKeyDown
    f.flush();
    expect(f.emitCalls).toEqual(['？']);
    // 第二次输入（keyup 后需重新 shiftKeyDown）
    f.comp.handleKeyDown({ key: 'Shift' });
    f.comp.handleInput(candidateInput('？'));
    keydown229WithDiff(f, ''); // diff 空读
    f.comp.handleKeyUp();
    f.flush();
    expect(f.emitCalls).toEqual(['？', '？']);
    // 总发送 = 原生 0 + 补偿 2 = 2
    expect(f.emitCalls.length).toBe(2);
  });
});

describe('settle 异常路径状态损坏防护', () => {
  /** 支持 emit 抛异常 + 重入 handleInput 的 fixture。
   *  - throwOnEmit：emit 抛错。
   *  - reentrantOnEmit：emit 内同步调 handleInput('y') 后抛错（模拟 emit 栈内重入新候选）。 */
  function createThrowFixture() {
    let now = 0;
    const queue: { fn: () => void; cancelled: boolean }[] = [];
    const emitCalls: string[] = [];
    let throwOnEmit = false;
    let reentrantOnEmit = false;
    const opts: ImeCompensatorOptions = {
      emit: (data: string) => {
        emitCalls.push(data);
        if (reentrantOnEmit) {
          // emit 内同步调 handleInput('y')（重入新候选），然后抛错
          comp.handleInput(candidateInput('y'));
          throw new Error('emit-boom');
        }
        if (throwOnEmit) throw new Error('emit-boom');
        comp.observeNative(data);
      },
      now: () => now,
      schedule: (fn: () => void, _delayMs?: number) => {
        const handle = { fn, cancelled: false, cancel() { this.cancelled = true; } };
        queue.push(handle);
        return handle;
      },
    };
    const comp = createImeCompensator(opts);
    return {
      comp,
      emitCalls,
      setThrowOnEmit(v: boolean) { throwOnEmit = v; },
      setReentrantOnEmit(v: boolean) { reentrantOnEmit = v; },
      advance(dt: number) { now += dt; },
      flush() {
        const batch = queue.splice(0);
        for (const h of batch) if (!h.cancelled) h.fn();
      },
      pendingScheduledCount() { return queue.length; },
      pushTimerCallback(fn: () => void) {
        const handle = { fn, cancelled: false, cancel() { this.cancelled = true; } };
        queue.push(handle);
      },
    };
  }

  it('c1 已消费原生 occurrence、c2 emit 抛错 → 后续 settle MUST NOT 重放 c1 双发', () => {
    // 反例（修复前）：c1 被原生覆盖并消耗 occurrence → c2 emit 抛错 → pending 仍含 c1/c2
    // 但 c1 的 occurrence 已消费 → 下次 settle 重放 c1 双发。
    // 修复后：try/finally 整批移出 pending，c1 不会重放。
    const f = createThrowFixture();
    // c1 候选（？）
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('？')); // c1
    // c2 候选（x）—— emit 时抛错
    f.comp.handleInput(candidateInput('x')); // c2
    // 原生发射「？」覆盖 c1（c1 的 occurrence 被消费）
    f.comp.observeNative('？');
    // c2 无原生覆盖 → emit('x') 抛错
    f.setThrowOnEmit(true);
    expect(() => f.flush()).toThrow('emit-boom');
    // c1 已被原生覆盖（emitCalls 不含？），c2 emit 抛错（emitCalls 含 x）
    expect(f.emitCalls).toEqual(['x']);
    // 关键断言：后续 settle MUST NOT 重放 c1（pending 应已清空，c1 的 occurrence 已消费）
    // 触发新 settle（若有残留 c1 会重放？）
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('y')); // 新候选触发新 settle
    f.setThrowOnEmit(false); // 新候选正常 emit
    f.flush();
    // 不应补发？（c1 已被覆盖，不应重放）
    expect(f.emitCalls).not.toContain('？');
    // y 无原生覆盖 → 补发 y 一次
    expect(f.emitCalls).toContain('y');
    // 精确序列断言：c2 emit('x') 抛错（x 已 emit 但不重试）→ 新 settle 补发 y
    // 错误实现「只删 c1 保留重放 c2=x」会得到 ['x','x','y']——精确序列锁定 x 不重试
    expect(f.emitCalls).toEqual(['x', 'y']);
  });

  it('真实重入：emit(x) 内同步 handleInput(y) 后抛错 → 下一个定时器必须补发 y（重入候选被 splice(0,batchSize) 保留）', () => {
    // settle 批次 [c1=x]：emit('x') 内同步 handleInput('y')（重入新候选 y 入 pending）后抛错。
    // try/finally splice(0, batchSize=1) 只移除前 1 条（x），保留重入追加的 y。
    // 下一个定时器 settle 补发 y。
    // 反证：finally 改成 pending.length=0 会把重入的 y 一起清掉 → y 永不补发 → 测试失败。
    const f = createThrowFixture();
    shiftKeyDown(f.comp);
    f.comp.handleInput(candidateInput('x')); // c1=x，emit 时重入 handleInput('y') + 抛错
    f.setReentrantOnEmit(true);
    expect(() => f.flush()).toThrow('emit-boom');
    // emit('x') 已记录（抛错前），重入 handleInput('y') 入 pending 并 schedule 新 settle
    expect(f.emitCalls).toEqual(['x']);
    // 下一个定时器：y 无原生覆盖 → 补发 y
    f.setReentrantOnEmit(false);
    f.flush();
    expect(f.emitCalls).toEqual(['x', 'y']);
  });
});
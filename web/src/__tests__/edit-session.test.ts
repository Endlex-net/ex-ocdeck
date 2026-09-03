// @vitest-environment jsdom
import { describe, it, expect, vi } from 'vitest';
import { EditSession, type EditSessionIO } from '../components/diff/edit-session';
import type { FileEditRead, FileEditReadEditable, FileEditWriteInput } from '../types';

/* ============================ 编辑写回协议状态机（design D5 前端写协议，tasks 5.6） ============================
 * 覆盖：debounce 合并、单在途串行合并、冻结 lineEnding/mode、409 阻塞、未知结果四元重读、
 * 放弃、还原、离开守卫。 */

const conflictErr = Object.assign(new Error('conflict'), { code: 'conflict' });
const isConflict = (err: unknown) => (err as { code?: string }).code === 'conflict';

function makeRead(content: string, over: Partial<FileEditReadEditable> = {}): FileEditReadEditable {
  return {
    editable: true,
    content,
    baseHash: 'h0',
    lineEnding: 'lf',
    hasBom: false,
    mode: '0644',
    ...over,
  };
}

function makeIO() {
  const writes: FileEditWriteInput[] = [];
  const reads: FileEditRead[] = [];
  const io: EditSessionIO & {
    writes: FileEditWriteInput[];
    readQueue: FileEditRead[];
    writeImpl: ReturnType<typeof vi.fn>;
    readImpl: ReturnType<typeof vi.fn>;
  } = {
    writes,
    readQueue: [],
    writeImpl: vi.fn(async (_input: FileEditWriteInput) => ({ baseHash: `h${writes.length}` })),
    readImpl: vi.fn(async () => {
      const r = io.readQueue.shift() ?? makeRead('');
      reads.push(r);
      return r;
    }),
    write: async (input) => {
      writes.push(input); // 记录全部写请求（含被拒/丢失的）
      return io.writeImpl(input);
    },
    read: () => io.readImpl(),
  };
  return io;
}

function makeSession(io: EditSessionIO, firstRead = makeRead('a\nb\n')) {
  return new EditSession({ path: 'f.txt', firstRead, io, debounceMs: 5, isConflict });
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

describe('debounce 与串行合并', () => {
  it('debounce 500ms（测试用 5ms）合并连续编辑为一次写请求', async () => {
    const io = makeIO();
    const s = makeSession(io);
    s.onEdit('v1\n');
    s.onEdit('v2\n');
    s.onEdit('v3\n');
    await sleep(30);
    expect(io.writes).toHaveLength(1);
    expect(io.writes[0].content).toBe('v3\n');
    expect(s.status).toBe('clean');
    s.dispose();
  });

  it('每文件单在途：在途期间的编辑合并为最新文档，响应后携带新 baseHash 再发', async () => {
    const io = makeIO();
    let resolveFirst!: (v: { baseHash: string }) => void;
    io.writeImpl.mockImplementationOnce(
      () =>
        new Promise((res) => {
          resolveFirst = res;
        }),
    );
    const s = makeSession(io);
    s.onEdit('v1\n');
    const p = s.flush();
    await sleep(1);
    expect(io.writes).toHaveLength(1);
    expect(io.writes[0]).toMatchObject({
      path: 'f.txt',
      content: 'v1\n',
      baseHash: 'h0',
      lineEnding: 'lf',
      baseMode: '0644',
    });
    expect(s.sentContent).toBe('v1\n');
    // 在途期间继续编辑
    s.onEdit('v2\n');
    resolveFirst({ baseHash: 'hX' });
    await p;
    expect(io.writes).toHaveLength(2);
    expect(io.writes[1]).toMatchObject({ content: 'v2\n', baseHash: 'hX' });
    expect(s.baseHash).toBe('h2'); // 第二次写由缺省 mock 返回 h2
    expect(s.status).toBe('clean');
    s.dispose();
  });

  it('lineEnding 冻结：CRLF 文件删除全部换行后新增换行，写请求仍携带冻结 crlf', async () => {
    const io = makeIO();
    const s = makeSession(io, makeRead('a\r\nb\r\n', { lineEnding: 'crlf' }));
    // 删除全部换行
    s.onEdit('ab');
    await s.flush();
    expect(io.writes[0].lineEnding).toBe('crlf');
    expect(io.writes[0].content).toBe('ab');
    // 再新增换行
    s.onEdit('a\nb');
    await s.flush();
    expect(io.writes[1].lineEnding).toBe('crlf');
    expect(io.writes[1].content).toBe('a\nb');
    s.dispose();
  });
});

describe('409 阻塞', () => {
  it('409 → 保留内容 + 暂停自动写回 + 阻塞；重试成功后恢复', async () => {
    const io = makeIO();
    io.writeImpl.mockRejectedValueOnce(conflictErr);
    const s = makeSession(io);
    s.onEdit('v1\n');
    await s.flush();
    expect(s.status).toBe('blocked');
    expect(s.latest).toBe('v1\n'); // 内容保留
    expect(s.blockedReason).toContain('冲突');
    // 阻塞期间继续编辑不触发写回
    s.onEdit('v2\n');
    await sleep(30);
    expect(io.writes).toHaveLength(1);
    // 重试：重发最新文档
    await s.retry();
    expect(io.writes).toHaveLength(2);
    expect(io.writes[1].content).toBe('v2\n');
    expect(s.status).toBe('clean');
    s.dispose();
  });

  it('阻塞未解决 → canLeave false；解决后 true', async () => {
    const io = makeIO();
    io.writeImpl.mockRejectedValueOnce(conflictErr);
    const s = makeSession(io);
    s.onEdit('v1\n');
    await s.flush();
    expect(await s.canLeave()).toBe(false);
    // F1：重试必须强制重发未确认的 latest（sentContent 不得冒充确认基线）
    await s.retry();
    expect(io.writes).toHaveLength(2);
    expect(io.writes[1]).toMatchObject({ content: 'v1\n', baseHash: 'h0' });
    expect(s.status).toBe('clean');
    expect(await s.canLeave()).toBe(true);
    s.dispose();
  });

  it('显式放弃：重读服务端内容并返回完整新读取（含新冻结元数据，F8）', async () => {
    const io = makeIO();
    io.writeImpl.mockRejectedValueOnce(conflictErr);
    io.readQueue.push(
      makeRead('server-side\n', { baseHash: 'hS', mode: '0755', lineEnding: 'crlf', hasBom: true }),
    );
    const s = makeSession(io);
    s.onEdit('v1\n');
    await s.flush();
    expect(s.status).toBe('blocked');
    const res = await s.discard();
    // 返回完整 editable 分支：调用方据此结束旧 session 并以新元数据创建新 session
    expect(res?.editedDuring).toBe(false);
    expect(res?.read).toMatchObject({
      editable: true,
      content: 'server-side\n',
      baseHash: 'hS',
      mode: '0755',
      lineEnding: 'crlf',
      hasBom: true,
    });
    expect(s.latest).toBe('server-side\n');
    expect(s.baseHash).toBe('hS');
    expect(s.status).toBe('clean');
    s.dispose();
  });

  it('F8：放弃时文件已不可编辑 → 返回 denied 分支（不返回 null 困住用户）', async () => {
    const io = makeIO();
    io.writeImpl.mockRejectedValueOnce(conflictErr);
    io.readQueue.push({
      editable: false,
      reasonCode: 'read_only',
      reason: '文件只读',
    });
    const s = makeSession(io);
    s.onEdit('v1\n');
    await s.flush();
    expect(s.status).toBe('blocked');
    const res = await s.discard();
    expect(res?.read).toMatchObject({ editable: false, reasonCode: 'read_only' });
    s.dispose();
  });

  it('F11：discard 等待 GET 期间收到新编辑 → 保留 latest 并标记 editedDuring，不覆盖', async () => {
    const io = makeIO();
    io.writeImpl.mockRejectedValueOnce(conflictErr);
    let resolveRead!: () => void;
    io.readImpl.mockImplementationOnce(
      () =>
        new Promise<FileEditRead>((res) => {
          resolveRead = () => res(makeRead('server-side\n', { baseHash: 'hS' }));
        }),
    );
    const s = makeSession(io);
    s.onEdit('v1\n');
    await s.flush();
    expect(s.status).toBe('blocked');
    const dp = s.discard();
    await sleep(1);
    // GET 在途期间的新编辑（F11 栅栏：不得被服务端内容覆盖）
    s.onEdit('v2\n');
    resolveRead();
    const res = await dp;
    expect(res?.editedDuring).toBe(true);
    expect(s.latest).toBe('v2\n'); // 用户输入保留
    expect(s.baseHash).toBe('hS'); // 新基线已采用（供调用方补发）
    expect(s.status).toBe('pending'); // 不写回（旧冻结元数据不可信，由调用方换 session 补发）
    expect(io.writes).toHaveLength(1);
    s.dispose();
  });

  it('F11：dispose({flush:false}) 不泵出未确认内容（换 session 场景唯一补发 owner）', async () => {
    const io = makeIO();
    io.writeImpl.mockRejectedValueOnce(conflictErr);
    let resolveRead!: () => void;
    io.readImpl.mockImplementationOnce(
      () =>
        new Promise<FileEditRead>((res) => {
          resolveRead = () => res(makeRead('server-side\n', { baseHash: 'hS' }));
        }),
    );
    const s = makeSession(io);
    s.onEdit('v1\n');
    await s.flush();
    expect(s.status).toBe('blocked');
    const dp = s.discard();
    await sleep(1); // 等 discard 事务取得链并记录代际
    s.onEdit('v2\n'); // GET 在途期间编辑（editSeq 栅栏）
    resolveRead();
    const res = await dp;
    expect(res?.editedDuring).toBe(true);
    s.dispose({ flush: false }); // 旧 session 不得再写（新 session 负责补发）
    await sleep(30);
    expect(io.writes).toHaveLength(1); // 仅最初的冲突写，无旧 session 补发
    // 对照：默认 dispose 仍尽力 flush（F11 卸载路径语义不变）
    const io2 = makeIO();
    const s2 = makeSession(io2);
    s2.onEdit('x\n');
    s2.dispose();
    await sleep(30);
    expect(io2.writes.map((w) => w.content)).toEqual(['x\n']);
  });
});

describe('结果未知的恢复确认（四元重读）', () => {
  it('四元全等 → 采用新基线视为已确认，后续编辑立即补发', async () => {
    const io = makeIO();
    io.writeImpl.mockRejectedValueOnce(new Error('network down'));
    const s = makeSession(io, makeRead('a\nb\n'));
    // 重读返回与 sentContent 一致的内容（rename 已成功而响应丢失）
    io.readQueue.push(makeRead('v1\n', { baseHash: 'hNew', lineEnding: 'lf' }));
    s.onEdit('v1\n'); // 含 \n → lineEnding 参与比对
    await s.flush();
    expect(s.status).toBe('clean');
    expect(s.baseHash).toBe('hNew');
    // 后续编辑以新基线补发
    s.onEdit('v2\n');
    await s.flush();
    expect(io.writes[1]).toMatchObject({ content: 'v2\n', baseHash: 'hNew' });
    s.dispose();
  });

  it('在途期间继续编辑 + 响应丢失 → 确认成功后立即以新基线补发最新文档', async () => {
    const io = makeIO();
    let rejectFirst!: (e: Error) => void;
    io.writeImpl.mockImplementationOnce(
      () =>
        new Promise((_, rej) => {
          rejectFirst = rej;
        }),
    );
    const s = makeSession(io);
    io.readQueue.push(makeRead('v1\n', { baseHash: 'hNew' }));
    s.onEdit('v1\n');
    const p = s.flush();
    await sleep(1); // v1 写请求在途
    s.onEdit('v2\n'); // 在途期间继续编辑
    rejectFirst(new Error('network down')); // 响应丢失
    await p;
    expect(io.writes).toHaveLength(2);
    expect(io.writes[1]).toMatchObject({ content: 'v2\n', baseHash: 'hNew' });
    expect(s.status).toBe('clean');
    s.dispose();
  });

  it('内容不一致 → 保持阻塞，不丢编辑器内容', async () => {
    const io = makeIO();
    io.writeImpl.mockRejectedValueOnce(new Error('network down'));
    const s = makeSession(io);
    io.readQueue.push(makeRead('someone-else\n', { baseHash: 'hO' }));
    s.onEdit('v1\n');
    await s.flush();
    expect(s.status).toBe('blocked');
    expect(s.latest).toBe('v1\n');
    expect(io.writes).toHaveLength(1);
    s.dispose();
  });

  it('BOM 不一致仍阻塞（规范化文本相同也不够）', async () => {
    const io = makeIO();
    io.writeImpl.mockRejectedValueOnce(new Error('network down'));
    const s = makeSession(io, makeRead('a\nb\n', { hasBom: false }));
    io.readQueue.push(makeRead('v1\n', { baseHash: 'hNew', hasBom: true }));
    s.onEdit('v1\n');
    await s.flush();
    expect(s.status).toBe('blocked');
    s.dispose();
  });

  it('mode 与首次读取值不一致仍阻塞', async () => {
    const io = makeIO();
    io.writeImpl.mockRejectedValueOnce(new Error('network down'));
    const s = makeSession(io);
    io.readQueue.push(makeRead('v1\n', { baseHash: 'hNew', mode: '0755' }));
    s.onEdit('v1\n');
    await s.flush();
    expect(s.status).toBe('blocked');
    s.dispose();
  });

  it('sentContent 不含换行时 lineEnding 不参与比对', async () => {
    const io = makeIO();
    io.writeImpl.mockRejectedValueOnce(new Error('network down'));
    const s = makeSession(io, makeRead('single', { lineEnding: 'lf' }));
    // 即使重读报 crlf 也不参与（内容无换行），其余三项相等 → 确认
    io.readQueue.push(makeRead('single-x', { baseHash: 'hNew', lineEnding: 'crlf' }));
    s.onEdit('single-x'); // 无 \n
    await s.flush();
    expect(s.status).toBe('clean');
    expect(s.baseHash).toBe('hNew');
    s.dispose();
  });
});

describe('还原', () => {
  it('还原 = 用快照走同一写回端点（携带当前 baseHash）', async () => {
    const io = makeIO();
    const s = makeSession(io, makeRead('orig\n'));
    s.onEdit('v1\n');
    await s.flush();
    s.onEdit('v2\n');
    await s.flush();
    expect(io.writes).toHaveLength(2);
    const shown = await s.restore();
    expect(shown).toBe('orig\n');
    expect(io.writes).toHaveLength(3);
    expect(io.writes[2]).toMatchObject({ content: 'orig\n', baseHash: 'h2' });
    expect(s.latest).toBe('orig\n');
    expect(s.status).toBe('clean');
    s.dispose();
  });

  it('flush 前置：还原前先完成在途写', async () => {
    const io = makeIO();
    let resolveFirst!: (v: { baseHash: string }) => void;
    io.writeImpl.mockImplementationOnce(
      () =>
        new Promise((res) => {
          resolveFirst = res;
        }),
    );
    const s = makeSession(io, makeRead('orig\n'));
    s.onEdit('v1\n');
    const rp = s.restore();
    await sleep(1);
    expect(io.writes).toHaveLength(1); // 在途为 v1，还原等待其完成
    resolveFirst({ baseHash: 'hX' });
    await rp;
    expect(io.writes).toHaveLength(2);
    expect(io.writes[1]).toMatchObject({ content: 'orig\n', baseHash: 'hX' });
    s.dispose();
  });

  it('阻塞态还原被拒绝（先解决冲突）', async () => {
    const io = makeIO();
    io.writeImpl.mockRejectedValue(conflictErr);
    const s = makeSession(io, makeRead('orig\n'));
    s.onEdit('v1\n');
    await s.flush();
    expect(s.status).toBe('blocked');
    expect(await s.restore()).toBe(null);
    expect(io.writes).toHaveLength(1);
    s.dispose();
  });

  it('F2：还原写在途期间继续编辑 → 不覆盖 latest，确认基线推进后立即补发最新文本', async () => {
    const io = makeIO();
    const s = makeSession(io, makeRead('orig\n'));
    s.onEdit('v1\n');
    await s.flush();
    expect(io.writes).toHaveLength(1);
    // 还原写请求挂起（模拟慢网络）
    let resolveRestore!: (v: { baseHash: string }) => void;
    io.writeImpl.mockImplementationOnce(
      () =>
        new Promise((res) => {
          resolveRestore = res;
        }),
    );
    const rp = s.restore();
    await sleep(1);
    expect(io.writes).toHaveLength(2);
    expect(io.writes[1].content).toBe('orig\n'); // 还原写在途
    // 在途期间用户继续编辑
    s.onEdit('v2\n');
    resolveRestore({ baseHash: 'hR' });
    const shown = await rp;
    // 用户内容不被快照覆盖，且以还原后的新基线补发
    expect(s.latest).toBe('v2\n');
    expect(io.writes).toHaveLength(3);
    expect(io.writes[2]).toMatchObject({ content: 'v2\n', baseHash: 'hR' });
    expect(s.status).toBe('clean');
    expect(shown).toBe('v2\n');
    s.dispose();
  });

  it('F2：还原在途无新编辑 → latest 归位快照', async () => {
    const io = makeIO();
    const s = makeSession(io, makeRead('orig\n'));
    s.onEdit('v1\n');
    await s.flush();
    const shown = await s.restore();
    expect(shown).toBe('orig\n');
    expect(s.latest).toBe('orig\n');
    expect(s.confirmedContent).toBe('orig\n');
    expect(io.writes).toHaveLength(2);
    s.dispose();
  });

  it('F2：还原期间编辑后撤销回原值 → 事件标志判定，最终文本不被快照覆盖且仍补发', async () => {
    const io = makeIO();
    const s = makeSession(io, makeRead('orig\n'));
    s.onEdit('v1\n');
    await s.flush();
    expect(io.writes).toHaveLength(1);
    let resolveRestore!: (v: { baseHash: string }) => void;
    io.writeImpl.mockImplementationOnce(
      () =>
        new Promise((res) => {
          resolveRestore = res;
        }),
    );
    const rp = s.restore();
    await sleep(1);
    expect(io.writes[1].content).toBe('orig\n'); // 还原写在途
    // 编辑后 undo 回还原前文本（最终字符串相等，但发生过编辑）
    s.onEdit('temp\n');
    s.onEdit('v1\n');
    resolveRestore({ baseHash: 'hR' });
    const shown = await rp;
    // 服务端当前为快照，用户最终文本 v1 必须以新基线补发，不得被快照覆盖
    expect(s.latest).toBe('v1\n');
    expect(shown).toBe('v1\n');
    expect(io.writes).toHaveLength(3);
    expect(io.writes[2]).toMatchObject({ content: 'v1\n', baseHash: 'hR' });
    expect(s.status).toBe('clean');
    s.dispose();
  });

  it('F2：锁覆盖初始 flush——首个普通保存在途时调用 restore()，等待期间编辑仍补发', async () => {
    const io = makeIO();
    let resolveFirst!: (v: { baseHash: string }) => void;
    io.writeImpl.mockImplementationOnce(
      () =>
        new Promise((res) => {
          resolveFirst = res;
        }),
    );
    const s = makeSession(io, makeRead('orig\n'));
    s.onEdit('v1\n');
    const rp = s.restore(); // 初始 flush 等待 v1 在途写
    await sleep(1);
    expect(io.writes).toHaveLength(1);
    expect(io.writes[0].content).toBe('v1\n');
    // 初始 flush 等待期间的新编辑（锁定窗口前的历史漏洞点）
    s.onEdit('v2\n');
    resolveFirst({ baseHash: 'hX' });
    const shown = await rp;
    // 时序：v2 被初始 flush 保存 → 快照写 → 补发最终用户文本 v2
    expect(io.writes.map((w) => w.content)).toEqual(['v1\n', 'v2\n', 'orig\n', 'v2\n']);
    expect(io.writes[3].baseHash).toBe('h3'); // 快照写响应基线
    expect(s.latest).toBe('v2\n');
    expect(shown).toBe('v2\n');
    expect(s.status).toBe('clean');
    s.dispose();
  });

  it('F13：普通写在途时 restore() 排队——排队窗口内的编辑置标志，不被快照覆盖', async () => {
    const io = makeIO();
    let resolveW1!: (v: { baseHash: string }) => void;
    io.writeImpl.mockImplementationOnce(
      () =>
        new Promise((res) => {
          resolveW1 = res;
        }),
    );
    const s = makeSession(io, makeRead('orig\n'));
    s.onEdit('w1\n');
    const flushP = s.flush(); // w1 写在途但未完成
    await sleep(1);
    expect(io.writes).toHaveLength(1);
    const rp = s.restore(); // 排队等待 w1——锁在调用时即生效
    s.onEdit('w2\n'); // 排队窗口内编辑：MUST 只置标志，不得走普通 pump
    resolveW1({ baseHash: 'hX' });
    await flushP;
    const shown = await rp;
    // w2 不得被快照覆盖：最终保留并以最新基线补发
    expect(s.latest).toBe('w2\n');
    expect(shown).toBe('w2\n');
    const seq = io.writes.map((w) => w.content);
    expect(seq[0]).toBe('w1\n');
    expect(seq).toContain('orig\n'); // 快照写发生
    expect(seq[seq.length - 1]).toBe('w2\n'); // 最后一次写是补发 w2
    expect(s.status).toBe('clean');
    s.dispose();
  });
});

describe('还原事务的单在途与离开屏障（F10）', () => {
  /** 包装 io.write 统计并发在途数。 */
  function withConcurrencyProbe(io: ReturnType<typeof makeIO>) {
    let inFlight = 0;
    const probe = { max: 0 };
    const orig = io.write;
    io.write = async (input: FileEditWriteInput) => {
      inFlight++;
      probe.max = Math.max(probe.max, inFlight);
      try {
        return await orig(input);
      } finally {
        inFlight--;
      }
    };
    return probe;
  }

  it('还原在途时 canLeave 等待还原事务完成，全程最大并发写 = 1', async () => {
    const io = makeIO();
    const probe = withConcurrencyProbe(io);
    const s = makeSession(io, makeRead('orig\n'));
    s.onEdit('v1\n');
    await s.flush();
    let resolveRestore!: (v: { baseHash: string }) => void;
    io.writeImpl.mockImplementationOnce(
      () =>
        new Promise((res) => {
          resolveRestore = res;
        }),
    );
    const rp = s.restore();
    await sleep(1);
    expect(io.writes).toHaveLength(2); // v1 + 快照写在途
    // 还原在途期间调用 canLeave（退出/切文件路径）——必须阻塞等待
    let left = false;
    const lp = s.canLeave().then((ok) => {
      left = true;
      return ok;
    });
    await sleep(20);
    expect(left).toBe(false);
    resolveRestore({ baseHash: 'hR' });
    expect(await lp).toBe(true);
    expect(await rp).toBe('orig\n');
    expect(probe.max).toBe(1);
    s.dispose();
  });

  it('还原期间编辑后离开：补发与还原串行，无并发写', async () => {
    const io = makeIO();
    const probe = withConcurrencyProbe(io);
    const s = makeSession(io, makeRead('orig\n'));
    s.onEdit('v1\n');
    await s.flush();
    let resolveRestore!: (v: { baseHash: string }) => void;
    io.writeImpl.mockImplementationOnce(
      () =>
        new Promise((res) => {
          resolveRestore = res;
        }),
    );
    const rp = s.restore();
    await sleep(1);
    s.onEdit('v2\n'); // 还原在途期间编辑
    const lp = s.canLeave();
    await sleep(10);
    resolveRestore({ baseHash: 'hR' });
    expect(await rp).toBe('v2\n');
    expect(await lp).toBe(true);
    // 快照写 → 补发 v2，串行
    expect(io.writes.map((w) => w.content)).toEqual(['v1\n', 'orig\n', 'v2\n']);
    expect(probe.max).toBe(1);
    s.dispose();
  });
});

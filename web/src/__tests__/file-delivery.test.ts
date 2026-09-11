import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { ApiError, UPLOAD_TARGET_UNAVAILABLE_MESSAGE } from '../api';
import {
  createFileDeliveryController,
  extractDropFiles,
  extractPasteFiles,
  RECEIPT_TIMEOUT_MS,
  UNSUPPORTED_DELIVERY_MSG,
  type FileDeliveryController,
  type FileDeliveryPort,
} from '../terminal/file-delivery';
import type { DeliverResult, TermConnEvent } from '../terminal/session';

/* ==================== terminal-file-paste-drop 5.1：投递状态机与共享队列 ====================
 * 纯逻辑单测（Node）：port 为 fake，upload 可控 Promise，时钟用 fake timers。
 * 规则唯一来源：specs/terminal-file-delivery/spec.md「投递状态机与结果反馈」
 * 「多文件完整性与投递节奏」共享队列状态表。 */

const UUID_A = '11111111-1111-4111-8111-111111111111';
const UUID_B = '22222222-2222-4222-8222-222222222222';
const UPLOAD_A = 'a'.repeat(32);
const UPLOAD_B = 'b'.repeat(32);
const UPLOAD_C = 'c'.repeat(32);
const UPLOAD_D = 'd'.repeat(32);

function makeFile(name: string): File {
  return new File(['x'], name, { type: 'application/octet-stream' });
}

interface FakePort extends FileDeliveryPort {
  connId: string | null;
  gateOpen: boolean;
  gateMessage: string;
  sentDelivers: string[];
  emitConn(ev: TermConnEvent): void;
  emitResult(r: DeliverResult): void;
}

function createPort(): FakePort {
  const connCbs = new Set<(ev: TermConnEvent) => void>();
  const resultCbs = new Set<(r: DeliverResult) => void>();
  const port: FakePort = {
    connId: UUID_A,
    gateOpen: true,
    gateMessage: '终端未连接，请连接后重试',
    sentDelivers: [],
    emitConn(ev) {
      for (const cb of connCbs) cb(ev);
    },
    emitResult(r) {
      for (const cb of resultCbs) cb(r);
    },
    getConnId() {
      return port.connId;
    },
    fileGateOpen() {
      return port.gateOpen;
    },
    fileGateMessage() {
      return port.gateMessage;
    },
    sendDeliver(uploadId: string) {
      if (!port.gateOpen) return false;
      port.sentDelivers.push(uploadId);
      return true;
    },
    onConnEvent(cb) {
      connCbs.add(cb);
      return () => connCbs.delete(cb);
    },
    onDeliverResult(cb) {
      resultCbs.add(cb);
      return () => resultCbs.delete(cb);
    },
  };
  return port;
}

interface PendingUpload {
  taskID: string;
  file: File;
  connId: string;
  resolve: (r: { uploadId: string }) => void;
  reject: (e: unknown) => void;
}

function createHarness() {
  const port = createPort();
  const uploads: PendingUpload[] = [];
  const openPicker = vi.fn();
  const ctl: FileDeliveryController = createFileDeliveryController({
    taskID: 'task1',
    port,
    upload: (taskID, file, connId) =>
      new Promise((resolve, reject) => {
        uploads.push({ taskID, file, connId, resolve, reject });
      }),
    openFilePicker: openPicker,
  });
  return { port, uploads, openPicker, ctl };
}

function itemOf(ctl: FileDeliveryController, name: string) {
  const item = ctl.snapshot().items.find((i) => i.name === name);
  if (!item) throw new Error(`item ${name} not found`);
  return item;
}

/** 排空微任务队列（fake timers 下 pumpLoop / upload 续体的推进）。 */
const flush = () => vi.advanceTimersByTimeAsync(0);

/** 驱动一项走完 上传→deliver→awaiting。 */
async function driveToAwaiting(
  h: Pick<ReturnType<typeof createHarness>, 'port' | 'uploads'>,
  uploadId: string,
): Promise<void> {
  h.uploads[h.uploads.length - 1].resolve({ uploadId });
  await flush();
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('投递状态机迁移（正常链路）', () => {
  it('排队中（待上传）→ 上传中 → 待投递 → 等待回执 → 已发送到终端', async () => {
    const { port, uploads, ctl } = createHarness();
    const labels: string[] = [];
    ctl.subscribe(() => {
      const s = ctl.snapshot();
      if (s.items[0]) labels.push(s.items[0].label);
    });

    ctl.handleCapturedFiles([makeFile('a.png')]);
    expect(labels[0]).toBe('排队中（待上传）');
    expect(labels).toContain('上传中'); // pump 同步启动上传
    expect(uploads).toHaveLength(1);
    expect(uploads[0].taskID).toBe('task1');
    expect(uploads[0].connId).toBe(UUID_A); // 捕获当前 connId

    uploads[0].resolve({ uploadId: UPLOAD_A });
    await flush();
    expect(labels).toContain('待投递');
    expect(port.sentDelivers).toEqual([UPLOAD_A]);
    expect(labels).toContain('等待回执');
    expect(itemOf(ctl, 'a.png').status).toBe('awaiting');

    port.emitResult({ uploadId: UPLOAD_A, ok: true });
    await flush();
    const item = itemOf(ctl, 'a.png');
    expect(item.status).toBe('sent');
    expect(item.label).toBe('已发送到终端');
    expect(ctl.snapshot().paused).toBe(false);
  });

  it('多文件串行投递：等待上一项 deliver_result 后才上传/投递下一项', async () => {
    const { port, uploads, ctl } = createHarness();
    ctl.handleCapturedFiles([makeFile('1.png'), makeFile('2.png')]);
    expect(uploads).toHaveLength(1); // 逐个上传

    await driveToAwaiting({ port, uploads }, UPLOAD_A);
    expect(port.sentDelivers).toEqual([UPLOAD_A]);
    expect(uploads).toHaveLength(1); // 第二项仍等待第一项回执

    port.emitResult({ uploadId: UPLOAD_A, ok: true });
    await flush();
    expect(uploads).toHaveLength(2);
    uploads[1].resolve({ uploadId: UPLOAD_B });
    await flush();
    expect(port.sentDelivers).toEqual([UPLOAD_A, UPLOAD_B]);
  });
});

describe('门禁失败分类（D6：syntheticInFlight 固定 false，锁定例外不适用）', () => {
  it('锁定态：归"明确失败"、不调用上传/发送接口、提示先解锁', async () => {
    const { port, uploads, ctl } = createHarness();
    port.gateOpen = false;
    port.gateMessage = '终端已锁定，请先解锁后重试';
    ctl.handleCapturedFiles([makeFile('a.png')]);
    await flush();
    const item = itemOf(ctl, 'a.png');
    expect(item.status).toBe('failed');
    expect(item.detail).toBe('终端已锁定，请先解锁后重试');
    expect(item.canRetry).toBe(true);
    expect(uploads).toHaveLength(0);
    expect(port.sentDelivers).toHaveLength(0);
  });

  it('未连接：归"明确失败"、不调用上传/发送接口', async () => {
    const { port, uploads, ctl } = createHarness();
    port.gateOpen = false;
    ctl.handleCapturedFiles([makeFile('a.png')]);
    await flush();
    expect(itemOf(ctl, 'a.png').detail).toBe('终端未连接，请连接后重试');
    expect(uploads).toHaveLength(0);
    expect(port.sentDelivers).toHaveLength(0);
  });

  it('单项门禁失败继续后续项，后续项按各自门禁继续判定', async () => {
    const { port, uploads, ctl } = createHarness();
    ctl.handleCapturedFiles([makeFile('1.png'), makeFile('2.png')]);
    uploads[0].resolve({ uploadId: UPLOAD_A });
    await flush(); // 1 awaiting
    port.gateOpen = false; // 回执后处理 2 时门禁关闭
    port.emitResult({ uploadId: UPLOAD_A, ok: true });
    await flush();
    expect(itemOf(ctl, '1.png').status).toBe('sent'); // 已成功项不受影响
    expect(itemOf(ctl, '2.png').status).toBe('failed');
  });

  it('deliver 前门禁复查：上传成功但随后锁定 → 明确失败、不发送 deliver', async () => {
    const { port, uploads, ctl } = createHarness();
    ctl.handleCapturedFiles([makeFile('a.png')]);
    port.gateOpen = false; // 上传期间锁定
    uploads[0].resolve({ uploadId: UPLOAD_A });
    await flush();
    expect(itemOf(ctl, 'a.png').status).toBe('failed');
    expect(port.sentDelivers).toHaveLength(0);
  });
});

describe('明确失败与手动重试（重试 = 重新上传新 uploadId）', () => {
  it('上传失败归明确失败；重试重新上传并取得新 uploadId', async () => {
    const { port, uploads, ctl } = createHarness();
    ctl.handleCapturedFiles([makeFile('a.png')]);
    uploads[0].reject(new ApiError(500, 'internal', '磁盘写失败'));
    await flush();
    expect(itemOf(ctl, 'a.png').status).toBe('failed');
    expect(itemOf(ctl, 'a.png').detail).toBe('磁盘写失败');

    ctl.retryItem(itemOf(ctl, 'a.png').id);
    await flush();
    expect(uploads).toHaveLength(2);
    uploads[1].resolve({ uploadId: UPLOAD_B });
    await flush();
    expect(port.sentDelivers).toEqual([UPLOAD_B]); // 新 uploadId，旧尝试永久结束
  });

  it('上传 404：固定文案提示、仅终止当前项、不永久降级', async () => {
    const { uploads, ctl } = createHarness();
    ctl.handleCapturedFiles([makeFile('a.png')]);
    uploads[0].reject(new ApiError(404, 'not_found', '任务不存在'));
    await flush();
    expect(itemOf(ctl, 'a.png').detail).toBe(UPLOAD_TARGET_UNAVAILABLE_MESSAGE);
    expect(ctl.snapshot().notice).toBe(UPLOAD_TARGET_UNAVAILABLE_MESSAGE);

    // 文件能力不因此永久降级：后续新上传仍可尝试
    ctl.handleCapturedFiles([makeFile('b.png')]);
    expect(uploads).toHaveLength(2);
    uploads[1].resolve({ uploadId: UPLOAD_B });
    await flush();
    expect(itemOf(ctl, 'b.png').status).toBe('awaiting');
  });

  it('上传响应时 connId 已换代（旧代次、从未发送 deliver）→ 明确失败', async () => {
    const { port, uploads, ctl } = createHarness();
    ctl.handleCapturedFiles([makeFile('a.png')]);
    port.connId = UUID_B; // 重连换代
    uploads[0].resolve({ uploadId: UPLOAD_A });
    await flush();
    expect(itemOf(ctl, 'a.png').status).toBe('failed');
    expect(port.sentDelivers).toHaveLength(0); // 不自动重发、不转投新连接
  });

  it('注入前准入拒绝（forbidden）归明确失败，不触发队列暂停', async () => {
    const { port, uploads, ctl } = createHarness();
    ctl.handleCapturedFiles([makeFile('a.png')]);
    await driveToAwaiting({ port, uploads }, UPLOAD_A);
    port.emitResult({ uploadId: UPLOAD_A, ok: false, error: 'forbidden' });
    await flush();
    expect(itemOf(ctl, 'a.png').status).toBe('failed');
    expect(ctl.snapshot().paused).toBe(false);
  });
});

describe('结果未知与共享队列暂停（队列级）', () => {
  it('回执超时 → 结果未知 + 队列暂停；暂停期新项只入队（排队中（待上传））', async () => {
    const { uploads, ctl } = createHarness();
    ctl.handleCapturedFiles([makeFile('1.png'), makeFile('2.png')]);
    uploads[0].resolve({ uploadId: UPLOAD_A });
    await flush(); // 1 awaiting
    await vi.advanceTimersByTimeAsync(RECEIPT_TIMEOUT_MS);

    const item1 = itemOf(ctl, '1.png');
    expect(item1.status).toBe('unknown');
    expect(item1.detail).toBe('回执超时');
    expect(item1.hint).toBe('重试可能重复');
    expect(ctl.snapshot().paused).toBe(true);

    // 暂停期新入队项：仅入队、不自动发送，不隐式替代未知项
    ctl.handleCapturedFiles([makeFile('3.png')]);
    await flush();
    const item3 = itemOf(ctl, '3.png');
    expect(item3.status).toBe('queued');
    expect(item3.label).toBe('排队中（待上传）');
    expect(uploads).toHaveLength(1);
  });

  it('write_failed → 结果未知（可能部分写入）+ 暂停；重试提示先检查终端输入框、可能重复', async () => {
    const { port, uploads, ctl } = createHarness();
    ctl.handleCapturedFiles([makeFile('a.png')]);
    await driveToAwaiting({ port, uploads }, UPLOAD_A);
    port.emitResult({ uploadId: UPLOAD_A, ok: false, error: 'write_failed' });
    await flush();
    const item = itemOf(ctl, 'a.png');
    expect(item.status).toBe('unknown');
    expect(item.detail).toBe('写入失败，可能部分写入');
    expect(item.hint).toContain('先检查终端输入框');
    expect(item.hint).toContain('重复');
    expect(ctl.snapshot().paused).toBe(true);
  });

  it('等待回执期间断线 → 结果未知 + 暂停；已成功项 MUST NOT 改写', async () => {
    const { port, uploads, ctl } = createHarness();
    ctl.handleCapturedFiles([makeFile('1.png')]);
    await driveToAwaiting({ port, uploads }, UPLOAD_A);
    port.emitResult({ uploadId: UPLOAD_A, ok: true });
    await flush(); // 1 sent
    ctl.handleCapturedFiles([makeFile('2.png')]);
    await driveToAwaiting({ port, uploads }, UPLOAD_B); // 2 awaiting

    port.emitConn({ type: 'closed' });
    await flush();
    expect(itemOf(ctl, '1.png').status).toBe('sent'); // 普通断线不改写已成功项
    expect(itemOf(ctl, '2.png').status).toBe('unknown');
    expect(itemOf(ctl, '2.png').detail).toBe('连接已断开');
    expect(ctl.snapshot().paused).toBe(true);
  });

  it('重试未知项取得确定结果 → 解除暂停；恢复后未上传项先上传取得 uploadId 再 deliver', async () => {
    const { port, uploads, ctl } = createHarness();
    ctl.handleCapturedFiles([makeFile('1.png')]);
    await driveToAwaiting({ port, uploads }, UPLOAD_A);
    await vi.advanceTimersByTimeAsync(RECEIPT_TIMEOUT_MS); // 1 unknown → paused
    ctl.handleCapturedFiles([makeFile('2.png')]); // 暂停期入队
    await flush();

    ctl.retryItem(itemOf(ctl, '1.png').id); // 恢复尝试
    await flush();
    expect(uploads).toHaveLength(2);
    uploads[1].resolve({ uploadId: UPLOAD_B });
    await flush();
    port.emitResult({ uploadId: UPLOAD_B, ok: true });
    await flush();
    expect(itemOf(ctl, '1.png').status).toBe('sent');
    expect(ctl.snapshot().paused).toBe(false); // 仅未知项恢复尝试解除暂停

    // 队列恢复自动推进：2 先上传取得 uploadId，再 deliver
    await flush();
    expect(uploads).toHaveLength(3);
    uploads[2].resolve({ uploadId: UPLOAD_C });
    await flush();
    expect(port.sentDelivers).toContain(UPLOAD_C);
    expect(itemOf(ctl, '2.png').status).toBe('awaiting');
  });

  it('暂停期重试"明确失败"项取得确定结果 MUST NOT 解除暂停；未知项恢复才解除', async () => {
    const { port, uploads, ctl } = createHarness();
    // A 上传失败 → 明确失败
    ctl.handleCapturedFiles([makeFile('a.png')]);
    uploads[0].reject(new ApiError(500, 'internal', 'x'));
    await flush();
    // B 回执超时 → 未知 + 暂停
    ctl.handleCapturedFiles([makeFile('b.png')]);
    uploads[1].resolve({ uploadId: UPLOAD_A });
    await flush();
    await vi.advanceTimersByTimeAsync(RECEIPT_TIMEOUT_MS);
    expect(ctl.snapshot().paused).toBe(true);

    // 暂停期重试 A（明确失败）：进入同一串行重试调度并取得确定结果
    ctl.retryItem(itemOf(ctl, 'a.png').id);
    await flush();
    expect(uploads).toHaveLength(3);
    uploads[2].resolve({ uploadId: UPLOAD_B });
    await flush();
    port.emitResult({ uploadId: UPLOAD_B, ok: true });
    await flush();
    expect(itemOf(ctl, 'a.png').status).toBe('sent');
    expect(ctl.snapshot().paused).toBe(true); // MUST NOT 解除既有暂停

    // 重试 B（未知）取得确定结果 → 队列恢复
    ctl.retryItem(itemOf(ctl, 'b.png').id);
    await flush();
    expect(uploads).toHaveLength(4);
    uploads[3].resolve({ uploadId: UPLOAD_D });
    await flush();
    port.emitResult({ uploadId: UPLOAD_D, ok: true });
    await flush();
    expect(itemOf(ctl, 'b.png').status).toBe('sent');
    expect(ctl.snapshot().paused).toBe(false);
  });

  it('未知项恢复尝试再次得到确定失败 → 同样解除暂停', async () => {
    const { port, uploads, ctl } = createHarness();
    ctl.handleCapturedFiles([makeFile('a.png')]);
    await driveToAwaiting({ port, uploads }, UPLOAD_A);
    await vi.advanceTimersByTimeAsync(RECEIPT_TIMEOUT_MS); // unknown → paused
    ctl.retryItem(itemOf(ctl, 'a.png').id);
    uploads[1].resolve({ uploadId: UPLOAD_B });
    await flush();
    port.emitResult({ uploadId: UPLOAD_B, ok: false, error: 'task_inactive' });
    await flush();
    expect(itemOf(ctl, 'a.png').status).toBe('failed');
    expect(ctl.snapshot().paused).toBe(false);
  });

  it('队列恢复后其他未知项未被单独重试前 MUST NOT 被补发', async () => {
    const { port, uploads, ctl } = createHarness();
    // A 未知 → 暂停；重试 A → forbidden（明确失败）→ 恢复（恢复尝试确定结果）
    ctl.handleCapturedFiles([makeFile('a.png')]);
    await driveToAwaiting({ port, uploads }, UPLOAD_A);
    await vi.advanceTimersByTimeAsync(RECEIPT_TIMEOUT_MS);
    ctl.retryItem(itemOf(ctl, 'a.png').id);
    uploads[1].resolve({ uploadId: UPLOAD_B });
    await flush();
    port.emitResult({ uploadId: UPLOAD_B, ok: false, error: 'forbidden' });
    await flush();
    expect(itemOf(ctl, 'a.png').status).toBe('failed');
    expect(ctl.snapshot().paused).toBe(false);

    // 后续项正常推进，已终态的 A 不再被上传/投递
    ctl.handleCapturedFiles([makeFile('b.png')]);
    await flush();
    expect(uploads).toHaveLength(3); // 仅 B 的新上传
    uploads[2].resolve({ uploadId: UPLOAD_C });
    await flush();
    // A 仅首试投递过一次（UPLOAD_A），未被补发
    expect(port.sentDelivers).toEqual([UPLOAD_A, UPLOAD_B, UPLOAD_C]);
  });

  it('重复点击同一项 MUST NOT 创建并发尝试', async () => {
    const { port, uploads, ctl } = createHarness();
    ctl.handleCapturedFiles([makeFile('a.png')]);
    await driveToAwaiting({ port, uploads }, UPLOAD_A);
    await vi.advanceTimersByTimeAsync(RECEIPT_TIMEOUT_MS); // unknown → paused
    const id = itemOf(ctl, 'a.png').id;

    ctl.retryItem(id);
    ctl.retryItem(id); // 已在新尝试中 → no-op
    expect(uploads).toHaveLength(2);
    uploads[1].resolve({ uploadId: UPLOAD_B });
    await flush();
    ctl.retryItem(id); // awaiting → no-op
    expect(uploads).toHaveLength(2);
    // 仅首轮 UPLOAD_A + 重试的 UPLOAD_B 两次投递，无并发尝试
    expect(port.sentDelivers).toEqual([UPLOAD_A, UPLOAD_B]);
  });

  it('File 不可用 → 打开绑定该项重试意图的选择器；取消保持原状态，选定创建新尝试', async () => {
    const { uploads, openPicker, ctl } = createHarness();
    ctl.handleDroppedFiles([], ['mydir']);
    await flush();
    const item = itemOf(ctl, 'mydir');
    expect(item.status).toBe('failed');
    expect(item.detail).toBe('不支持目录');

    ctl.retryItem(item.id); // file=null → File 不可用路径
    expect(openPicker).toHaveBeenCalledTimes(1);
    expect(itemOf(ctl, 'mydir').status).toBe('failed');

    ctl.handlePickerResult(null); // 用户取消选择 → 保持原状态
    expect(itemOf(ctl, 'mydir').status).toBe('failed');

    ctl.retryItem(item.id);
    ctl.handlePickerResult([makeFile('replacement.png')]); // 选定 → 新尝试
    await flush();
    expect(uploads).toHaveLength(1);
    expect(ctl.snapshot().items[0].name).toBe('replacement.png');
    uploads[0].resolve({ uploadId: UPLOAD_A });
    await flush();
    expect(itemOf(ctl, 'replacement.png').status).toBe('awaiting');
  });
});

describe('迟到回执（状态已迁移 / uploadId 已换代）', () => {
  it('重试后旧 uploadId 回执到达：只丢弃 + 日志，不改变项状态与队列状态', async () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      const { port, uploads, ctl } = createHarness();
      ctl.handleCapturedFiles([makeFile('a.png')]);
      await driveToAwaiting({ port, uploads }, UPLOAD_A);
      await vi.advanceTimersByTimeAsync(RECEIPT_TIMEOUT_MS); // unknown（UPLOAD_A 尝试已结束）
      ctl.retryItem(itemOf(ctl, 'a.png').id);
      uploads[1].resolve({ uploadId: UPLOAD_B });
      await flush(); // 新尝试 awaiting（UPLOAD_B）

      port.emitResult({ uploadId: UPLOAD_A, ok: true }); // 迟到回执
      await flush();
      expect(itemOf(ctl, 'a.png').status).toBe('awaiting'); // 状态不被改写
      expect(ctl.snapshot().paused).toBe(true); // 队列状态不被改变

      port.emitResult({ uploadId: UPLOAD_B, ok: true });
      await flush();
      expect(itemOf(ctl, 'a.png').status).toBe('sent');
      expect(warnSpy).toHaveBeenCalled();
    } finally {
      warnSpy.mockRestore();
    }
  });

  it('已发送项收到迟到的 write_failed 回执：保持已发送、不触发队列暂停', async () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      const { port, uploads, ctl } = createHarness();
      ctl.handleCapturedFiles([makeFile('a.png')]);
      await driveToAwaiting({ port, uploads }, UPLOAD_A);
      port.emitResult({ uploadId: UPLOAD_A, ok: true });
      await flush(); // sent

      port.emitResult({ uploadId: UPLOAD_A, ok: false, error: 'write_failed' }); // 迟到
      await flush();
      expect(itemOf(ctl, 'a.png').status).toBe('sent');
      expect(ctl.snapshot().paused).toBe(false);
      expect(warnSpy).toHaveBeenCalled();
    } finally {
      warnSpy.mockRestore();
    }
  });
});

describe('connId 能力降级（D7 两路径中的无 connId 路径）', () => {
  it('auth_ok 无 connId：捕获文件 → 固定提示、不入队、不上传、不发送 deliver', async () => {
    const { port, uploads, ctl } = createHarness();
    port.connId = null;
    ctl.handleCapturedFiles([makeFile('a.png')]);
    await flush();
    expect(ctl.snapshot().notice).toBe(UNSUPPORTED_DELIVERY_MSG);
    expect(ctl.snapshot().items).toHaveLength(0);
    expect(uploads).toHaveLength(0);
    expect(port.sentDelivers).toHaveLength(0);

    ctl.handleDroppedFiles([makeFile('b.png')], ['d']);
    await flush();
    expect(uploads).toHaveLength(0);
  });

  it('auth_ok 恢复有效 connId 后提示清除，可正常上传', async () => {
    const { port, uploads, ctl } = createHarness();
    port.connId = null;
    ctl.handleCapturedFiles([makeFile('a.png')]);
    expect(ctl.snapshot().notice).toBe(UNSUPPORTED_DELIVERY_MSG);

    port.connId = UUID_A;
    port.emitConn({ type: 'auth_ok', connId: UUID_A });
    expect(ctl.snapshot().notice).toBeNull();

    ctl.handleCapturedFiles([makeFile('a.png')]);
    await driveToAwaiting({ port, uploads }, UPLOAD_A);
    expect(itemOf(ctl, 'a.png').status).toBe('awaiting');
  });
});

describe('拖拽捕获提取（D7：目录分项拒绝、不遍历目录）', () => {
  it('extractPasteFiles：items 有效时不合并 files 兜底', () => {
    const itemFile = makeFile('item.png');
    const filesFile = makeFile('files.png');
    const dt = {
      items: [
        { kind: 'file', getAsFile: () => itemFile },
        { kind: 'string', getAsFile: () => null },
      ],
      files: [filesFile],
    };
    expect(extractPasteFiles(dt)).toEqual([itemFile]);
  });

  it('extractPasteFiles：items 无有效 File 时才读 files 兜底；纯文本返回空', () => {
    const f = makeFile('fallback.png');
    expect(
      extractPasteFiles({ items: [{ kind: 'string', getAsFile: () => null }], files: [f] }),
    ).toEqual([f]);
    expect(extractPasteFiles({ items: [{ kind: 'string', getAsFile: () => null }], files: [] })).toEqual([]);
    expect(extractPasteFiles(null)).toEqual([]);
  });

  it('extractDropFiles：目录项分项拒绝、普通文件继续', () => {
    const f2 = makeFile('f2.png');
    const dt = {
      items: [
        {
          kind: 'file',
          getAsFile: () => null,
          webkitGetAsEntry: () => ({ isDirectory: true, name: 'mydir' }),
        },
        {
          kind: 'file',
          getAsFile: () => f2,
          webkitGetAsEntry: () => ({ isDirectory: false, name: 'f2.png' }),
        },
      ],
      files: [],
    };
    expect(extractDropFiles(dt)).toEqual({ files: [f2], directories: ['mydir'] });
  });

  it('extractDropFiles：items 为空时兜底 files', () => {
    const f = makeFile('x.png');
    expect(extractDropFiles({ items: [], files: [f] })).toEqual({ files: [f], directories: [] });
  });

  it('handleDroppedFiles：目录项归明确失败（不支持目录），普通文件继续上传投递', async () => {
    const { uploads, ctl } = createHarness();
    ctl.handleDroppedFiles([makeFile('ok.png')], ['mydir']);
    await flush();
    const dir = itemOf(ctl, 'mydir');
    expect(dir.status).toBe('failed');
    expect(dir.detail).toBe('不支持目录');
    expect(uploads).toHaveLength(1); // 普通文件继续，不遍历目录
    uploads[0].resolve({ uploadId: UPLOAD_A });
    await flush();
    expect(itemOf(ctl, 'ok.png').status).toBe('awaiting');
  });
});

describe('文件读取失效 → 重新选择文件路径（W2）', () => {
  it.each(['NotReadableError', 'NotFoundError'])(
    'size 可访问但读取/上传失败（%s）→ File 置不可用、归明确失败；重试走绑定选择器而非重新上传',
    async (name) => {
      const { uploads, openPicker, ctl } = createHarness();
      ctl.handleCapturedFiles([makeFile('dead.png')]);
      uploads[0].reject(new DOMException('source unavailable', name));
      await flush();
      const item = itemOf(ctl, 'dead.png');
      expect(item.status).toBe('failed');
      expect(item.detail).toBe('文件不可读取，请重新选择');

      // 原始 File 不再用于重试：进入重新选择路径（绑定该项重试意图的选择器）
      ctl.retryItem(item.id);
      expect(openPicker).toHaveBeenCalledTimes(1);
      expect(uploads).toHaveLength(1); // 未重新上传已失效的 File
      expect(ctl.snapshot().pickerPendingItem).toBe(item.id);

      ctl.handlePickerResult([makeFile('fresh.png')]);
      await flush();
      expect(ctl.snapshot().pickerPendingItem).toBeNull();
      expect(uploads).toHaveLength(2); // 新 File 走新尝试
      uploads[1].resolve({ uploadId: UPLOAD_A });
      await flush();
      expect(itemOf(ctl, 'fresh.png').status).toBe('awaiting');
    },
  );

  it('普通上传错误（无 name 类别）不走重新选择路径：重试仍复用原 File', async () => {
    const { uploads, openPicker, ctl } = createHarness();
    ctl.handleCapturedFiles([makeFile('a.png')]);
    uploads[0].reject(new TypeError('network down'));
    await flush();
    ctl.retryItem(itemOf(ctl, 'a.png').id);
    await flush();
    expect(openPicker).not.toHaveBeenCalled();
    expect(uploads).toHaveLength(2); // 复用原 File 重新上传
  });
});

describe('在途选择器绑定不被覆盖（W3）', () => {
  it('两个失败项交替点重试：在途绑定保持，文件回填到正确项', async () => {
    const { openPicker, ctl } = createHarness();
    ctl.handleDroppedFiles([], ['dirA', 'dirB']);
    await flush();
    const a = itemOf(ctl, 'dirA');
    const b = itemOf(ctl, 'dirB');

    ctl.retryItem(a.id);
    expect(openPicker).toHaveBeenCalledTimes(1);
    expect(ctl.snapshot().pickerPendingItem).toBe(a.id);

    ctl.retryItem(b.id); // 在途绑定 dirA：拒绝启动第二个选择器、不覆盖绑定
    expect(openPicker).toHaveBeenCalledTimes(1);
    expect(ctl.snapshot().pickerPendingItem).toBe(a.id);

    ctl.handlePickerResult([makeFile('a-file.png')]); // 回填到 dirA
    const rebound = ctl.snapshot().items.find((i) => i.id === a.id)!;
    expect(rebound.name).toBe('a-file.png');
    expect(b.name).toBe('dirB'); // B 未被误绑定
    expect(ctl.snapshot().pickerPendingItem).toBeNull();

    // B 随后走自己的选择器并回填
    ctl.retryItem(b.id);
    expect(openPicker).toHaveBeenCalledTimes(2);
    ctl.handlePickerResult([makeFile('b-file.png')]);
    const reboundB = ctl.snapshot().items.find((i) => i.id === b.id)!;
    expect(reboundB.name).toBe('b-file.png');
  });

  it('同项重复打开选择器幂等；取消只清理对应绑定', async () => {
    const { openPicker, ctl } = createHarness();
    ctl.handleDroppedFiles([], ['dirA']);
    await flush();
    const a = itemOf(ctl, 'dirA');

    ctl.retryItem(a.id);
    ctl.retryItem(a.id); // 同项重复 → 幂等再开，不产生第二个绑定
    expect(openPicker).toHaveBeenCalledTimes(2);
    expect(ctl.snapshot().pickerPendingItem).toBe(a.id);

    ctl.handlePickerResult(null); // 用户取消：绑定解除、项保持原状态
    expect(ctl.snapshot().pickerPendingItem).toBeNull();
    expect(a.status).toBe('failed');
    expect(a.detail).toBe('不支持目录');
  });
});

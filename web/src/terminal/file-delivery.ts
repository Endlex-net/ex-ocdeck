/**
 * 浏览器侧文件捕获 → HTTP 上传 → deliver 投递状态机（terminal-file-paste-drop D7）。
 *
 * 纯逻辑模块：不依赖 DOM / xterm。事件捕获归 TerminalView、HTTP 归 api.uploadAttachment、
 * deliver 文本帧归 TermSession.sendDeliver；本模块经 FileDeliveryPort 端口与其协作。
 *
 * 状态机与共享队列规则的唯一来源（本文件逐字实现，不另立规则）：
 * openspec/changes/terminal-file-paste-drop/specs/terminal-file-delivery/spec.md
 * 「投递状态机与结果反馈」「多文件完整性与投递节奏」。
 */

import { ApiError, UPLOAD_TARGET_UNAVAILABLE_MESSAGE, isFileReadFailure } from '../api';
import type { DeliverResult, TermConnEvent } from './session';

/** auth_ok 缺失/非法（空串/非 uuid 形态）时捕获到文件的用户提示（能力降级路径）。 */
export const UNSUPPORTED_DELIVERY_MSG = '当前 server 版本不支持文件投递';
/** 浏览器侧等待 deliver_result 的时限（与服务端 5s 写 deadline 为独立两层时限）。 */
export const RECEIPT_TIMEOUT_MS = 10_000;

export type FileItemStatus =
  | 'queued' // 排队中（待上传）：尚未取得 uploadId（与已取得 uploadId 的"待投递"区分）
  | 'uploading' // 上传中
  | 'ready' // 待投递：已取得 uploadId，deliver 未发
  | 'awaiting' // 等待回执：deliver 已发
  | 'sent' // 已发送到终端（当前请求 + 当前 connId 匹配的 ok:true）
  | 'failed' // 明确失败（注入前准入拒绝 / 上传失败 / 从未发送 deliver 的旧代次操作 / 本地门禁拒绝）
  | 'unknown'; // 结果未知（回执超时 / 等待回执期间断线 / write_failed）

export const FILE_STATUS_LABEL: Record<FileItemStatus, string> = {
  queued: '排队中（待上传）',
  uploading: '上传中',
  ready: '待投递',
  awaiting: '等待回执',
  sent: '已发送到终端',
  failed: '明确失败',
  unknown: '结果未知',
};

export type FileDeliveryConnEvent = TermConnEvent;

/** 文件投递对终端会话的窄端口（TermSession 结构化实现）。 */
export interface FileDeliveryPort {
  /** 当前连接 connId；auth_ok 缺失/非法（空串/非 uuid）→ null，文件能力降级。 */
  getConnId(): string | null;
  /** 上传/投递共用的输入门禁同款判定（syntheticInFlight 固定 false，D6）。 */
  fileGateOpen(): boolean;
  /** 门禁不通过时的用户提示（锁定 → 提示解锁；未连接 → 提示连接）。 */
  fileGateMessage(): string;
  /** 发送 deliver 文本帧；发送前复查门禁，不通过返回 false 且不发送。 */
  sendDeliver(uploadId: string): boolean;
  /** 连接事件订阅（auth_ok / 断开）。 */
  onConnEvent(cb: (ev: FileDeliveryConnEvent) => void): () => void;
  /** deliver_result 回执订阅（仅当前连接的合法帧）。 */
  onDeliverResult(cb: (r: DeliverResult) => void): () => void;
}

export interface FileDeliveryDeps {
  taskID: string;
  port: FileDeliveryPort;
  /** HTTP 上传（api.uploadAttachment）；uploadId 为服务端签发 32hex。 */
  upload: (taskID: string, file: File, connId: string) => Promise<{ uploadId: string }>;
  /** 打开绑定重试意图的文件选择器（原 File 不可用路径）；结果经 handlePickerResult 回流。 */
  openFilePicker: () => void;
  /** 回执超时定时器注入（默认 setTimeout；测试可控时钟）。 */
  schedule?: (fn: () => void, delayMs: number) => { cancel(): void };
}

export interface FileItemView {
  id: string;
  name: string;
  status: FileItemStatus;
  label: string;
  /** 失败原因 / 未知原因说明。 */
  detail: string | null;
  /** 结果未知项的重试提示（可能重复；write_failed 另提示先检查终端输入框）。 */
  hint: string | null;
  canRetry: boolean;
}

export interface FileDeliverySnapshot {
  items: readonly FileItemView[];
  /** 队列级暂停：任一项进入"结果未知"后暂停自动推进，仅未知项恢复尝试的确定结果可解除。 */
  paused: boolean;
  /** 能力降级 / 上传 404 等入口级提示。 */
  notice: string | null;
  /** 在途选择器请求绑定的项（File 不可用重试路径）；非 null 时其他项的重试按钮应禁用。 */
  pickerPendingItem: string | null;
}

export interface FileDeliveryController {
  /** paste / 文件选择器 / drop 提取后的普通文件入口。 */
  handleCapturedFiles(files: readonly File[]): void;
  /** drop 入口：目录项已由提取层分项拒绝（directories 为目录名），普通文件继续处理。 */
  handleDroppedFiles(files: readonly File[], directories: readonly string[]): void;
  /** 文件选择器结果回流：有绑定重试意图 → 为该项创建新尝试；用户取消（null）→ 该项保持原状态。 */
  handlePickerResult(files: readonly File[] | null): void;
  /** 手动重试 = 重新上传取得新 uploadId；原 File 不可用时转绑定该项重试意图的选择器。 */
  retryItem(id: string): void;
  snapshot(): FileDeliverySnapshot;
  subscribe(cb: () => void): () => void;
  dispose(): void;
}

interface FileItem {
  id: string;
  name: string;
  file: File | null;
  status: FileItemStatus;
  detail: string | null;
  /** 结果未知原因（write_failed 的重试提示须含"先检查终端输入框"）。 */
  unknownCause: 'write_failed' | 'timeout' | 'disconnected' | null;
  uploadId: string | null;
  /** 本尝试上传时捕获的 connId（代次绑定，D5）。 */
  capturedConnId: string | null;
  /** 手动重试创建的尝试：暂停期间允许进入串行重试调度（暂停期新入队项不可）。 */
  retryIntent: boolean;
  /** 重试前状态为"结果未知" → 本次尝试取得确定结果后解除队列暂停。 */
  resumeEligible: boolean;
}

/** 注入前准入拒绝（确定零 PTY 写入）→ 明确失败；write_failed → 结果未知。 */
const DELIVER_ERROR_MESSAGES: Record<string, string> = {
  forbidden: '投递被拒绝（连接或终端不匹配）',
  task_inactive: '任务已挂起或已删除',
  not_found: '上传文件不存在或已失效',
  expired: '文件已过保留期',
  invalid_input: '投递内容校验失败',
};

function deliverErrorMessage(error: string | undefined): string {
  if (error && DELIVER_ERROR_MESSAGES[error]) return DELIVER_ERROR_MESSAGES[error];
  return error ? `投递失败（${error}）` : '投递失败';
}

function fileUsable(file: File | null): boolean {
  if (!file) return false;
  try {
    return typeof file.size === 'number';
  } catch {
    return false; // File 对象已失效（如底层句柄被回收）
  }
}

export function createFileDeliveryController(deps: FileDeliveryDeps): FileDeliveryController {
  const schedule =
    deps.schedule ??
    ((fn: () => void, delayMs: number) => {
      const id = setTimeout(fn, delayMs);
      return { cancel: () => clearTimeout(id) };
    });

  let disposed = false;
  let seq = 0;
  let paused = false;
  let notice: string | null = null;
  const items: FileItem[] = [];
  const subscribers = new Set<() => void>();

  /** 当前处于"等待回执"的项（串行节奏：同时最多一个待确认投递）。 */
  let awaitingId: string | null = null;
  let receiptTimer: { cancel(): void } | null = null;
  /** 绑定重试意图的文件选择器待回填项（File 不可用重试路径）；同一时刻至多一个。 */
  let pendingRetryId: string | null = null;
  let pumping = false;

  function emit(): void {
    if (disposed) return;
    for (const cb of subscribers) cb();
  }

  function view(item: FileItem): FileItemView {
    return {
      id: item.id,
      name: item.name,
      status: item.status,
      label: FILE_STATUS_LABEL[item.status],
      detail: item.detail,
      hint:
        item.status !== 'unknown'
          ? null
          : item.unknownCause === 'write_failed'
            ? '重试可能重复，请先检查终端输入框'
            : '重试可能重复',
      canRetry: item.status === 'failed' || item.status === 'unknown',
    };
  }

  function enqueue(file: File): FileItem {
    const item: FileItem = {
      id: `fd${++seq}`,
      name: file.name || '(未命名)',
      file,
      status: 'queued',
      detail: null,
      unknownCause: null,
      uploadId: null,
      capturedConnId: null,
      retryIntent: false,
      resumeEligible: false,
    };
    items.push(item);
    return item;
  }

  function enqueueRejected(name: string, detail: string): FileItem {
    const item: FileItem = {
      id: `fd${++seq}`,
      name,
      file: null,
      status: 'failed',
      detail,
      unknownCause: null,
      uploadId: null,
      capturedConnId: null,
      retryIntent: false,
      resumeEligible: false,
    };
    items.push(item);
    return item;
  }

  /**
   * 项进入终态/未知。未知 → 队列级暂停（共享队列状态表第一行）；
   * 确定结果仅当该项是"结果未知"项的恢复尝试（resumeEligible）时解除暂停
   * （重试"明确失败"项的确定结果 MUST NOT 解除既有暂停）。
   */
  function settle(
    item: FileItem,
    status: 'sent' | 'failed' | 'unknown',
    detail: string | null,
    unknownCause: FileItem['unknownCause'] = null,
  ): void {
    item.status = status;
    item.detail = detail;
    item.unknownCause = unknownCause;
    if (status === 'unknown') {
      paused = true;
    } else if (paused && item.resumeEligible) {
      paused = false;
    }
    if (awaitingId === item.id) {
      awaitingId = null;
      receiptTimer?.cancel();
      receiptTimer = null;
    }
    emit();
  }

  function hasInFlight(): boolean {
    return (
      awaitingId !== null ||
      items.some((i) => i.status === 'uploading' || i.status === 'ready')
    );
  }

  /** 串行节奏：上一项 deliver_result（或其 10s 超时）前不取下一项；暂停期仅重试调度可执行。 */
  function pickNext(): FileItem | null {
    if (hasInFlight()) return null;
    for (const item of items) {
      if (item.status !== 'queued') continue;
      if (paused && !item.retryIntent) continue; // 暂停期新入队项只入队、不自动发送
      return item;
    }
    return null;
  }

  function kick(): void {
    if (pumping || disposed) return;
    pumping = true;
    void pumpLoop().finally(() => {
      pumping = false;
    });
  }

  async function pumpLoop(): Promise<void> {
    for (;;) {
      if (disposed) return;
      const item = pickNext();
      if (!item) return;
      await processItem(item);
      if (item.status === 'awaiting') return; // 注册回执等待后暂停推进，回执/超时再 kick
    }
  }

  async function processItem(item: FileItem): Promise<void> {
    // 上传前门禁（D6；锁定/未连接 → 不调用上传接口，归"明确失败"并继续后续项）
    if (!deps.port.fileGateOpen()) {
      settle(item, 'failed', deps.port.fileGateMessage());
      return;
    }
    const connId = deps.port.getConnId();
    if (connId === null) {
      // 能力降级：MUST NOT 发起上传/投递，不以自造值充当 connId
      settle(item, 'failed', UNSUPPORTED_DELIVERY_MSG);
      return;
    }
    item.status = 'uploading';
    item.detail = null;
    item.uploadId = null;
    item.capturedConnId = connId;
    emit();
    let uploadId: string;
    try {
      ({ uploadId } = await deps.upload(deps.taskID, item.file!, connId));
    } catch (err) {
      if (disposed) return;
      if (err instanceof ApiError && err.status === 404) {
        // 404：业务 404 与旧 server 无该路由不可区分 → 固定文案；仅终止当前项，不永久降级
        notice = UPLOAD_TARGET_UNAVAILABLE_MESSAGE;
        settle(item, 'failed', UPLOAD_TARGET_UNAVAILABLE_MESSAGE);
      } else if (isFileReadFailure(err)) {
        // 文件读取失效（size 可达不代表读取路径可用）：原 File 置为不可用，该项重试转入
        // "重新选择文件"路径（绑定该项重试意图的选择器），不再重试读取已失效的 File。
        item.file = null;
        settle(item, 'failed', '文件不可读取，请重新选择');
      } else {
        settle(item, 'failed', err instanceof Error ? err.message : '上传失败');
      }
      return;
    }
    if (disposed) return;
    // 代次绑定（D5）：上传响应返回但 connId 已过期 → 从未发送 deliver = 明确失败，不自动重发
    if (deps.port.getConnId() !== connId) {
      settle(item, 'failed', '连接已更换，请重试');
      return;
    }
    item.uploadId = uploadId;
    item.status = 'ready';
    emit();
    // deliver 前复查门禁（D6）与代次（D5）
    if (!deps.port.fileGateOpen()) {
      settle(item, 'failed', deps.port.fileGateMessage());
      return;
    }
    if (deps.port.getConnId() !== connId) {
      settle(item, 'failed', '连接已更换，请重试');
      return;
    }
    if (!deps.port.sendDeliver(uploadId)) {
      settle(item, 'failed', deps.port.fileGateMessage());
      return;
    }
    item.status = 'awaiting';
    awaitingId = item.id;
    receiptTimer = schedule(() => onReceiptTimeout(item.id), RECEIPT_TIMEOUT_MS);
    emit();
  }

  function onReceiptTimeout(id: string): void {
    const item = items.find((i) => i.id === id);
    if (!item || item.status !== 'awaiting') return;
    settle(item, 'unknown', '回执超时', 'timeout');
    kick();
  }

  function onDeliverResult(r: DeliverResult): void {
    const item = items.find((i) => i.status === 'awaiting' && i.uploadId === r.uploadId);
    if (!item) {
      // 迟到回执（状态已迁移 / uploadId 已换代）：只丢弃 + 日志，不改变项状态与队列状态
      console.warn('[file-delivery] 迟到回执，丢弃', r.uploadId);
      return;
    }
    if (r.ok) {
      settle(item, 'sent', null);
    } else if (r.error === 'write_failed') {
      settle(item, 'unknown', '写入失败，可能部分写入', 'write_failed');
    } else {
      settle(item, 'failed', deliverErrorMessage(r.error));
    }
    kick();
  }

  const unsubConn = deps.port.onConnEvent((ev) => {
    if (disposed) return;
    if (ev.type === 'closed') {
      // 等待回执期间断线 → 结果未知 + 队列暂停；已成功项（sent）MUST NOT 改写
      for (const item of items) {
        if (item.status === 'awaiting') settle(item, 'unknown', '连接已断开', 'disconnected');
      }
      kick();
      return;
    }
    // auth_ok：连接能力刷新（等待回执项已按 closed 归"结果未知"）；有效 connId 恢复后清降级提示
    if (ev.connId !== null) notice = null;
    emit();
  });
  const unsubResult = deps.port.onDeliverResult(onDeliverResult);

  function startRetry(item: FileItem): void {
    item.retryIntent = true;
    item.resumeEligible = item.status === 'unknown';
    item.status = 'queued';
    item.detail = null;
    item.unknownCause = null;
    item.uploadId = null;
    item.capturedConnId = null;
    emit();
    kick();
  }

  function capabilityDegraded(): boolean {
    return deps.port.getConnId() === null;
  }

  return {
    handleCapturedFiles(files: readonly File[]): void {
      if (disposed || files.length === 0) return;
      if (capabilityDegraded()) {
        notice = UNSUPPORTED_DELIVERY_MSG;
        emit();
        return;
      }
      for (const f of files) enqueue(f);
      emit();
      kick();
    },

    handleDroppedFiles(files: readonly File[], directories: readonly string[]): void {
      if (disposed || (files.length === 0 && directories.length === 0)) return;
      if (capabilityDegraded()) {
        notice = UNSUPPORTED_DELIVERY_MSG;
        emit();
        return;
      }
      // 目录项分项拒绝并提示；普通文件继续处理，不遍历目录
      for (const name of directories) enqueueRejected(name, '不支持目录');
      for (const f of files) enqueue(f);
      emit();
      kick();
    },

    handlePickerResult(files: readonly File[] | null): void {
      if (disposed) return;
      const retryId = pendingRetryId;
      pendingRetryId = null;
      const file = files && files.length > 0 ? files[0] : null;
      if (retryId !== null) {
        const item = items.find((i) => i.id === retryId);
        if (item && file && (item.status === 'failed' || item.status === 'unknown')) {
          item.file = file;
          item.name = file.name || item.name;
          startRetry(item); // startRetry 内部已 emit
        } else {
          // 用户取消选择（或项状态已不可重试）：仅解除在途绑定，该项保持原状态
          // （未知项所在共享队列保持暂停）；pickerPendingItem 变化需 emit 刷新 UI
          emit();
        }
        return;
      }
      if (files) this.handleCapturedFiles(files);
    },

    retryItem(id: string): void {
      if (disposed) return;
      // 已有绑定到其他项的在途选择器请求：拒绝启动（一次只有一个绑定重试意图的待回填项，
      // 不被后一次 retryItem 覆盖）；同项重复打开幂等。
      if (pendingRetryId !== null && pendingRetryId !== id) return;
      const item = items.find((i) => i.id === id);
      // 仅失败/未知项可重试；重复点击时该项已在新尝试中（queued/uploading/…）→ no-op，不并发
      if (!item || (item.status !== 'failed' && item.status !== 'unknown')) return;
      if (!fileUsable(item.file)) {
        // 原 File 不可用：打开绑定该项重试意图的文件选择器；取消则该项保持原状态
        pendingRetryId = id;
        deps.openFilePicker();
        emit();
        return;
      }
      startRetry(item);
    },

    snapshot(): FileDeliverySnapshot {
      return { items: items.map(view), paused, notice, pickerPendingItem: pendingRetryId };
    },

    subscribe(cb: () => void): () => void {
      subscribers.add(cb);
      return () => {
        subscribers.delete(cb);
      };
    },

    dispose(): void {
      disposed = true;
      receiptTimer?.cancel();
      receiptTimer = null;
      subscribers.clear();
      unsubConn();
      unsubResult();
    },
  };
}

/* ============================ 事件提取（D7 捕获规则，纯函数） ============================ */

/** DataTransfer 结构子集（真实 DataTransfer / DataTransferItem 结构兼容，便于无 DOM 单测）。 */
export interface DataTransferFileItemLike {
  kind: string;
  getAsFile(): File | null;
  webkitGetAsEntry?(): { isDirectory?: boolean; name?: string } | null;
}

export type DataTransferLike = {
  items?: ArrayLike<DataTransferFileItemLike>;
  files?: ArrayLike<File>;
};

function fileListToArray(files: ArrayLike<File> | undefined): File[] {
  if (!files) return [];
  const out: File[] = [];
  for (let i = 0; i < files.length; i++) out.push(files[i]);
  return out;
}

/**
 * 粘贴提取：按 clipboardData.items 顺序提取有效 File；items 中存在有效 File 时
 * MUST NOT 合并 files 兜底，仅 items 无有效 File 时才读 files（spec「粘贴文件捕获与上传」）。
 */
export function extractPasteFiles(dt: DataTransferLike | null): File[] {
  if (!dt) return [];
  const fromItems: File[] = [];
  const { items } = dt;
  if (items) {
    for (let i = 0; i < items.length; i++) {
      const entry = items[i];
      if (entry.kind !== 'file') continue;
      const file = entry.getAsFile();
      if (file) fromItems.push(file);
    }
  }
  if (fromItems.length > 0) return fromItems;
  return fileListToArray(dt.files);
}

export interface DropExtraction {
  files: File[];
  /** 被分项拒绝的目录名（MUST NOT 遍历目录内容）。 */
  directories: string[];
}

/**
 * drop 提取（仅在 drop 时读取）：items 逐项判定——目录项
 * （DataTransferItem.webkitGetAsEntry()?.isDirectory）分项拒绝，普通文件 getAsFile 继续；
 * items 为空时兜底 files。
 */
export function extractDropFiles(dt: DataTransferLike | null): DropExtraction {
  const files: File[] = [];
  const directories: string[] = [];
  if (!dt) return { files, directories };
  const { items } = dt;
  if (items && items.length > 0) {
    for (let i = 0; i < items.length; i++) {
      const entry = items[i];
      if (entry.kind !== 'file') continue;
      const maybeEntry = entry.webkitGetAsEntry?.();
      if (maybeEntry?.isDirectory) {
        directories.push(maybeEntry.name ?? '(目录)');
        continue;
      }
      const file = entry.getAsFile();
      if (file) files.push(file);
    }
    return { files, directories };
  }
  return { files: fileListToArray(dt.files), directories };
}

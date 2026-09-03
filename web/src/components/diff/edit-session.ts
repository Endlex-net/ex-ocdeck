import { ApiError } from '../../api';
import type {
  FileEditRead,
  FileEditReadEditable,
  FileEditWriteInput,
  FileEditWriteResult,
  LineEnding,
} from '../../types';

/* ============================ 编辑写回协议状态机（diff-review-workbench design D5 前端写协议） ============================
 * 冻结 lineEnding/hasBom/mode；debounce 500ms；每文件单在途写请求 + 冻结 sentContent + 串行合并；
 * 409 阻塞（保留内容+暂停自动写回）；结果未知 → 重读四元确认；显式放弃出口；还原走同一写回端点。
 * 本类不依赖 React/CodeMirror，编辑器侧仅负责把文档文本经 onEdit 喂入。 */

export interface EditSessionIO {
  read: () => Promise<FileEditRead>;
  write: (input: FileEditWriteInput) => Promise<FileEditWriteResult>;
}

export type EditSessionStatus =
  /** 无未保存改动 */ | 'clean'
  /** 有改动等待 debounce/在途合并 */ | 'pending'
  /** 写请求在途 */ | 'saving'
  /** 冲突或未知未确认：暂停自动写回，保留内容 */ | 'blocked';

export interface EditSessionOptions {
  path: string;
  /** 首次编辑读取（GET editable=true 分支）：冻结 lineEnding/hasBom/mode、初始 baseHash 与还原快照。 */
  firstRead: FileEditReadEditable;
  io: EditSessionIO;
  debounceMs?: number;
  /** 409 判定（可注入便于测试）；缺省按 ApiError status=409 或 code=conflict。 */
  isConflict?: (err: unknown) => boolean;
  onChange?: () => void;
}

const defaultIsConflict = (err: unknown): boolean =>
  err instanceof ApiError && (err.status === 409 || err.code === 'conflict');

export class EditSession {
  readonly path: string;
  readonly frozenLineEnding: LineEnding;
  readonly frozenHasBom: boolean;
  readonly frozenMode: string;
  /** 还原快照：进入编辑会话时的内容，会话结束即弃（仅内存态）。 */
  readonly snapshot: string;

  /** 编辑器最新文档文本。 */
  latest: string;
  /** 最近一次已确认基线 hash。 */
  baseHash: string;
  /** 确认基线文本：仅写成功或四元确认成功后推进（与 sentContent 分离，409 重试判定依据）。 */
  confirmedContent: string;
  /** 当前/最近一次在途写请求实际发送的文档文本（单次请求冻结，不代表已确认）。 */
  sentContent: string | null = null;

  status: EditSessionStatus = 'clean';
  blockedReason = '';

  private readonly io: EditSessionIO;
  private readonly debounceMs: number;
  private readonly isConflict: (err: unknown) => boolean;
  private readonly emit: () => void;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private pumpPromise: Promise<void> | null = null;
  /** 还原写事务进行中：onEdit 只记录 latest 不调度（还原纳入同一串行时序，防止并发写）。 */
  private restoreLock = false;
  /** 还原事务期间是否发生过编辑（事件标志，F2：编辑后撤销回原值也算编辑，不得按字符串比较漏判）。 */
  private restoreEdited = false;

  constructor(opts: EditSessionOptions) {
    this.path = opts.path;
    this.io = opts.io;
    this.debounceMs = opts.debounceMs ?? 500;
    this.isConflict = opts.isConflict ?? defaultIsConflict;
    this.emit = opts.onChange ?? (() => {});
    this.frozenLineEnding = opts.firstRead.lineEnding;
    this.frozenHasBom = opts.firstRead.hasBom;
    this.frozenMode = opts.firstRead.mode;
    this.snapshot = opts.firstRead.content;
    this.latest = opts.firstRead.content;
    this.baseHash = opts.firstRead.baseHash;
    this.confirmedContent = opts.firstRead.content;
    this.sentContent = opts.firstRead.content;
  }

  /** 编辑器文档变更入口。blocked 态与还原写事务期间暂停自动写回（内容仍记录，待后续补发/用户决策）。 */
  onEdit(text: string): void {
    this.latest = text;
    if (this.restoreLock) {
      this.restoreEdited = true;
      this.emit();
      return;
    }
    if (this.status === 'blocked') {
      this.emit();
      return;
    }
    this.status = 'pending';
    this.emit();
    this.schedule();
  }

  /** 切换文件/退出编辑/还原前：flush 并等待在途写完成。 */
  async flush(): Promise<void> {
    this.clearTimer();
    for (;;) {
      // eslint-disable-next-line no-await-in-loop
      await this.pump();
      if (this.status === 'blocked' || this.latest === this.confirmedContent) return;
    }
  }

  /** 冲突提示的「重试」：解除阻塞并强制重发未确认的最新文档（携带当前 baseHash）。 */
  async retry(): Promise<void> {
    if (this.status !== 'blocked') return;
    this.status = this.latest === this.confirmedContent ? 'clean' : 'pending';
    this.blockedReason = '';
    this.emit();
    await this.flush();
  }

  /** 显式放弃本地改动：重读文件，编辑器重置为服务端内容；返回新内容（失败返回 null）。 */
  async discard(): Promise<string | null> {
    this.clearTimer();
    if (this.pumpPromise) await this.pumpPromise; // join 在途
    let r: FileEditRead;
    try {
      r = await this.io.read();
    } catch {
      return null;
    }
    if (!r.editable) return null;
    this.latest = r.content;
    this.sentContent = r.content;
    this.confirmedContent = r.content;
    this.baseHash = r.baseHash;
    this.status = 'clean';
    this.blockedReason = '';
    this.restoreEdited = false;
    this.emit();
    return r.content;
  }

  /**
   * 还原：用会话快照走同一写回端点（继承 baseHash 冲突保护）。
   * 返回编辑器应显示的内容；null = 失败/被阻塞（保持编辑器现状与冲突提示）。
   * F2：restoreLock 覆盖整个「初始 flush + 还原写」事务——期间任何编辑（含编辑后
   * 撤销回原值）只置 restoreEdited 标志不调度；快照写成功后若标志置位，MUST NOT
   * 用快照覆盖 latest，未确认部分立即以新基线补发。
   */
  async restore(): Promise<string | null> {
    this.restoreLock = true;
    this.restoreEdited = false;
    try {
      await this.flush();
      if (this.status === 'blocked') return null;
      if (
        !this.restoreEdited &&
        this.latest === this.snapshot &&
        this.confirmedContent === this.snapshot
      ) {
        return this.snapshot;
      }
      const ok = await this.performWrite(this.snapshot);
      if (!ok) return null;
      if (this.restoreEdited) {
        this.restoreEdited = false;
        if (this.latest !== this.confirmedContent) {
          this.status = 'pending';
          this.emit();
          await this.flush();
          // flush 可能在补发中遇冲突转 blocked（TS 控制流不跨 await 追踪字段突变，显式读取）
          const after = this.status as EditSessionStatus;
          if (after === 'blocked') return null;
        } else {
          this.status = 'clean';
          this.emit();
        }
        return this.latest;
      }
      this.latest = this.snapshot;
      this.status = 'clean';
      this.emit();
      return this.snapshot;
    } finally {
      this.restoreLock = false; // 所有 blocked/失败/异常出口统一释放
    }
  }

  /** 离开守卫：flush 后仍有未解决阻塞 → 不允许切换文件/退出编辑。 */
  async canLeave(): Promise<boolean> {
    await this.flush();
    return this.status !== 'blocked';
  }

  dispose(): void {
    this.clearTimer();
  }

  // ---------- 内部 ----------

  private schedule(): void {
    this.clearTimer();
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.flush();
    }, this.debounceMs);
  }

  private clearTimer(): void {
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
  }

  /** 单在途泵：同一时刻至多一个写请求；在途期间的编辑合并为 latest，响应后补发。 */
  private pump(): Promise<void> {
    if (!this.pumpPromise) {
      this.pumpPromise = this.run().finally(() => {
        this.pumpPromise = null;
      });
    }
    return this.pumpPromise;
  }

  private async run(): Promise<void> {
    while (this.status !== 'blocked' && this.latest !== this.confirmedContent) {
      // eslint-disable-next-line no-await-in-loop
      const ok = await this.performWrite(this.latest);
      if (!ok) return;
    }
    if (this.status !== 'blocked') {
      this.status = this.latest === this.confirmedContent ? 'clean' : 'pending';
      this.emit();
    }
  }

  /**
   * 执行一次写回（含恢复确认）。返回 true = 已确认（含未知→重读四元确认通过）。
   * false = 已转 blocked（409 或恢复确认未通过）。
   * 确认基线（baseHash/confirmedContent）仅在成功路径推进。
   */
  private async performWrite(content: string): Promise<boolean> {
    this.sentContent = content; // 冻结本次发送文本（仅单次请求语义，非确认基线）
    this.status = 'saving';
    this.emit();
    try {
      const res = await this.io.write({
        path: this.path,
        content,
        baseHash: this.baseHash,
        lineEnding: this.frozenLineEnding, // 冻结携带
        baseMode: this.frozenMode,
      });
      this.baseHash = res.baseHash;
      this.confirmedContent = content;
      return true;
    } catch (err) {
      if (this.isConflict(err)) {
        this.block('保存冲突：文件已在别处被修改。你的编辑内容已保留，自动写回已暂停。');
        return false;
      }
      // 网络/internal 等结果未知 → 恢复确认流程
      const recovered = await this.recover(content);
      if (recovered) {
        this.confirmedContent = content;
        return true;
      }
      this.block('保存结果未知，且重读确认未通过。你的编辑内容已保留，自动写回已暂停。');
      return false;
    }
  }

  /** 未知结果恢复：重读并逐项比对四元——content==sentContent、hasBom==冻结、
   *  （sentContent 含 \n 时）lineEnding==冻结、mode==首次读取值；全等才采用新基线。 */
  private async recover(sent: string): Promise<boolean> {
    let r: FileEditRead;
    try {
      r = await this.io.read();
    } catch {
      return false; // 重读失败 = 未确认
    }
    if (!r.editable) return false;
    const lineEndingOk = sent.includes('\n') ? r.lineEnding === this.frozenLineEnding : true;
    if (
      r.content === sent &&
      r.hasBom === this.frozenHasBom &&
      lineEndingOk &&
      r.mode === this.frozenMode
    ) {
      this.baseHash = r.baseHash; // rename 可能已成功而响应丢失：采用新基线
      return true;
    }
    return false;
  }

  private block(reason: string): void {
    this.status = 'blocked';
    this.blockedReason = reason;
    this.clearTimer();
    this.emit();
  }
}

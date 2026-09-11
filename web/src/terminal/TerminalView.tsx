import { useCallback, useEffect, useRef, useState } from 'react';
import { TermSession, type TermConnState } from './session';
import {
  createFileDeliveryController,
  extractDropFiles,
  extractPasteFiles,
  type FileDeliveryController,
  type FileDeliverySnapshot,
} from './file-delivery';
import { api } from '../api';
import {
  clearTerminalFocus,
  isFocusRequestTargetAllowed,
  isTerminalFocusExpired,
  pendingTerminalFocus,
  subscribeTerminalFocus,
} from './focus-request';
import { DEFAULT_CAPS, resolveMobileCaps } from './mobile-mode';
import {
  loadMobileCaps,
  loadMobileMode,
  loadTermPrefs,
  TERM_PREFS_CHANGED,
} from './preferences';
import {
  createClipboardController,
  loadClipboardPolicy,
  saveClipboardPolicy,
} from './clipboard';
import { writeTextToClipboard } from '../clipboard';
import { debugMark } from '../debug';
import { useMediaQuery } from '../hooks';
import '@xterm/xterm/css/xterm.css';
import './mobile.css';
import './file-delivery.css'; // 文件投递 UI 视觉层（terminal-file-paste-drop 5.3 精修）
import './fonts.css'; // Nerd Font 图标字形 @font-face（terminal-links-emoji-icons design D3）

interface TerminalViewProps {
  /** WS 路径，如 /ws/terminal/<taskID> 或 /ws/terminal/shell/<tid>。 */
  wsPath: string;
  /** 是否建立连接（标签可见 && 任务允许连接）。 */
  active: boolean;
  onState?: (s: TermConnState) => void;
}

const STATE_LABEL: Record<TermConnState, string> = {
  idle: '未连接',
  connecting: '连接中…',
  connected: '',
  reconnecting: '连接断开，正在重连…',
  recovering: '进程启动中',
  suspended: '任务已挂起',
  closed: '会话已断开',
  replaced: '此终端已在其他标签页打开',
  gone: '终端已关闭，可在上方新建终端',
  auth_failed: '认证失败',
};

const TOAST_MS = 2000;

/** 成功项瞬时反馈驻留时长：「已发送到终端」在浮层停留该时长后仅从呈现层隐去
 * （状态机不删项），避免浮层持续遮挡终端输入区；失败/结果未知项保留待重试。 */
export const FILE_SENT_FEEDBACK_MS = 1200;

/** wsPath 形态区分（design D3）：/ws/terminal/<taskID> 为 TUI（返回 taskID，可消费焦点请求）；
 * /ws/terminal/shell/... 为 shell 实例（返回 null，MUST NOT 消费）。 */
function tuiTaskIDFromWsPath(wsPath: string): string | null {
  const prefix = '/ws/terminal/';
  if (!wsPath.startsWith(prefix)) return null;
  const rest = wsPath.slice(prefix.length);
  return rest === '' || rest.startsWith('shell/') ? null : rest;
}

/** 用户手势内复制走共享 util（web/src/clipboard.ts，workbench-base-ref-and-overflow D5）：
 *  有 Clipboard API 走 writeText，否则 execCommand；失败则保留可选中文本。 */

export function TerminalView({ wsPath, active, onState }: TerminalViewProps) {
  const hostRef = useRef<HTMLDivElement>(null);
  const wrapRef = useRef<HTMLDivElement>(null);
  const sessionRef = useRef<TermSession | null>(null);
  const [state, setState] = useState<TermConnState>('idle');
  const [locked, setLocked] = useState(false);
  const [fallbackText, setFallbackText] = useState<string | null>(null);
  const [toastVisible, setToastVisible] = useState(false);
  const onStateRef = useRef(onState);
  onStateRef.current = onState;
  // clipboard.ts：串行队列（latest-wins + 限速）承载 auto 写入；手动复制走用户手势直写。
  const clipCtl = useRef(createClipboardController({ write: writeTextToClipboard })).current;
  const clipSeq = useRef(0);
  const toastTimer = useRef<ReturnType<typeof setTimeout>>();
  const onClipboardWriteRef = useRef<(text: string) => void>(() => {});

  // 文件投递（terminal-file-paste-drop 5.3）：仅 TUI 实例挂载；shell 终端不挂任何入口。
  const fdRef = useRef<FileDeliveryController | null>(null);
  const [fdSnapshot, setFdSnapshot] = useState<FileDeliverySnapshot | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const dragDepth = useRef(0);
  const [dropHover, setDropHover] = useState(false);
  // 成功项驻留隐去（呈现层语义，状态机契约不变）：已进入「已发送到终端」且
  // 驻留期满的项 id 集合；定时器按项 id 管理，项消失/实例切换时清理。
  const [dismissedSentIds, setDismissedSentIds] = useState<readonly string[]>([]);
  const sentDismissTimers = useRef(new Map<string, ReturnType<typeof setTimeout>>());

  // 导航焦点请求（design D3，唯一消费方 = 目标任务 TUI TerminalView）：
  // shell 实例不订阅不消费；本地不持有请求状态，等待/取消期间的请求有效性
  // 一律以单例 pendingTerminalFocus() 快照校验（被取消/覆盖后自然失效）。
  const tuiTaskID = tuiTaskIDFromWsPath(wsPath);
  const connStateRef = useRef<TermConnState>('idle');
  const lockedRef = useRef(false);

  /**
   * 焦点请求消费（design D3 状态表 + 门禁）：未 connected（idle/connecting/
   * reconnecting/recovering）挂起等待；进入 connected 后逐条门禁——①过期
   * ②taskID 匹配（不匹配暂不消费、请求保留给其目标）③锁定 ④⑤焦点保护
   * （用户已在他处交互，宁可不聚焦不抢焦点）任一不满足即作废，全部通过才
   * 消费并聚焦（seq 最新由单例「新覆盖旧」保证）。
   */
  const tryConsumeFocusRequest = useCallback(() => {
    const session = sessionRef.current;
    if (!session || connStateRef.current !== 'connected') return;
    const req = pendingTerminalFocus();
    if (!req || req.taskID !== tuiTaskID) return;
    if (
      lockedRef.current ||
      isTerminalFocusExpired(req) ||
      !isFocusRequestTargetAllowed(document.activeElement)
    ) {
      clearTerminalFocus(req.seq);
      return;
    }
    clearTerminalFocus(req.seq);
    session.focus();
  }, [tuiTaskID]);

  const showCopiedToast = () => {
    if (!clipCtl.takeToastSlot()) return;
    setToastVisible(true);
    if (toastTimer.current !== undefined) clearTimeout(toastTimer.current);
    toastTimer.current = setTimeout(() => setToastVisible(false), TOAST_MS);
  };

  onClipboardWriteRef.current = (text: string) => {
    const action = clipCtl.onValidatedWrite(text);
    if (action === 'drop') return;
    const seq = ++clipSeq.current;
    if (action === 'ask') {
      setFallbackText(text);
      return;
    }
    void clipCtl.requestWrite(text).then((outcome) => {
      if (seq !== clipSeq.current) return;
      if (outcome === 'written') {
        setFallbackText(null);
        showCopiedToast();
      } else if (outcome === 'failed') {
        setFallbackText(text);
      }
    });
  };

  // 按钮可见性 = 生效能力 caps.lock（mobile-terminal-mode-settings D3）：由
  // mode +（仅 on 时）caps + coarse pointer 经 resolveMobileCaps 统一判定，
  // 随模式/指针/偏好变更即时刷新，与 session 锁状态正交。
  const coarsePointer = useMediaQuery('(pointer: coarse)');
  const [lockCap, setLockCap] = useState<boolean>(() => {
    const mode = loadMobileMode();
    return resolveMobileCaps(mode, mode === 'on' ? loadMobileCaps() : DEFAULT_CAPS, coarsePointer).lock;
  });

  useEffect(() => {
    const refresh = () => {
      const mode = loadMobileMode();
      setLockCap(resolveMobileCaps(mode, mode === 'on' ? loadMobileCaps() : DEFAULT_CAPS, coarsePointer).lock);
    };
    refresh(); // coarsePointer 变化时以新 pointer 状态重算
    window.addEventListener(TERM_PREFS_CHANGED, refresh);
    window.addEventListener('storage', refresh);
    return () => {
      window.removeEventListener(TERM_PREFS_CHANGED, refresh);
      window.removeEventListener('storage', refresh);
    };
  }, [coarsePointer]);

  useEffect(() => {
    const host = hostRef.current;
    const wrap = wrapRef.current;
    if (!host || !wrap) return;
    const session = new TermSession(
      host,
      wrap,
      wsPath,
      (s) => {
        connStateRef.current = s;
        setState(s);
        onStateRef.current?.(s);
        // 状态表：进入 connected 即对挂起中的焦点请求做门禁检查并消费（design D3）
        if (s === 'connected') tryConsumeFocusRequest();
      },
      (text) => onClipboardWriteRef.current(text),
    );
    sessionRef.current = session;
    setLocked(session.isLocked());
    lockedRef.current = session.isLocked();
    const unsubLock = session.onLockChange((v) => {
      lockedRef.current = v;
      setLocked(v);
    });

    // 文件投递（terminal-file-paste-drop 5.3）：仅 TUI 实例挂载 controller 与全部事件
    // listener（paste capture/dragover/dragenter/dragleave/drop），shell 终端零注册零清理；
    // 随终端实例生命周期挂载/清理。
    let fd: FileDeliveryController | null = null;
    let unsubFd: (() => void) | null = null;
    let detachFileListeners: (() => void) | null = null;
    if (tuiTaskID) {
      fd = createFileDeliveryController({
        taskID: tuiTaskID,
        port: session,
        upload: (taskID, file, connId) => api.uploadAttachment(taskID, file, connId),
        openFilePicker: () => fileInputRef.current?.click(),
      });
      fdRef.current = fd;
      unsubFd = fd.subscribe(() => setFdSnapshot(fd!.snapshot()));
      setFdSnapshot(fd.snapshot());

      // paste：终端容器 capture 阶段 listener（先于 xterm textarea）；纯文本不拦截，
      // 确认含文件后同步 preventDefault + stopPropagation（D7）。
      const onPaste = (e: Event) => {
        const files = extractPasteFiles((e as ClipboardEvent).clipboardData);
        if (files.length === 0) return;
        e.preventDefault();
        e.stopPropagation();
        fd!.handleCapturedFiles(files);
      };
      // drop/dragover：阻止浏览器导航；dropEffect='copy'；dragenter/leave 计数防闪烁（D7）。
      const onDragOver = (e: Event) => {
        e.preventDefault();
        const dt = (e as DragEvent).dataTransfer;
        if (dt) dt.dropEffect = 'copy';
      };
      const onDragEnter = (e: Event) => {
        e.preventDefault();
        dragDepth.current += 1;
        setDropHover(true);
      };
      const onDragLeave = () => {
        dragDepth.current = Math.max(0, dragDepth.current - 1);
        if (dragDepth.current === 0) setDropHover(false);
      };
      const onDrop = (e: Event) => {
        e.preventDefault();
        dragDepth.current = 0;
        setDropHover(false);
        const { files, directories } = extractDropFiles((e as DragEvent).dataTransfer);
        if (files.length === 0 && directories.length === 0) return;
        fd!.handleDroppedFiles(files, directories);
      };
      host.addEventListener('paste', onPaste, true);
      host.addEventListener('dragover', onDragOver);
      host.addEventListener('dragenter', onDragEnter);
      host.addEventListener('dragleave', onDragLeave);
      host.addEventListener('drop', onDrop);
      detachFileListeners = () => {
        host.removeEventListener('paste', onPaste, true);
        host.removeEventListener('dragover', onDragOver);
        host.removeEventListener('dragenter', onDragEnter);
        host.removeEventListener('dragleave', onDragLeave);
        host.removeEventListener('drop', onDrop);
      };
    }

    return () => {
      sessionRef.current = null;
      unsubLock();
      detachFileListeners?.();
      if (unsubFd) unsubFd();
      fd?.dispose();
      fdRef.current = null;
      session.dispose();
    };
  }, [wsPath, tryConsumeFocusRequest]);

  // 文件选择器「取消」事件（input 的 cancel，React types 未覆盖）：解除绑定重试意图，
  // 该项保持原状态（未知项所在共享队列保持暂停）。
  useEffect(() => {
    const input = fileInputRef.current;
    if (!tuiTaskID || !input) return;
    const onCancel = () => fdRef.current?.handlePickerResult(null);
    input.addEventListener('cancel', onCancel);
    return () => input.removeEventListener('cancel', onCancel);
  }, [tuiTaskID]);

  // 成功项驻留调度：项进入 sent 即起 FILE_SENT_FEEDBACK_MS 定时器，到期把 id 记入
  // dismissedSentIds（仅呈现层过滤，不触碰 controller 内部状态）；项被移除时清理其定时器。
  useEffect(() => {
    const timers = sentDismissTimers.current;
    if (!fdSnapshot) return;
    const liveIds = new Set(fdSnapshot.items.map((it) => it.id));
    for (const [id, timer] of timers) {
      if (!liveIds.has(id)) {
        clearTimeout(timer);
        timers.delete(id);
      }
    }
    for (const it of fdSnapshot.items) {
      if (it.status !== 'sent' || timers.has(it.id) || dismissedSentIds.includes(it.id)) continue;
      timers.set(
        it.id,
        setTimeout(() => {
          sentDismissTimers.current.delete(it.id);
          setDismissedSentIds((prev) => (prev.includes(it.id) ? prev : [...prev, it.id]));
        }, FILE_SENT_FEEDBACK_MS),
      );
    }
  }, [fdSnapshot, dismissedSentIds]);

  // 终端实例切换（wsPath 变化 → controller 重建、项 id 重新计数）：清空驻留标记与在途
  // 定时器，避免旧 id 命中新会话的成功项；卸载时一并清理全部定时器。
  useEffect(() => {
    for (const timer of sentDismissTimers.current.values()) clearTimeout(timer);
    sentDismissTimers.current.clear();
    setDismissedSentIds([]);
    return () => {
      for (const timer of sentDismissTimers.current.values()) clearTimeout(timer);
      sentDismissTimers.current.clear();
    };
  }, [wsPath]);

  // 焦点请求订阅（design D3）：挂载即可能同步交付 pending 快照（早于挂载的发布）；
  // 回调仅在已 connected 时立即按门禁消费，否则等待 onState('connected')。
  // 等待期的取消不在这里：输入区取消由请求层全局 focusin 守卫负责、路由离开取消由
  // TaskWorkbenchPage 卸载 cancelTerminalFocusForTask 负责；退订不删更晚的新请求。
  useEffect(() => {
    if (!tuiTaskID) return;
    return subscribeTerminalFocus(() => tryConsumeFocusRequest());
  }, [tuiTaskID, tryConsumeFocusRequest]);

  useEffect(() => {
    const session = sessionRef.current;
    if (!session) return;
    if (active) {
      debugMark('odterm:connect-call');
      session.connect();
    } else {
      session.disconnect();
    }
  }, [active, wsPath]);

  // 偏好变更即时生效：同页 CustomEvent + 跨标签页 storage 事件。
  // 远程剪贴板策略切到 off：作废排队/在途写入的等待方 + 递增代数（in-flight 写入的
  // 迟到结果不得重开 popover 或改 UI）+ 关闭在途确认 popover。
  useEffect(() => {
    const session = sessionRef.current;
    if (!session) return;
    const apply = () => {
      session.applyPreferences(loadTermPrefs());
      if (loadClipboardPolicy() === 'off') {
        clipCtl.cancelPending();
        clipSeq.current += 1;
        setFallbackText(null);
      }
    };
    window.addEventListener(TERM_PREFS_CHANGED, apply);
    window.addEventListener('storage', apply);
    return () => {
      window.removeEventListener(TERM_PREFS_CHANGED, apply);
      window.removeEventListener('storage', apply);
    };
  }, [wsPath]);

  useEffect(() => {
    return () => {
      if (toastTimer.current !== undefined) clearTimeout(toastTimer.current);
      clipCtl.dispose();
    };
  }, []);

  const popoverOpen = fallbackText !== null;

  // Escape 关闭弹层（LifecycleLogModal 同款模式）；capture + stopPropagation
  // 避免这一下 Escape 透传进远程会话产生副作用。
  useEffect(() => {
    if (!popoverOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      e.stopPropagation();
      setFallbackText(null);
    };
    window.addEventListener('keydown', onKey, true);
    return () => window.removeEventListener('keydown', onKey, true);
  }, [popoverOpen]);

  const copyFromPopover = () => {
    if (fallbackText === null) return;
    const text = fallbackText;
    const seq = clipSeq.current;
    void writeTextToClipboard(text).then(
      () => {
        if (seq !== clipSeq.current) return;
        setFallbackText(null);
        showCopiedToast();
      },
      () => {
        /* 手势内仍失败：保留可选中文本 */
      },
    );
  };

  const alwaysAllow = () => {
    try {
      saveClipboardPolicy('auto');
    } catch {
      /* 持久化失败仍继续本次复制 */
    }
    copyFromPopover();
  };

  const showOverlay = state !== 'connected' && state !== 'idle';
  const wrapClass = dropHover ? 'terminal-wrap terminal-drop-hover' : 'terminal-wrap';
  // 呈现层可见项：驻留期满的成功项被过滤（浮层无待关注内容时整体消失，不遮挡输入区）；
  // 失败/结果未知项始终保留（重试入口）。
  const visibleFdItems = fdSnapshot
    ? fdSnapshot.items.filter(
        (it) => !(it.status === 'sent' && dismissedSentIds.includes(it.id)),
      )
    : [];

  return (
    <div className={wrapClass} ref={wrapRef}>
      <div className="terminal-host" ref={hostRef} />
      {lockCap && (
        <button
          type="button"
          className="terminal-lock-button"
          onClick={() => {
            const session = sessionRef.current;
            if (!session) return;
            if (session.isLocked()) session.unlock();
            else session.lock();
          }}
        >
          {locked ? '🔒 锁定中 · 点此解锁' : '🔓 已解锁 · 点此锁定'}
        </button>
      )}
      {showOverlay && (
        <div className="terminal-overlay">
          <div className="terminal-overlay-box">
            {(state === 'connecting' || state === 'reconnecting' || state === 'recovering') && (
              <span className="spinner" aria-hidden />
            )}
            <span>{STATE_LABEL[state]}</span>
            {(state === 'closed' || state === 'suspended') && (
              <button
                className="btn btn-small"
                onClick={() => sessionRef.current?.reconnect()}
              >
                重新连接
              </button>
            )}
            {state === 'replaced' && (
              <button
                className="btn btn-small"
                onClick={() => sessionRef.current?.reconnect()}
              >
                接管
              </button>
            )}
          </div>
        </div>
      )}
      {popoverOpen && (
        <div className="terminal-clipboard-popover" role="dialog" aria-label="写入剪贴板请求">
          <div className="terminal-clipboard-title">写入剪贴板请求</div>
          <div className="terminal-clipboard-desc">
            终端中的远程程序想把以下内容复制到你的本机剪贴板。
          </div>
          <pre className="terminal-clipboard-text">{fallbackText}</pre>
          <div className="terminal-clipboard-actions">
            <button
              type="button"
              className="btn btn-small btn-primary"
              onClick={copyFromPopover}
            >
              允许复制
            </button>
            <button type="button" className="btn btn-small" onClick={alwaysAllow}>
              始终允许
            </button>
            <button
              type="button"
              className="btn btn-small btn-ghost"
              onClick={() => setFallbackText(null)}
            >
              关闭
            </button>
          </div>
        </div>
      )}
      {toastVisible && (
        <div className="od-toast" role="status">
          已复制
        </div>
      )}
      {tuiTaskID && (
        <div className="terminal-file-bar">
          {/* 无常驻入口按钮（粘贴/拖拽为主路径，不遮挡输入区）；隐藏 file input 必须保留：
              失败/未知项的重试重选路径（绑定重试意图的选择器）仍经 openFilePicker 触发它。 */}
          {fdSnapshot && (visibleFdItems.length > 0 || fdSnapshot.notice) && (
            <div className="terminal-file-panel" aria-live="polite">
              {fdSnapshot.notice && <div className="terminal-file-notice">{fdSnapshot.notice}</div>}
              {visibleFdItems.length > 0 && (
                <>
                  <div className="terminal-file-panel-title">
                    文件投递
                    {fdSnapshot.paused && (
                      <span className="terminal-file-paused">（队列已暂停）</span>
                    )}
                  </div>
                  <ul className="terminal-file-list">
                    {visibleFdItems.map((it) => (
                      <li key={it.id} className={`terminal-file-item terminal-file-item-${it.status}`}>
                        <div className="terminal-file-row">
                          <span className="terminal-file-name" title={it.name}>
                            {it.name}
                          </span>
                          <span className={`terminal-file-status terminal-file-status-${it.status}`}>
                            <span className="terminal-file-dot" aria-hidden />
                            {it.label}
                          </span>
                          {it.canRetry && (
                            <button
                              type="button"
                              className="terminal-file-retry"
                              disabled={
                                fdSnapshot.pickerPendingItem !== null &&
                                fdSnapshot.pickerPendingItem !== it.id
                              }
                              onClick={() => fdRef.current?.retryItem(it.id)}
                            >
                              重试
                            </button>
                          )}
                        </div>
                        {(it.detail || it.hint) && (
                          <div className="terminal-file-sub">
                            {it.detail && <span className="terminal-file-detail">{it.detail}</span>}
                            {it.hint && <span className="terminal-file-hint">{it.hint}</span>}
                          </div>
                        )}
                      </li>
                    ))}
                  </ul>
                </>
              )}
            </div>
          )}
          <input
            ref={fileInputRef}
            type="file"
            multiple
            hidden
            onChange={(e) => {
              const input = e.currentTarget;
              const files = Array.from(input.files ?? []);
              input.value = '';
              fdRef.current?.handlePickerResult(files.length > 0 ? files : null);
            }}
          />
        </div>
      )}
    </div>
  );
}

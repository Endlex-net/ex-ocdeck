import { useCallback, useEffect, useRef, useState } from 'react';
import { TermSession, type TermConnState } from './session';
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
    return () => {
      sessionRef.current = null;
      unsubLock();
      session.dispose();
    };
  }, [wsPath, tryConsumeFocusRequest]);

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

  return (
    <div className="terminal-wrap" ref={wrapRef}>
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
    </div>
  );
}

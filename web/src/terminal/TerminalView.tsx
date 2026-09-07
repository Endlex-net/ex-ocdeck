import { useEffect, useRef, useState } from 'react';
import { TermSession, type TermConnState } from './session';
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
import { debugMark } from '../debug';
import { useMediaQuery } from '../hooks';
import '@xterm/xterm/css/xterm.css';
import './mobile.css';

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

/** 用户手势内复制：有 Clipboard API 走 writeText，否则 execCommand；失败则保留可选中文本。 */
function writeTextToClipboard(text: string): Promise<void> {
  if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
    return navigator.clipboard.writeText(text);
  }
  return copyViaExecCommand(text);
}

function copyViaExecCommand(text: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.left = '-9999px';
    document.body.appendChild(ta);
    ta.select();
    try {
      if (document.execCommand('copy')) resolve();
      else reject(new Error('copy failed'));
    } catch (err) {
      reject(err);
    } finally {
      ta.remove();
    }
  });
}

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
        setState(s);
        onStateRef.current?.(s);
      },
      (text) => onClipboardWriteRef.current(text),
    );
    sessionRef.current = session;
    setLocked(session.isLocked());
    const unsubLock = session.onLockChange(setLocked);
    return () => {
      sessionRef.current = null;
      unsubLock();
      session.dispose();
    };
  }, [wsPath]);

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

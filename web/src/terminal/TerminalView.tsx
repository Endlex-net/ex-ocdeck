import { useEffect, useRef, useState } from 'react';
import { TermSession, type TermConnState } from './session';
import { loadTermPrefs, TERM_PREFS_CHANGED } from './preferences';
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
  suspended: '任务已挂起',
  closed: '会话已断开',
  replaced: '此终端已在其他标签页打开',
  gone: '终端已关闭，可在上方新建终端',
  auth_failed: '认证失败',
};

export function TerminalView({ wsPath, active, onState }: TerminalViewProps) {
  const hostRef = useRef<HTMLDivElement>(null);
  const wrapRef = useRef<HTMLDivElement>(null);
  const sessionRef = useRef<TermSession | null>(null);
  const [state, setState] = useState<TermConnState>('idle');
  const [locked, setLocked] = useState(false);
  const onStateRef = useRef(onState);
  onStateRef.current = onState;

  // 按钮可见性 = 视图侧 coarse pointer（与 session 锁状态正交）。
  const coarsePointer = useMediaQuery('(pointer: coarse)');

  useEffect(() => {
    const host = hostRef.current;
    const wrap = wrapRef.current;
    if (!host || !wrap) return;
    const session = new TermSession(host, wrap, wsPath, (s) => {
      setState(s);
      onStateRef.current?.(s);
    });
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
      session.connect();
    } else {
      session.disconnect();
    }
  }, [active, wsPath]);

  // 偏好变更即时生效：同页 CustomEvent + 跨标签页 storage 事件。
  useEffect(() => {
    const session = sessionRef.current;
    if (!session) return;
    const apply = () => session.applyPreferences(loadTermPrefs());
    window.addEventListener(TERM_PREFS_CHANGED, apply);
    window.addEventListener('storage', apply);
    return () => {
      window.removeEventListener(TERM_PREFS_CHANGED, apply);
      window.removeEventListener('storage', apply);
    };
  }, [wsPath]);

  const showOverlay = state !== 'connected' && state !== 'idle';

  return (
    <div className="terminal-wrap" ref={wrapRef}>
      <div className="terminal-host" ref={hostRef} />
      {coarsePointer && (
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
            {(state === 'connecting' || state === 'reconnecting') && (
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
    </div>
  );
}

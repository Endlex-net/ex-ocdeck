import { useEffect, useRef, useState } from 'react';
import { TermSession, type TermConnState } from './session';
import '@xterm/xterm/css/xterm.css';

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
  const sessionRef = useRef<TermSession | null>(null);
  const [state, setState] = useState<TermConnState>('idle');
  const onStateRef = useRef(onState);
  onStateRef.current = onState;

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    const session = new TermSession(host, wsPath, (s) => {
      setState(s);
      onStateRef.current?.(s);
    });
    sessionRef.current = session;
    return () => {
      sessionRef.current = null;
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

  const showOverlay = state !== 'connected' && state !== 'idle';

  return (
    <div className="terminal-wrap">
      <div className="terminal-host" ref={hostRef} />
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

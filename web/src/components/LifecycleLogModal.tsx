import { useEffect, useRef, useState } from 'react';
import { ApiError } from '../api';

interface LifecycleLogModalProps {
  title: string;
  /** 拉取 text/plain 日志（api.getInitLog / api.getPreDeleteLog）。 */
  fetchLog: () => Promise<string>;
  onClose: () => void;
}

/**
 * 生命周期日志查看弹窗（tasks.md 5.3）：
 * 日志仅为辅助信息（脚本启动前失败时可能为旧内容）；无日志显示空态。
 */
export function LifecycleLogModal({ title, fetchLog, onClose }: LifecycleLogModalProps) {
  const [log, setLog] = useState<string | null>(null);
  const [error, setError] = useState('');
  const closeRef = useRef<HTMLButtonElement>(null);

  // Escape 关闭 + 打开时聚焦关闭按钮（简单焦点管理，不做完整 focus trap）
  useEffect(() => {
    closeRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const text = await fetchLog();
        if (!cancelled) setLog(text);
      } catch (err) {
        if (!cancelled)
          setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '加载日志失败');
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div
        className="modal modal-wide"
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="modal-title">{title}</div>
        <div className="modal-body">
          {error && <div className="error-line">{error}</div>}
          {!error && log === null && (
            <div className="env-empty">
              <span className="spinner spinner-inline" aria-hidden /> 加载中…
            </div>
          )}
          {!error && log !== null && log === '' && <div className="env-empty">暂无日志。</div>}
          {!error && log && <pre className="lc-log">{log}</pre>}
        </div>
        <div className="modal-actions">
          <button className="btn" onClick={onClose} ref={closeRef}>
            关闭
          </button>
        </div>
      </div>
    </div>
  );
}

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import type {
  CSSProperties,
  KeyboardEvent as ReactKeyboardEvent,
  PointerEvent as ReactPointerEvent,
} from 'react';

/* ============================ 共享零依赖 resize 原语（design D4） ============================
 * 侧栏 / git 文件面板 / diff 分栏共用的分割条与持久化尺寸 hook。
 * 拖拽状态机：idle → dragging → committed | cancelled → idle；
 * pointerup 先清除活动事务（committed）再保存一次、最后 release capture——
 * 此后到达的 lostpointercapture 无事务可回滚，闭合 pointerup/lostpointercapture 竞态。 */

export type ResizeMode = 'px' | 'ratio';

const HANDLE_WIDTH_PX = 6;
const KEYBOARD_STEP_PX = 10;

/** 存储读取解码：值约定为 JSON number 文本。解析失败、非有限 number、null、
 * 字符串型数字 → null；整数域下非整数 → null（px 不取整回退）；合法越界 → clamp。 */
function parsePersistedSize(raw: string, min: number, max: number, integerOnly: boolean): number | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (typeof parsed !== 'number' || !Number.isFinite(parsed)) return null;
  if (integerOnly && !Number.isInteger(parsed)) return null;
  return Math.min(max, Math.max(min, parsed));
}

export interface PersistedSize {
  value: number;
  /** 仅更新保留内存：拖拽实时调整、取消恢复起始值。 */
  setSize: (next: number) => void;
  /** 更新保留内存并写一次存储；禁用期间不访问存储；写失败（QuotaExceeded 等）捕获。 */
  commitSize: (next: number) => void;
}

export function usePersistedSize(
  key: string,
  defaultValue: number,
  min: number,
  max: number,
  enabled: boolean,
): PersistedSize {
  // 契约签名无模式参数：default/min/max 均为整数视为整数域（px 宽度），
  // 存储读到非整数回退默认；比例域（非整数边界）允许浮点。
  const integerOnly = Number.isInteger(defaultValue) && Number.isInteger(min) && Number.isInteger(max);
  const [memValue, setMemValue] = useState(defaultValue);
  const readOnceRef = useRef(false);

  useEffect(() => {
    // enabled=false 不读存储；首次翻 true 读一次，之后内存保留
    if (!enabled || readOnceRef.current) return;
    readOnceRef.current = true;
    let raw: string | null;
    try {
      raw = localStorage.getItem(key);
    } catch {
      return; // 存储访问异常不崩溃，保留默认
    }
    if (raw === null) return;
    const parsed = parsePersistedSize(raw, min, max, integerOnly);
    if (parsed !== null) setMemValue(parsed);
  }, [enabled, key, min, max, integerOnly]);

  const setSize = useCallback((next: number) => {
    setMemValue(next);
  }, []);

  const commitSize = useCallback(
    (next: number) => {
      setMemValue(next);
      if (!enabled) return; // 禁用期间不访问存储
      try {
        localStorage.setItem(key, JSON.stringify(next));
      } catch {
        // 写失败保留本次内存布局
      }
    },
    [key, enabled],
  );

  // 禁用对外返回默认值；保留内存供再次启用恢复
  return { value: enabled ? memValue : defaultValue, setSize, commitSize };
}

interface ResizeHandleProps {
  mode: ResizeMode;
  /** 当前尺寸（px 或比例），与 min/max 同单位。 */
  value: number;
  min: number;
  max: number;
  /** ratio 模式必填：容器宽度 px；零宽不启动拖拽。 */
  containerWidth?: number;
  /** 拖拽实时调整 / 取消恢复起始值：仅更新保留内存（px 为取整 clamp 后的值）。 */
  onDrag: (next: number) => void;
  /** 提交持久化：pointerup 保存一次；键盘每次有效调整立即保存。 */
  onCommit: (next: number) => void;
  /** 拖拽中变 false 一律按 cancelled 结束（布局失效统一出口）。 */
  enabled?: boolean;
  className?: string;
  style?: CSSProperties;
}

/** 拖拽事务：存在即视为 dragging，置 null 即离开 dragging
 * （pointerup 的 committed 清事务与 cancelled 清事务共用此出口）。
 * 记录启动 pointerId 与 capture 元素：隔离其他指针，释放用事务记录的 ID。 */
interface DragTx {
  startValue: number;
  startClientX: number;
  lastNext: number;
  pointerId: number;
  captureEl: HTMLDivElement;
}

export function ResizeHandle({
  mode,
  value,
  min,
  max,
  containerWidth,
  onDrag,
  onCommit,
  enabled = true,
  className,
  style,
}: ResizeHandleProps) {
  const dragRef = useRef<DragTx | null>(null);
  const onDragRef = useRef(onDrag);
  useLayoutEffect(() => {
    onDragRef.current = onDrag;
  });

  const clampToRange = useCallback((v: number) => Math.min(max, Math.max(min, v)), [min, max]);

  // 清事务后释放 capture；capture 已先行丢失（如 lostpointercapture）则跳过
  const releaseTx = (tx: DragTx) => {
    if (tx.captureEl.hasPointerCapture(tx.pointerId)) {
      tx.captureEl.releasePointerCapture(tx.pointerId);
    }
  };

  // cancelled 统一出口：恢复存活持有者起始保留内存、不写存储、清事务后释放 capture；
  // 此后迟到的 pointerup / lostpointercapture 无事务，不提交
  const cancelDrag = useCallback(() => {
    const tx = dragRef.current;
    if (!tx) return;
    dragRef.current = null;
    onDragRef.current(tx.startValue);
    releaseTx(tx);
  }, []);

  // 失效处理先于 handle 移除/模式失效执行：layout 阶段同步清事务，
  // 不留「禁用已提交、事务仍在」的可提交窗口
  useLayoutEffect(() => {
    if (!enabled) cancelDrag();
  }, [enabled, cancelDrag]);
  // handle 卸载 / 实例销毁
  useLayoutEffect(() => () => cancelDrag(), [cancelDrag]);

  const ratioUsable = mode === 'px' || (containerWidth !== undefined && containerWidth > 0);

  const computeNext = (tx: DragTx, clientX: number): number => {
    const delta = clientX - tx.startClientX;
    // px 统一取整后 clamp；比例 clamp(startRatio + deltaPx / containerWidth)
    if (mode === 'ratio') return clampToRange(tx.startValue + delta / (containerWidth as number));
    return clampToRange(Math.round(tx.startValue + delta));
  };

  const handlePointerDown = (e: ReactPointerEvent<HTMLDivElement>) => {
    if (dragRef.current || !enabled || !ratioUsable) return;
    if (!e.isPrimary || e.button !== 0) return;
    dragRef.current = {
      startValue: value,
      startClientX: e.clientX,
      lastNext: value,
      pointerId: e.pointerId,
      captureEl: e.currentTarget,
    };
    e.currentTarget.setPointerCapture(e.pointerId);
  };

  const handlePointerMove = (e: ReactPointerEvent<HTMLDivElement>) => {
    const tx = dragRef.current;
    if (!tx || !enabled || e.pointerId !== tx.pointerId) return;
    const next = computeNext(tx, e.clientX);
    tx.lastNext = next;
    onDrag(next);
  };

  const handlePointerUp = (e: ReactPointerEvent<HTMLDivElement>) => {
    const tx = dragRef.current;
    if (!tx || !enabled || e.pointerId !== tx.pointerId) return;
    dragRef.current = null; // committed：先清活动事务再保存一次
    onCommit(tx.lastNext);
    releaseTx(tx); // 最后 release capture：此后到达的 lostpointercapture 无事务，仅清理
  };

  const handlePointerCancel = (e: ReactPointerEvent<HTMLDivElement>) => {
    const tx = dragRef.current;
    if (!tx || e.pointerId !== tx.pointerId) return;
    cancelDrag();
  };

  const handleLostPointerCapture = (e: ReactPointerEvent<HTMLDivElement>) => {
    const tx = dragRef.current;
    if (!tx) return; // pointerup 已提交：不回滚、不再写
    if (e.pointerId !== tx.pointerId) return; // 其他指针的 capture 事件
    cancelDrag(); // 拖拽中意外 lostpointercapture → cancelled
  };

  const handleKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    if (dragRef.current || !enabled) return;
    const dir =
      e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0;
    if (dir === 0) return;
    if (!ratioUsable) return;
    e.preventDefault();
    // 方向键步进 10px，比例模式换算为 10px / 容器宽度
    const step = mode === 'ratio' ? KEYBOARD_STEP_PX / (containerWidth as number) : KEYBOARD_STEP_PX;
    const candidate = clampToRange(value + dir * step);
    if (candidate === value) return; // 无效调整不保存
    onCommit(candidate); // 每次有效调整立即保存
  };

  return (
    <div
      role="separator"
      aria-orientation="vertical"
      tabIndex={0}
      aria-valuenow={value}
      aria-valuemin={min}
      aria-valuemax={max}
      className={className ? `od-resize-handle ${className}` : 'od-resize-handle'}
      style={{
        width: HANDLE_WIDTH_PX,
        height: '100%',
        cursor: 'col-resize',
        touchAction: 'none',
        ...style,
      }}
      onPointerDown={handlePointerDown}
      onPointerMove={handlePointerMove}
      onPointerUp={handlePointerUp}
      onPointerCancel={handlePointerCancel}
      onLostPointerCapture={handleLostPointerCapture}
      onKeyDown={handleKeyDown}
    />
  );
}

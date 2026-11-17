import { useLayoutEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import type { Project, TaskSummary } from '../types';
import { taskStatusLabel } from './task-status';

export interface TaskHoverCardProps {
  id: string;
  task: TaskSummary;
  project: Project;
  anchor: HTMLElement;
  current: boolean;
  onEnter: () => void;
  onLeave: () => void;
}

const VIEWPORT_GAP = 8;
const ANCHOR_GAP = 10;

/** 只读任务摘要。portal 只承载浮层，不承载或重挂任何工作区内容。 */
export function TaskHoverCard({
  id,
  task,
  project,
  anchor,
  current,
  onEnter,
  onLeave,
}: TaskHoverCardProps) {
  const cardRef = useRef<HTMLDivElement>(null);
  const [position, setPosition] = useState({ left: VIEWPORT_GAP, top: VIEWPORT_GAP });

  useLayoutEffect(() => {
    const card = cardRef.current;
    if (!card || !anchor.isConnected) return;

    const anchorRect = anchor.getBoundingClientRect();
    const cardRect = card.getBoundingClientRect();
    const cardWidth = cardRect.width || 264;
    const cardHeight = cardRect.height || 150;
    const viewportWidth = window.innerWidth;
    const viewportHeight = window.innerHeight;

    let left = anchorRect.right + ANCHOR_GAP;
    if (left + cardWidth > viewportWidth - VIEWPORT_GAP) {
      left = anchorRect.left - cardWidth - ANCHOR_GAP;
    }
    left = Math.max(
      VIEWPORT_GAP,
      Math.min(left, Math.max(VIEWPORT_GAP, viewportWidth - cardWidth - VIEWPORT_GAP)),
    );

    const centeredTop = anchorRect.top + (anchorRect.height - cardHeight) / 2;
    const top = Math.max(
      VIEWPORT_GAP,
      Math.min(centeredTop, Math.max(VIEWPORT_GAP, viewportHeight - cardHeight - VIEWPORT_GAP)),
    );
    setPosition({ left, top });
  }, [anchor, project, task]);

  return createPortal(
    <div
      ref={cardRef}
      id={id}
      className="task-hover-card"
      role="tooltip"
      data-task-id={task.id}
      style={{ left: position.left, top: position.top }}
      onMouseEnter={onEnter}
      onMouseLeave={onLeave}
    >
      <div className="task-hover-card-head">
        <div className="task-hover-card-name">{task.name}</div>
        {current && <span className="task-hover-card-current">当前任务</span>}
      </div>
      <dl className="task-hover-card-details">
        <div className="task-hover-card-row">
          <dt>项目</dt>
          <dd>{project.name}</dd>
        </div>
        <div className="task-hover-card-row">
          <dt>当前状态</dt>
          <dd>{taskStatusLabel(task)}</dd>
        </div>
        <div className="task-hover-card-row">
          <dt>待处理</dt>
          <dd>{task.attention_count ?? 0}</dd>
        </div>
      </dl>
    </div>,
    document.body,
  );
}

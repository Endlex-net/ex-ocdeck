import { useCallback, useEffect, useId, useRef, useState } from 'react';
import type { Project } from '../types';
import { requestTerminalFocus } from '../terminal/focus-request';
import { orderSidebarGroups, transitionalLabel } from './sidebar-order';
import { taskStatusDotClass } from './task-status';
import { TaskHoverCard } from './TaskHoverCard';

export interface CompactTaskNavProps {
  projects: Project[];
  currentTaskID?: string;
  variant: 'sidebar' | 'zen';
  visible?: boolean;
}

interface ActiveHover {
  taskID: string;
  projectID: string;
  anchor: HTMLAnchorElement;
}

function ownerIsHidden(owner: HTMLElement): boolean {
  let node: HTMLElement | null = owner;
  while (node) {
    if (node.hidden) return true;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.visibility === 'collapse') {
      return true;
    }
    node = node.parentElement;
  }
  return false;
}

/** 全局折叠侧栏与 Zen 共用的任务点导航；projects 完全由调用方共享快照传入。 */
export function CompactTaskNav({
  projects,
  currentTaskID,
  variant,
  visible = true,
}: CompactTaskNavProps) {
  const ownerRef = useRef<HTMLElement>(null);
  const closeTimerRef = useRef<number | null>(null);
  const [active, setActive] = useState<ActiveHover | null>(null);
  const reactID = useId();
  const tooltipID = `compact-task-hover-${reactID.replace(/:/g, '')}`;
  const groups = orderSidebarGroups(projects);

  const clearCloseTimer = useCallback(() => {
    if (closeTimerRef.current !== null) {
      window.clearTimeout(closeTimerRef.current);
      closeTimerRef.current = null;
    }
  }, []);

  const closeNow = useCallback(() => {
    clearCloseTimer();
    setActive(null);
  }, [clearCloseTimer]);

  const scheduleClose = useCallback(() => {
    clearCloseTimer();
    closeTimerRef.current = window.setTimeout(() => {
      closeTimerRef.current = null;
      setActive(null);
    }, 100);
  }, [clearCloseTimer]);

  const openNow = useCallback(
    (taskID: string, projectID: string, anchor: HTMLAnchorElement) => {
      clearCloseTimer();
      setActive({ taskID, projectID, anchor });
    },
    [clearCloseTimer],
  );

  useEffect(() => {
    if (!visible) {
      closeNow();
      return;
    }
    const owner = ownerRef.current;
    if (!owner) return;

    const checkOwner = () => {
      if (!owner.isConnected || ownerIsHidden(owner)) closeNow();
    };
    const onVisibilityChange = () => {
      if (document.hidden) closeNow();
    };
    const onScroll = (event: Event) => {
      const target = event.target;
      // window capture 也会收到 portal 卡片的 scroll；仅当前卡片内部滚动豁免，
      // 任务列表、owner、页面及其他滚动仍立即关闭。
      if (target instanceof Node && document.getElementById(tooltipID)?.contains(target)) return;
      closeNow();
    };

    owner.addEventListener('scroll', closeNow);
    window.addEventListener('scroll', onScroll, true);
    window.addEventListener('resize', closeNow);
    window.addEventListener('blur', closeNow);
    window.addEventListener('pagehide', closeNow);
    document.addEventListener('visibilitychange', onVisibilityChange);

    const media = typeof window.matchMedia === 'function'
      ? window.matchMedia('(max-width: 767px)')
      : null;
    media?.addEventListener('change', closeNow);

    const mutationObserver = typeof MutationObserver === 'undefined'
      ? null
      : new MutationObserver(checkOwner);
    if (mutationObserver) {
      let node: HTMLElement | null = owner;
      while (node) {
        mutationObserver.observe(node, {
          attributes: true,
          attributeFilter: ['class', 'style', 'hidden'],
        });
        node = node.parentElement;
      }
    }

    const resizeObserver = typeof ResizeObserver === 'undefined'
      ? null
      : new ResizeObserver(checkOwner);
    resizeObserver?.observe(owner);

    return () => {
      clearCloseTimer();
      owner.removeEventListener('scroll', closeNow);
      window.removeEventListener('scroll', onScroll, true);
      window.removeEventListener('resize', closeNow);
      window.removeEventListener('blur', closeNow);
      window.removeEventListener('pagehide', closeNow);
      document.removeEventListener('visibilitychange', onVisibilityChange);
      media?.removeEventListener('change', closeNow);
      mutationObserver?.disconnect();
      resizeObserver?.disconnect();
    };
  }, [clearCloseTimer, closeNow, tooltipID, visible]);

  const activeGroup = active
    ? groups.find(
        (group) =>
          group.project.id === active.projectID &&
          group.tasks.some((task) => task.id === active.taskID),
      )
    : undefined;
  const activeTask = activeGroup?.tasks.find((task) => task.id === active?.taskID);

  useEffect(() => {
    if (active && (!activeGroup || !activeTask)) closeNow();
  }, [active, activeGroup, activeTask, closeNow]);

  if (!visible) return null;

  return (
    <nav
      ref={ownerRef}
      className={`compact-task-nav compact-task-nav--${variant}`}
      aria-label="任务导航"
    >
      <div className="compact-task-list" onScroll={closeNow}>
        {groups.map(({ project, tasks }) => (
          <div className="compact-task-group" key={project.id} role="group" aria-label={project.name}>
            {tasks.map((task) => {
              const transitional = transitionalLabel(task.status);
              const described =
                active?.taskID === task.id && active.projectID === project.id && activeTask
                  ? tooltipID
                  : undefined;
              const current = currentTaskID === task.id;
              return (
                <a
                  key={task.id}
                  className={`compact-task-item${current ? ' compact-task-item--current' : ''}`}
                  href={`#/task/${task.id}`}
                  aria-label={`${task.name}，${project.name}`}
                  aria-current={current ? 'page' : undefined}
                  aria-describedby={described}
                  onMouseEnter={(event) => openNow(task.id, project.id, event.currentTarget)}
                  onMouseLeave={scheduleClose}
                  onFocus={(event) => openNow(task.id, project.id, event.currentTarget)}
                  onBlur={scheduleClose}
                  onClick={() => {
                    closeNow();
                    requestTerminalFocus(task.id);
                  }}
                >
                  {transitional ? (
                    <span className="od-spinner compact-task-spinner" aria-hidden="true" />
                  ) : (
                    <span
                      className={`od-agent od-agent-${taskStatusDotClass(task)}`}
                      aria-hidden="true"
                    >
                      <span className="od-agent-dot" />
                    </span>
                  )}
                </a>
              );
            })}
          </div>
        ))}
      </div>
      {active && activeGroup && activeTask && (
        <TaskHoverCard
          id={tooltipID}
          task={activeTask}
          project={activeGroup.project}
          anchor={active.anchor}
          current={currentTaskID === activeTask.id}
          onEnter={clearCloseTimer}
          onLeave={scheduleClose}
        />
      )}
    </nav>
  );
}

import { useEffect, useMemo, useRef, useState } from 'react';
import { navigate } from '../router';
import { useProjects } from '../hooks';
import { SearchIcon } from '../icons';

/** 命令面板条目（统一形态，静态入口与动态任务/操作共用）。 */
export interface PaletteCommand {
  group: string;
  label: string;
  hint?: string;
  /** 跳转 hash 路径（如 "#/"）。有 action 时可省略。 */
  href?: string;
  /** 执行动作（优先于 href）。 */
  action?: () => void;
  /** 关键词（含中文，用于模糊匹配）。 */
  keywords?: string;
  /** agent 状态点（仅任务条目）；attention = 等待人工（待答问题/待授权限），优先于运行态。 */
  dot?: 'idle' | 'busy' | 'retry' | 'attention';
}

/** 模糊匹配：query 按空白分词，每个词须作为子串出现在 group+label+hint+keywords 拼接串中。
 *  与 docs/design/assets/ocdeck-palette.js matches() 同语义（含中文关键词）。
 *  纯函数，可独立测试。 */
export function matchCommand(cmd: PaletteCommand, q: string): boolean {
  if (!q) return true;
  const hay = `${cmd.group} ${cmd.label} ${cmd.hint ?? ''} ${cmd.keywords ?? ''}`.toLowerCase();
  return q
    .toLowerCase()
    .split(/\s+/)
    .filter(Boolean)
    .every((w) => hay.includes(w));
}

interface CommandPaletteProps {
  open: boolean;
  onClose: () => void;
  /** 新建任务操作回调（跳转指挥中心 + 聚焦新建入口，由 App/P3 接线）。 */
  onNewTask?: () => void;
  /** 注册项目操作回调（跳转项目管理）。 */
  onRegisterProject?: () => void;
}

/** ⌘K 全局命令面板（design.md D10 + spec web-ui-shell）：
 *  6 个静态入口（指挥中心/项目管理/设置四子标签深链）+ 任务列表（共享 store）+ 新建任务/注册项目操作。
 *  模糊匹配含中文；↑↓/Enter/Esc；⌘K/Ctrl+K 由 App 层监听。无第三方组件库。 */
export function CommandPalette({ open, onClose, onNewTask, onRegisterProject }: CommandPaletteProps) {
  const { projects } = useProjects();
  const [query, setQuery] = useState('');
  const [cursor, setCursor] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  // 命令注册表：6 静态入口 + 任务列表 + 操作（与 ocdeck-palette.js GLOBAL 同源）。
  const allCommands = useMemo<PaletteCommand[]>(() => {
    const statics: PaletteCommand[] = [
      { group: '页面', label: '指挥中心', hint: '首页', href: '/', keywords: 'command center home 首页 活跃' },
      { group: '页面', label: '项目管理', hint: '工作区', href: '/projects', keywords: 'projects workspace 项目 工作区' },
      { group: '页面', label: '设置 · 终端外观', href: '/configs#appearance', keywords: 'settings terminal font 设置 终端 字体' },
      { group: '页面', label: '设置 · 环境变量', href: '/configs#env', keywords: 'settings env 环境变量' },
      { group: '页面', label: '设置 · opencode 配置', href: '/configs#opencode', keywords: 'settings opencode config 配置文件' },
      { group: '页面', label: '设置 · AI 配置', href: '/configs#ai', keywords: 'settings ai provider key 模型' },
    ];
    const tasks: PaletteCommand[] = [];
    for (const p of projects) {
      for (const t of p.tasks ?? []) {
        if (t.status === 'archived') continue;
        const dot = (t.attention_count ?? 0) > 0 ? 'attention' : t.agentStatus === 'busy' || t.agentStatus === 'retry' ? t.agentStatus : t.agentStatus === 'idle' ? 'idle' : undefined;
        tasks.push({
          group: '任务',
          label: t.name,
          hint: `${p.name} · ${t.branch || t.status}`,
          href: `/task/${t.id}`,
          keywords: `task workbench 任务 工作台 ${p.name} ${t.branch ?? ''}`,
          dot,
        });
      }
    }
    const actions: PaletteCommand[] = [
      {
        group: '操作',
        label: '新建任务',
        hint: '指挥中心',
        href: '/',
        keywords: 'new create task 新建 创建',
        action: onNewTask,
      },
      {
        group: '操作',
        label: '注册项目',
        hint: '项目管理',
        href: '/projects',
        keywords: 'register add project 注册 添加',
        action: onRegisterProject,
      },
    ];
    return [...statics, ...tasks, ...actions];
  }, [projects, onNewTask, onRegisterProject]);

  // 过滤后的命令列表（模糊匹配含中文）。
  const filtered = useMemo(() => {
    const q = query.trim();
    return allCommands.filter((c) => matchCommand(c, q));
  }, [allCommands, query]);

  // 打开时重置 query + cursor + 聚焦输入。
  useEffect(() => {
    if (open) {
      setQuery('');
      setCursor(0);
      // 下一帧聚焦（DOM 挂载后）。
      requestAnimationFrame(() => inputRef.current?.focus());
    }
  }, [open]);

  // cursor 变化时滚动到可见（手动算 offset，避免 scrollIntoView 影响外层）。
  useEffect(() => {
    if (!open || !listRef.current) return;
    const nodes = listRef.current.querySelectorAll<HTMLElement>('[data-cmd-idx]');
    const el = nodes[cursor];
    if (el) {
      listRef.current.scrollTop = el.offsetTop - listRef.current.clientHeight / 2 + el.clientHeight / 2;
    }
  }, [cursor, open]);

  if (!open) return null;

  const run = (cmd: PaletteCommand) => {
    onClose();
    if (cmd.action) {
      cmd.action();
      return;
    }
    if (cmd.href) navigate(cmd.href);
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      e.preventDefault();
      onClose();
    } else if (e.key === 'ArrowDown') {
      e.preventDefault();
      setCursor((c) => Math.min(filtered.length - 1, c + 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setCursor((c) => Math.max(0, c - 1));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      const cmd = filtered[cursor];
      if (cmd) run(cmd);
    }
  };

  // 分组渲染：保留 allCommands 顺序，按 group 连续渲染时插入分组标题。
  let lastGroup = '';
  let renderedIdx = -1;

  return (
    <div
      className="od-palette-overlay open"
      data-od-id="command-palette"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div className="od-palette" role="dialog" aria-modal="true" aria-label="命令面板">
        <div className="od-palette-input-row">
          <SearchIcon />
          <input
            ref={inputRef}
            className="od-palette-input"
            type="text"
            placeholder="搜索页面、任务、操作…"
            aria-label="搜索命令"
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setCursor(0);
            }}
            onKeyDown={onKeyDown}
          />
          <span className="od-palette-kbd">esc</span>
        </div>
        <div className="od-palette-list" role="listbox" ref={listRef}>
          {filtered.length === 0 ? (
            <div className="od-palette-empty">没有匹配的命令</div>
          ) : (
            filtered.map((cmd) => {
              renderedIdx += 1;
              const idx = renderedIdx;
              const showGroup = cmd.group !== lastGroup;
              lastGroup = cmd.group;
              return (
                <div key={`${cmd.group}-${cmd.label}-${idx}`}>
                  {showGroup && <div className="od-palette-group">{cmd.group}</div>}
                  <button
                    type="button"
                    className={`od-palette-item${cursor === idx ? ' current' : ''}`}
                    role="option"
                    aria-selected={cursor === idx ? 'true' : 'false'}
                    data-cmd-idx={idx}
                    onMouseMove={() => setCursor(idx)}
                    onClick={() => run(cmd)}
                  >
                    {cmd.dot && (
                      <span className={`od-agent od-agent-${cmd.dot} od-palette-dot`} aria-hidden="true">
                        <span className="od-agent-dot"></span>
                      </span>
                    )}
                    <span className="od-palette-label">{cmd.label}</span>
                    {cmd.hint && <span className="od-palette-hint">{cmd.hint}</span>}
                  </button>
                </div>
              );
            })
          )}
        </div>
        <div className="od-palette-foot">
          <span>
            <b>↑↓</b>选择
          </span>
          <span>
            <b>⏎</b>执行
          </span>
          <span>
            <b>esc</b>关闭
          </span>
        </div>
      </div>
    </div>
  );
}
import { useEffect, useMemo, useRef, useState } from 'react';
import { rankByQuery, foldForMatch } from '../fuzzy-match';
import { navigate } from '../router';
import { useProjects } from '../hooks';
import { SearchIcon } from '../icons';
import type { PaletteFocusPayload, PaletteMatchMode } from '../palette-focus';

/** ECMAScript WhiteSpace + LineTerminator（与 Go isECMAScriptSpace / PaletteConfigPanel 同源，不得用 /\s/）。 */
const ECMA_SCRIPT_SPACE_CODES = new Set([
  0x0009, 0x000a, 0x000b, 0x000c, 0x000d, 0x0020, 0x00a0, 0x1680, 0x2000, 0x2001, 0x2002, 0x2003,
  0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000,
  0xfeff,
]);

function isECMAScriptSpaceCode(code: number): boolean {
  return ECMA_SCRIPT_SPACE_CODES.has(code);
}

function trimECMAScript(s: string): string {
  let start = 0;
  let end = s.length;
  while (start < end && isECMAScriptSpaceCode(s.charCodeAt(start))) start += 1;
  while (end > start && isECMAScriptSpaceCode(s.charCodeAt(end - 1))) end -= 1;
  return s.slice(start, end);
}

/** 触发词快速新建解析：大小写不敏感字面前缀 + 尾随空白才进入模式。空白边界与余文切片用原始 UTF-16 下标。 */
export function parseQuickCreateQuery(query: string, triggerWord: string): { projectQuery: string } | null {
  if (foldForMatch(query.slice(0, triggerWord.length)) !== foldForMatch(triggerWord)) return null;
  if (query.length <= triggerWord.length) return null;
  if (!isECMAScriptSpaceCode(query.charCodeAt(triggerWord.length))) return null;
  return { projectQuery: trimECMAScript(query.slice(triggerWord.length)) };
}

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
  onNewTask?: (payload?: PaletteFocusPayload) => void;
  /** 注册项目操作回调（跳转项目管理）。 */
  onRegisterProject?: () => void;
  /** App 下发的快速新建触发词（D-3 消费）。 */
  triggerWord?: string;
  /** App 下发的匹配模式（D-3 消费）。 */
  matchMode?: PaletteMatchMode;
}

/** ⌘K 全局命令面板（design.md D10 + spec web-ui-shell）：
 *  7 个静态入口（指挥中心/项目管理/设置五子标签深链）+ 任务列表（共享 store）+ 新建任务/注册项目操作。
 *  模糊匹配含中文；↑↓/Enter/Esc；⌘K/Ctrl+K 由 App 层监听。无第三方组件库。 */
export function CommandPalette({
  open,
  onClose,
  onNewTask,
  onRegisterProject,
  triggerWord = 'new',
}: CommandPaletteProps) {
  const { projects } = useProjects();
  const [query, setQuery] = useState('');
  const [cursor, setCursor] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  // 命令注册表：7 静态入口 + 任务列表 + 操作（与 ocdeck-palette.js GLOBAL 同源）。
  const allCommands = useMemo<PaletteCommand[]>(() => {
    const statics: PaletteCommand[] = [
      { group: '页面', label: '指挥中心', hint: '首页', href: '/', keywords: 'command center home 首页 活跃' },
      { group: '页面', label: '项目管理', hint: '工作区', href: '/projects', keywords: 'projects workspace 项目 工作区' },
      { group: '页面', label: '设置 · 终端外观', href: '/configs#appearance', keywords: 'settings terminal font 设置 终端 字体' },
      { group: '页面', label: '设置 · 环境变量', href: '/configs#env', keywords: 'settings env 环境变量' },
      { group: '页面', label: '设置 · opencode 配置', href: '/configs#opencode', keywords: 'settings opencode config 配置文件' },
      { group: '页面', label: '设置 · AI 配置', href: '/configs#ai', keywords: 'settings ai provider key 模型' },
      { group: '页面', label: '设置 · 命令面板', href: '/configs#palette', keywords: 'palette 命令面板 设置' },
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

  // 过滤后的命令列表：触发词+空白进入快速新建；否则既有模糊匹配。
  const filtered = useMemo(() => {
    const parsed = parseQuickCreateQuery(query, triggerWord);
    if (parsed) {
      const createTask: PaletteCommand = {
        group: '操作',
        label: '新建任务',
        hint: '指挥中心',
        href: '/',
        keywords: 'new create task 新建 创建',
        action: onNewTask ? () => onNewTask({ projectName: parsed.projectQuery }) : undefined,
      };
      let ranked = rankByQuery(projects, parsed.projectQuery);
      if (parsed.projectQuery !== '' && ranked.length === 0) {
        ranked = rankByQuery(projects, '');
      }
      const projectCmds: PaletteCommand[] = ranked.map((p) => ({
        group: '项目',
        label: p.name,
        hint: p.path,
        href: '/',
        action: onNewTask ? () => onNewTask({ projectName: p.name, projectID: p.id }) : undefined,
      }));
      return [createTask, ...projectCmds];
    }
    const q = query.trim();
    return allCommands.filter((c) => matchCommand(c, q));
  }, [allCommands, onNewTask, projects, query, triggerWord]);

  // 候选收缩（共享项目快照更新等）后 cursor 可能越界：渲染/键盘统一用 clamp 后的
  // effectiveCursor，并在列表长度变化后回写 state，保证 Enter 始终执行当前首选项。
  const effectiveCursor = Math.min(cursor, Math.max(0, filtered.length - 1));
  useEffect(() => {
    setCursor((c) => Math.min(c, Math.max(0, filtered.length - 1)));
  }, [filtered.length]);

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
    const el = nodes[effectiveCursor];
    if (el) {
      listRef.current.scrollTop = el.offsetTop - listRef.current.clientHeight / 2 + el.clientHeight / 2;
    }
  }, [effectiveCursor, open]);

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
      const cmd = filtered[effectiveCursor];
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
                    className={`od-palette-item${effectiveCursor === idx ? ' current' : ''}`}
                    role="option"
                    aria-selected={effectiveCursor === idx ? 'true' : 'false'}
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
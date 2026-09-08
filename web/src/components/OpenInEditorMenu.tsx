import { useEffect, useRef, useState } from 'react';
import {
  buildEditorUri,
  EDITOR_TOOLS_CHANGED,
  loadDefaultTool,
  loadEditorTools,
  loadEditorUriTemplate,
  resolveDefaultTool,
  saveDefaultTool,
  type EditorTool,
} from '../editor-tools';
import { CaretDownIcon, GoLandIcon, VSCodeIcon } from '../icons';
import { shouldCloseOverflowOnBlur } from '../pages/workbench-overflow';

/* ============================ 任务详情页「在编辑器中打开」入口（add-frontend-tool-quick-open 3.1/3.2） ============================
 * 「默认工具图标主按钮 + ⌄ 下拉」组合：数据源 loadEditorTools + loadDefaultTool，
 * 监听 EDITOR_TOOLS_CHANGED + storage 同时重读两类键再经 resolveDefaultTool 派生默认
 * （覆盖开关与默认值两类变更、跨标签页即时收敛）。无已启用工具返回 null；
 * worktree_path 空/缺失保留组合但双按钮禁用。disclosure 模式参照 WorkbenchOverflow
 * （Escape/外部点击关闭、打开聚焦首项）。打开目标 = worktree_path，前端不按任务模式改写。 */

const TOOL_LABEL: Record<EditorTool, string> = { vscode: 'VSCode', goland: 'GoLand' };
/** 下拉展示与默认回退顺序（spec 固定：VSCode → GoLand）。 */
const TOOL_ORDER: readonly EditorTool[] = ['vscode', 'goland'];

function ToolIcon({ tool }: { tool: EditorTool }) {
  return tool === 'vscode' ? <VSCodeIcon /> : <GoLandIcon />;
}

export function OpenInEditorMenu({ worktreePath }: { worktreePath: string }) {
  const [tools, setTools] = useState(() => loadEditorTools());
  const [stored, setStored] = useState(() => loadDefaultTool());
  const [menuOpen, setMenuOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);

  // 开关变更与默认值变更（本页写入 / 另一标签页 storage）都重读两类键收敛。
  useEffect(() => {
    const reload = () => {
      setTools(loadEditorTools());
      setStored(loadDefaultTool());
    };
    window.addEventListener(EDITOR_TOOLS_CHANGED, reload);
    window.addEventListener('storage', reload);
    return () => {
      window.removeEventListener(EDITOR_TOOLS_CHANGED, reload);
      window.removeEventListener('storage', reload);
    };
  }, []);

  const enabledTools = TOOL_ORDER.filter((t) => tools[t]);
  const defaultTool = resolveDefaultTool(tools, stored);

  // disclosure 模式：打开后焦点进入菜单内首个可用项
  useEffect(() => {
    if (menuOpen) {
      menuRef.current?.querySelector<HTMLButtonElement>('button:not(:disabled)')?.focus();
    }
  }, [menuOpen]);

  // 关闭下拉；键盘取消（Escape）/失焦兜底后焦点恢复触发器
  const closeMenu = (restoreFocus: boolean) => {
    setMenuOpen(false);
    if (restoreFocus) triggerRef.current?.focus();
  };

  // 无已启用工具 → 完全隐藏、无占位（spec）；hook 已全部执行，此处才可提前返回。
  if (enabledTools.length === 0 || defaultTool === null) return null;

  // worktree_path 为空/缺失：组合入口保留，双按钮禁用（点击即不可能触发唤起/写入）。
  const disabled = !worktreePath;

  /** 点击主流程（spec 执行顺序）：主按钮 ①→③；下拉选择 ①→②→③。 */
  const openWith = (tool: EditorTool, viaDropdown: boolean) => {
    // ① 前置校验 + URI 构造：点击时重读存储（不用渲染快照，覆盖「工具已被关闭但未派发事件」）；
    // 构造整体兜异常（encodeURIComponent 对孤立 UTF-16 代理对抛 URIError 等）。
    // 任一失败零副作用（不写存储、不派发、不唤起、不改默认），仅关菜单。
    let uri = '';
    try {
      uri =
        loadEditorTools()[tool] && worktreePath
          ? buildEditorUri(tool, worktreePath, loadEditorUriTemplate(tool))
          : '';
    } catch {
      uri = '';
    }
    if (uri === '') {
      setMenuOpen(false);
      return;
    }
    // ②（仅下拉选择）写默认工具键；失败捕获保留原默认与 ✓，不唤起，菜单关闭。
    if (viaDropdown) {
      try {
        saveDefaultTool(tool);
      } catch {
        setMenuOpen(false);
        return;
      }
      setStored(tool);
    }
    // ③ 同一用户手势调用链内同步唤起；同步异常捕获、页面保持可用、已保存默认不回滚。
    try {
      window.location.assign(uri);
    } catch {
      /* 唤起失败不可观测（design D2 行为边界），不做状态变更 */
    }
    setMenuOpen(false);
  };

  return (
    <span
      className="header-overflow"
      onBlur={(e) => {
        // React onBlur 冒泡：焦点真实落到组合之外才关闭；relatedTarget=null 不关
        // （触屏/Safari 点击不转移焦点，由 backdrop click 兜底，见 shouldCloseOverflowOnBlur）。
        const next = e.relatedTarget as Node | null;
        if (shouldCloseOverflowOnBlur(next, (n) => e.currentTarget.contains(n))) closeMenu(false);
      }}
    >
      <button
        className="btn btn-small btn-ghost"
        aria-label={`使用 ${TOOL_LABEL[defaultTool]} 打开`}
        title={`使用 ${TOOL_LABEL[defaultTool]} 打开`}
        disabled={disabled}
        onClick={() => openWith(defaultTool, false)}
      >
        <ToolIcon tool={defaultTool} />
      </button>
      <button
        ref={triggerRef}
        className="btn btn-small btn-ghost"
        aria-label="选择编辑器"
        title="选择编辑器"
        aria-haspopup="true"
        aria-expanded={menuOpen}
        aria-controls="open-in-editor-menu"
        disabled={disabled}
        onClick={() => setMenuOpen((o) => !o)}
        onKeyDown={(e) => {
          // 菜单开着但焦点仍在触发器时 Escape 也可关闭
          if (e.key === 'Escape' && menuOpen) {
            e.stopPropagation();
            closeMenu(false);
          }
        }}
      >
        <CaretDownIcon />
      </button>
      {menuOpen && (
        <>
          <div className="overflow-backdrop" onClick={() => closeMenu(true)} />
          <div
            ref={menuRef}
            id="open-in-editor-menu"
            className="overflow-menu"
            onKeyDown={(e) => {
              if (e.key === 'Escape') {
                e.stopPropagation();
                closeMenu(true);
              }
            }}
          >
            {enabledTools.map((t) => (
              <button key={t} className="overflow-item" onClick={() => openWith(t, true)}>
                <ToolIcon tool={t} /> {TOOL_LABEL[t]}
                {t === defaultTool ? ' ✓' : ''}
              </button>
            ))}
          </div>
        </>
      )}
    </span>
  );
}

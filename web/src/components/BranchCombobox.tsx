import { useEffect, useId, useRef, useState } from 'react';

interface BranchComboboxProps {
  /** 候选分支短名（调用方保证当前 value 总在列表内）。 */
  options: string[];
  value: string;
  /** 项目默认分支（选项内标注"（默认）"）。 */
  defaultBranch: string;
  onChange: (v: string) => void;
  title?: string;
  /** 下拉打开时回调（页面用于首次打开自动 refresh 远端分支，D10）。 */
  onOpen?: () => void;
  /** refresh 进行中：下拉内显示 loading 提示。 */
  loading?: boolean;
}

/**
 * 基线分支搜索选择 combobox（add-plain-dir-project D10）：
 * - 输入即过滤（子串匹配、大小写不敏感）；
 * - ↑/↓ 移动高亮、Enter 确认、Esc 关闭并还原、点击外部（blur）关闭；
 * - 选中项高亮标注，默认分支标注"（默认）"；
 * - 列表超长时滚动（CSS max-height）。
 */
export function BranchCombobox({
  options,
  value,
  defaultBranch,
  onChange,
  title,
  onOpen,
  loading = false,
}: BranchComboboxProps) {
  const [open, setOpen] = useState(false);
  // dirty：打开后用户是否已开始输入（未输入时输入框显示当前选中值、列表不过滤）
  const [dirty, setDirty] = useState(false);
  const [query, setQuery] = useState('');
  const [highlight, setHighlight] = useState(0);
  const listID = useId();
  const rootRef = useRef<HTMLDivElement>(null);

  const q = dirty ? query.trim().toLowerCase() : '';
  const filtered = q ? options.filter((o) => o.toLowerCase().includes(q)) : options;

  // 打开时高亮落在当前选中项上
  useEffect(() => {
    if (!open) return;
    const idx = filtered.indexOf(value);
    setHighlight(idx >= 0 ? idx : 0);
    // 仅在打开时定位一次，后续高亮由键盘驱动
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  // 过滤结果变化时收敛高亮下标
  useEffect(() => {
    if (highlight >= filtered.length) setHighlight(Math.max(0, filtered.length - 1));
  }, [filtered.length, highlight]);

  // 点击组件外部关闭（blur 之外的兜底，如点击页面非可聚焦区域）
  useEffect(() => {
    if (!open) return;
    const onDocDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) close();
    };
    document.addEventListener('mousedown', onDocDown);
    return () => document.removeEventListener('mousedown', onDocDown);
  }, [open]);

  const close = () => {
    setOpen(false);
    setDirty(false);
    setQuery('');
  };

  // 统一打开入口：触发 onOpen（首次打开自动 refresh 等），打开方自行去重
  const openList = () => {
    setOpen(true);
    onOpen?.();
  };

  const pick = (v: string) => {
    onChange(v);
    close();
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (!open) {
        openList();
        return;
      }
      if (filtered.length === 0) return;
      const delta = e.key === 'ArrowDown' ? 1 : -1;
      setHighlight((h) => (h + delta + filtered.length) % filtered.length);
    } else if (e.key === 'Enter') {
      // combobox 内的回车只用于确认选择，绝不冒泡触发表单提交
      e.preventDefault();
      if (open && filtered.length > 0) pick(filtered[highlight]);
      else openList();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      close();
    }
  };

  const activeOptionID = open && filtered.length > 0 ? `${listID}-${highlight}` : undefined;

  return (
    <div className="combobox" ref={rootRef}>
      <input
        className="input combobox-input"
        role="combobox"
        aria-expanded={open}
        aria-controls={listID}
        aria-activedescendant={activeOptionID}
        aria-autocomplete="list"
        title={title}
        placeholder="搜索并选择基线分支"
        value={open && dirty ? query : value}
        onFocus={openList}
        onChange={(e) => {
          setQuery(e.target.value);
          setDirty(true);
          if (!open) openList();
        }}
        onKeyDown={onKeyDown}
        onBlur={close}
      />
      {open && (
        <div className="combobox-list" role="listbox" id={listID}>
          {loading && (
            <div className="combobox-empty">
              <span className="spinner" aria-hidden /> 刷新远端分支中…
            </div>
          )}
          {filtered.length === 0 ? (
            <div className="combobox-empty">无匹配分支</div>
          ) : (
            filtered.map((b, i) => (
              <button
                key={b}
                type="button"
                role="option"
                id={`${listID}-${i}`}
                aria-selected={b === value}
                className={`combobox-option${i === highlight ? ' combobox-option-active' : ''}${
                  b === value ? ' combobox-option-selected' : ''
                }`}
                // mousedown 先于 blur：阻止输入框失焦，保证点击选项可完成选择
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => pick(b)}
                onMouseEnter={() => setHighlight(i)}
              >
                {b === defaultBranch ? `${b}（默认）` : b}
              </button>
            ))
          )}
        </div>
      )}
    </div>
  );
}

import { useState } from 'react';
import {
  clearTermPrefs,
  DEFAULT_FONT_FAMILY,
  DEFAULT_FONT_SIZE,
  loadTermPrefs,
  saveTermPrefs,
  TERM_PREFS_CHANGED,
  validateFontSize,
} from '../terminal/preferences';

const CJK_HINT =
  '自定义字体栈需自行包含 CJK 字体（如 PingFang SC / 更纱黑体），否则中文无法显示。';

/**
 * 终端外观编辑器（design.md D4）：fontFamily + fontSize 偏好，存 localStorage。
 * 保存前整体校验，成功后派发变更事件；存储异常显示错误且不派发事件。
 */
export function TermAppearanceEditor() {
  const init = loadTermPrefs();
  const [fontFamily, setFontFamily] = useState(init.fontFamily ?? '');
  const [fontSize, setFontSize] = useState(
    init.fontSize !== undefined ? String(init.fontSize) : '',
  );
  const [error, setError] = useState('');
  const [saved, setSaved] = useState(false);

  const handleSave = () => {
    setError('');
    setSaved(false);

    const ffTrim = fontFamily.trim();
    const fsRaw = fontSize.trim();

    // fontSize 非空时必须合法；fontFamily 空白视为未设置。
    const prefs: { fontFamily?: string; fontSize?: number } = {};
    if (ffTrim !== '') prefs.fontFamily = ffTrim;
    if (fsRaw !== '') {
      const v = validateFontSize(fsRaw);
      if (v === null) {
        setError('字号必须为 8–32 之间的整数');
        return;
      }
      prefs.fontSize = v;
    }

    try {
      saveTermPrefs(prefs);
    } catch (err) {
      setError(err instanceof Error ? err.message : '保存失败');
      return;
    }
    // 归一为已存值显示
    setFontFamily(prefs.fontFamily ?? '');
    setFontSize(prefs.fontSize !== undefined ? String(prefs.fontSize) : '');
    window.dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED));
    setSaved(true);
  };

  const handleReset = () => {
    setError('');
    setSaved(false);
    try {
      clearTermPrefs();
    } catch (err) {
      setError(err instanceof Error ? err.message : '清除失败');
      return;
    }
    setFontFamily('');
    setFontSize('');
    window.dispatchEvent(new CustomEvent(TERM_PREFS_CHANGED));
  };

  return (
    <div className="term-appearance">
      {error && <div className="error-line">{error}</div>}
      {saved && <div className="term-appearance-saved">已保存并即时生效</div>}
      <div className="term-appearance-row">
        <label className="term-appearance-label">字体栈</label>
        <input
          className="input input-grow mono"
          value={fontFamily}
          placeholder={DEFAULT_FONT_FAMILY}
          onChange={(e) => setFontFamily(e.target.value)}
        />
      </div>
      <div className="term-appearance-row">
        <label className="term-appearance-label">字号</label>
        <input
          className="input term-appearance-size"
          value={fontSize}
          placeholder={String(DEFAULT_FONT_SIZE)}
          onChange={(e) => setFontSize(e.target.value)}
        />
      </div>
      <div className="term-appearance-hint">ⓘ {CJK_HINT}</div>
      <div className="term-appearance-actions">
        <button className="btn btn-primary btn-small" onClick={handleSave}>
          保存
        </button>
        <button className="btn btn-small btn-ghost" onClick={handleReset}>
          恢复默认
        </button>
      </div>
    </div>
  );
}
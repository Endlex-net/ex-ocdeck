import { useEffect, useState } from 'react';
import { api, ApiError } from '../api';
import { formatHotkey, normalizeHotkey, validateCanonicalHotkey } from '../hotkey';
import {
  PALETTE_CONFIG_CHANGED_EVENT,
  type PaletteConfig,
  type PaletteMatchMode,
} from '../palette-focus';

export type PaletteConfigLoadState = 'loading' | 'ready' | 'error';

export const DEFAULT_PALETTE_CONFIG: PaletteConfig = {
  hotkey: 'mod+k',
  triggerWord: 'new',
  matchMode: 'exact-then-substring',
};

const TRIGGER_MAX_CODE_POINTS = 32;

const MATCH_MODE_ITEMS: { value: PaletteMatchMode; label: string; desc: string }[] = [
  { value: 'exact', label: '仅精确匹配', desc: '忽略大小写，仅精确命中才预选' },
  {
    value: 'exact-then-substring',
    label: '精确优先、否则唯一子串',
    desc: '精确匹配优先；无精确命中时取唯一子串匹配',
  },
];

/** ECMAScript WhiteSpace + LineTerminator（与 Go isECMAScriptSpace 同源，不得用 /\s/）。 */
const ECMA_SCRIPT_SPACES = new Set([
  0x0009, 0x000a, 0x000b, 0x000c, 0x000d, 0x0020, 0x00a0, 0x1680, 0x2000, 0x2001, 0x2002, 0x2003,
  0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000,
  0xfeff,
]);

function isECMAScriptSpace(ch: string): boolean {
  const cp = ch.codePointAt(0);
  return cp !== undefined && ECMA_SCRIPT_SPACES.has(cp);
}

function validateTriggerWord(raw: string): string | null {
  if (raw === '') return '触发词不能为空';
  const points = Array.from(raw);
  if (points.length > TRIGGER_MAX_CODE_POINTS) {
    return `触发词不能超过 ${TRIGGER_MAX_CODE_POINTS} 个字符`;
  }
  if (points.some(isECMAScriptSpace)) return '触发词不能包含空白字符';
  return null;
}

function validateHotkeyDraft(raw: string): { canonical: string } | { error: string } {
  const canonical = normalizeHotkey(raw);
  if (canonical === null) return { error: '热键格式无效' };
  const reason = validateCanonicalHotkey(canonical);
  if (reason) return { error: reason };
  return { canonical };
}

export function PaletteConfigPanel({
  config,
  loadState,
  loadError = '',
}: {
  config: PaletteConfig;
  loadState: PaletteConfigLoadState;
  loadError?: string;
}) {
  const source = loadState === 'error' ? DEFAULT_PALETTE_CONFIG : config;
  const [hotkey, setHotkey] = useState(source.hotkey);
  const [triggerWord, setTriggerWord] = useState(source.triggerWord);
  const [matchMode, setMatchMode] = useState<PaletteMatchMode>(source.matchMode);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');

  useEffect(() => {
    const next = loadState === 'error' ? DEFAULT_PALETTE_CONFIG : config;
    setHotkey(next.hotkey);
    setTriggerWord(next.triggerWord);
    setMatchMode(next.matchMode);
  }, [config, loadState]);

  const hotkeyPreview = validateHotkeyDraft(hotkey);
  const preview = 'canonical' in hotkeyPreview ? formatHotkey(hotkeyPreview.canonical) : '';
  const loading = loadState === 'loading';

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    if (loading || saving) return;
    setError('');
    setNotice('');

    const triggerErr = validateTriggerWord(triggerWord);
    if (triggerErr) {
      setError(triggerErr);
      return;
    }
    const hotkeyResult = validateHotkeyDraft(hotkey);
    if ('error' in hotkeyResult) {
      setError(hotkeyResult.error);
      return;
    }

    setSaving(true);
    try {
      const saved = await api.putPaletteConfig({
        hotkey: hotkeyResult.canonical,
        triggerWord,
        matchMode,
      });
      setHotkey(saved.hotkey);
      setTriggerWord(saved.triggerWord);
      setMatchMode(saved.matchMode);
      setNotice('保存成功，已即时生效');
      window.dispatchEvent(new CustomEvent(PALETTE_CONFIG_CHANGED_EVENT, { detail: saved }));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '保存失败');
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="od-card">
      <div className="od-card-head">
        <h2>命令面板</h2>
        <span className="muted" style={{ fontSize: '12.5px' }}>
          保存即时生效
        </span>
      </div>

      {loadState === 'error' && (
        <div className="od-alert od-alert-danger" style={{ marginBottom: 14 }}>
          <div className="od-alert-body">
            加载命令面板配置失败{loadError ? `：${loadError}` : ''}
            <br />
            已使用默认配置渲染，保存合法配置后即时生效。
          </div>
        </div>
      )}
      {error && (
        <div className="od-alert od-alert-danger" style={{ marginBottom: 14 }}>
          <div className="od-alert-body">{error}</div>
        </div>
      )}
      {notice && (
        <div className="od-alert od-alert-info" style={{ marginBottom: 14 }}>
          <div className="od-alert-body">{notice}</div>
        </div>
      )}

      <form onSubmit={(e) => void save(e)}>
        <div className="od-field">
          <label className="od-label" htmlFor="palette-hotkey">
            唤起热键
          </label>
          <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
            <input
              className="od-input mono"
              id="palette-hotkey"
              spellCheck={false}
              style={{ maxWidth: 280 }}
              value={hotkey}
              onChange={(e) => setHotkey(e.target.value)}
            />
            <span className="muted mono" style={{ fontSize: '12.5px' }}>
              {preview}
            </span>
          </div>
          <div className="od-hint">输入规范串（如 mod+k）或自由组合；保存时规范化。默认展示 ⌘K / Ctrl+K</div>
        </div>

        <div className="od-field">
          <label className="od-label" htmlFor="palette-trigger">
            快速新建触发词
          </label>
          <input
            className="od-input mono"
            id="palette-trigger"
            spellCheck={false}
            style={{ maxWidth: 280 }}
            value={triggerWord}
            onChange={(e) => setTriggerWord(e.target.value)}
          />
          <div className="od-hint">命令面板输入「触发词 + 空格 + 项目名」进入快速新建；不含空白，最多 32 个字符</div>
        </div>

        <div className="od-field">
          <span className="od-label">项目名匹配模式</span>
          {MATCH_MODE_ITEMS.map((item) => (
            <label key={item.value} style={{ display: 'block', margin: '4px 0' }}>
              <input
                type="radio"
                name="palette-match-mode"
                value={item.value}
                checked={matchMode === item.value}
                onChange={() => setMatchMode(item.value)}
              />{' '}
              {item.label}
              <span className="muted" style={{ fontSize: '12px', marginLeft: 6 }}>
                {item.desc}
              </span>
            </label>
          ))}
          <div className="od-hint">仅影响指挥中心自动预选；候选列表排序不受此选项影响</div>
        </div>

        <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: 6 }}>
          <button className="od-btn od-btn-primary" type="submit" disabled={saving || loading}>
            {saving ? '保存中…' : '保存'}
          </button>
        </div>
      </form>
    </section>
  );
}

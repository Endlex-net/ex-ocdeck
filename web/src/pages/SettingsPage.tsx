import { useEffect, useRef, useState } from 'react';
import { api, ApiError } from '../api';
import {
  EDITOR_TOOLS_CHANGED,
  isUsableCustomTool,
  loadCustomEditorTools,
  loadEditorTools,
  loadEditorUriTemplate,
  saveCustomEditorTools,
  saveEditorTool,
  saveEditorUriTemplate,
  type CustomEditorTool,
  type EditorTool,
} from '../editor-tools';
import { GlobalEnvEditor } from '../components/GlobalEnvEditor';
import { SystemEnvPanel } from '../components/SystemEnvPanel';
import { NotificationConfigPanel } from '../components/NotificationConfigPanel';
import { PaletteConfigPanel, type PaletteConfigLoadState } from '../components/PaletteConfigPanel';
import { TermAppearanceEditor } from '../components/TermAppearanceEditor';
import type { PaletteConfig } from '../palette-focus';
import { navigate, type ConfigsTab } from '../router';
import type { MobileCaps, MobileMode } from '../terminal/mobile-mode';
import {
  loadClipboardPolicy,
  saveClipboardPolicy,
  type ClipboardPolicy,
} from '../terminal/clipboard';
import {
  loadMobileCaps,
  loadMobileMode,
  saveMobileCaps,
  saveMobileMode,
  TERM_PREFS_CHANGED,
} from '../terminal/preferences';
import { useTheme, type ThemePreference } from '../hooks';
import type { OcConfigContent, OcConfigInfo } from '../types';
import './settings.css';

/* ============================ 设置多合一（tasks.md 7.1-7.5 + task-notifications D12） ============================
 * 终端外观 / 环境变量 / opencode 配置 / AI 配置 / 通知 / 命令面板 合并为单页子标签。
 * 深链恢复：resolveRoute 已将 #/configs#<tab> 的 fragment 归一为合法 ConfigsTab（未知回 appearance）。
 * 子标签切换更新 hash（replace 模式不污染历史）。
 * 终端外观 / 环境变量复用既有编辑器组件；opencode / AI 逻辑已迁入本页（旧 ConfigsPage/AIConfigPage 已删）。 */

const TABS: { key: ConfigsTab; label: string; id: string; panel: string }[] = [
  { key: 'appearance', label: '终端外观', id: 'tab-appearance', panel: 'panel-appearance' },
  { key: 'env', label: '环境变量', id: 'tab-env', panel: 'panel-env' },
  { key: 'opencode', label: 'opencode 配置', id: 'tab-opencode', panel: 'panel-opencode' },
  { key: 'ai', label: 'AI 配置', id: 'tab-ai', panel: 'panel-ai' },
  { key: 'notifications', label: '通知', id: 'tab-notifications', panel: 'panel-notifications' },
  { key: 'palette', label: '命令面板', id: 'tab-palette', panel: 'panel-palette' },
  { key: 'tools', label: '常用工具', id: 'tab-tools', panel: 'panel-tools' },
];

const THEME_OPTIONS: { value: ThemePreference; label: string }[] = [
  { value: 'system', label: '跟随系统' },
  { value: 'light', label: '浅色' },
  { value: 'dark', label: '深色' },
];

export function SettingsPage({
  tab,
  paletteConfig,
  paletteLoadState,
  paletteLoadError,
}: {
  tab: ConfigsTab;
  paletteConfig: PaletteConfig;
  paletteLoadState: PaletteConfigLoadState;
  paletteLoadError: string;
}) {
  const tabRefs = useRef<Record<string, HTMLButtonElement | null>>({});
  // 环境变量 tab 共享 reload 信号（host-env-sync-and-display D5）：SystemEnvPanel 添加/刷新
  // 成功后递增，驱动 GlobalEnvEditor 重载（resolvedValue 可能变化）；SystemEnvPanel 同步已配置 key 集。
  const [envReloadTick, setEnvReloadTick] = useState(0);

  const selectTab = (key: ConfigsTab) => {
    // replace 模式：不污染返回栈（design.md D3 子标签切换不新增历史项）。
    navigate(`/configs#${key}`, true);
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
    const idx = TABS.findIndex((t) => t.key === tab);
    if (idx < 0) return;
    e.preventDefault();
    const next = TABS[(idx + (e.key === 'ArrowRight' ? 1 : TABS.length - 1)) % TABS.length];
    tabRefs.current[next.key]?.focus();
    selectTab(next.key);
  };

  return (
    <>
      <header className="od-page-head">
        <div className="od-page-title">
          <h1>设置</h1>
          <p className="muted" style={{ fontSize: '13px' }}>
            终端外观 · 全局环境变量 · opencode 配置 · AI provider
          </p>
        </div>
      </header>

      <div className="set-tabs" role="tablist" onKeyDown={onKeyDown}>
        {TABS.map((t) => (
          <button
            key={t.key}
            ref={(el) => {
              tabRefs.current[t.key] = el;
            }}
            className="set-tab"
            role="tab"
            id={t.id}
            aria-controls={t.panel}
            aria-selected={tab === t.key}
            onClick={() => selectTab(t.key)}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'appearance' && (
        <div role="tabpanel" id="panel-appearance" aria-labelledby="tab-appearance">
          <AppearancePanel />
        </div>
      )}
      {tab === 'env' && (
        <div role="tabpanel" id="panel-env" aria-labelledby="tab-env">
          <section className="od-card">
            <div className="od-card-head">
              <h2>全局环境变量</h2>
            </div>
            <GlobalEnvEditor
              reloadTick={envReloadTick}
              onChanged={() => setEnvReloadTick((t) => t + 1)}
            />
          </section>
          <section className="od-card">
            <div className="od-card-head">
              <h2>系统环境变量</h2>
              <span className="muted" style={{ fontSize: '12.5px' }}>
                服务端读到的宿主环境（进程环境 ∪ login shell）
              </span>
            </div>
            <SystemEnvPanel
              reloadTick={envReloadTick}
              onGlobalChanged={() => setEnvReloadTick((t) => t + 1)}
            />
          </section>
        </div>
      )}
      {tab === 'opencode' && (
        <div role="tabpanel" id="panel-opencode" aria-labelledby="tab-opencode">
          <OcConfigPanel />
        </div>
      )}
      {tab === 'ai' && (
        <div role="tabpanel" id="panel-ai" aria-labelledby="tab-ai">
          <AIConfigPanel />
        </div>
      )}
      {tab === 'notifications' && (
        <div role="tabpanel" id="panel-notifications" aria-labelledby="tab-notifications">
          <NotificationConfigPanel />
        </div>
      )}
      {tab === 'palette' && (
        <div role="tabpanel" id="panel-palette" aria-labelledby="tab-palette">
          <PaletteConfigPanel
            config={paletteConfig}
            loadState={paletteLoadState}
            loadError={paletteLoadError}
          />
        </div>
      )}
      {tab === 'tools' && (
        <div role="tabpanel" id="panel-tools" aria-labelledby="tab-tools">
          <EditorToolsPanel />
        </div>
      )}
    </>
  );
}

/* ============================ 终端外观子标签 ============================ */
const MOBILE_MODE_OPTIONS: { value: MobileMode; label: string }[] = [
  { value: 'auto', label: '自动' },
  { value: 'on', label: '开启' },
  { value: 'off', label: '关闭' },
];

const MOBILE_MODE_HINT: Record<MobileMode, string> = {
  auto: '触屏设备自动启用锁定/手势，键盘避让保持默认',
  on: '所有设备按下方子开关强制启用',
  off: '完全按桌面终端处理（含不收缩避让键盘）',
};

const CLIPBOARD_POLICY_OPTIONS: { value: ClipboardPolicy; label: string }[] = [
  { value: 'ask', label: '询问' },
  { value: 'auto', label: '自动允许' },
  { value: 'off', label: '关闭' },
];

const CLIPBOARD_POLICY_HINT: Record<ClipboardPolicy, string> = {
  ask: '远程终端复制时先弹窗确认，再写入本机剪贴板',
  auto: '远程终端复制直接写入本机剪贴板',
  off: '忽略远程终端的复制请求',
};

/** 远程剪贴板策略（ask/auto/off）：授权是源级持久的，必须在设置里可撤销。 */
function ClipboardPolicyField() {
  const [policy, setPolicy] = useState<ClipboardPolicy>(() => loadClipboardPolicy());

  // 外部变更（终端「始终允许」、跨标签页）后按实际存储收敛。
  useEffect(() => {
    const reload = () => setPolicy(loadClipboardPolicy());
    window.addEventListener(TERM_PREFS_CHANGED, reload);
    window.addEventListener('storage', reload);
    return () => {
      window.removeEventListener(TERM_PREFS_CHANGED, reload);
      window.removeEventListener('storage', reload);
    };
  }, []);

  const selectPolicy = (p: ClipboardPolicy) => {
    try {
      saveClipboardPolicy(p);
    } catch {
      return;
    }
    setPolicy(p);
  };

  return (
    <div className="od-field">
      <span className="od-label" id="clipPolicySegLabel">
        远程剪贴板
      </span>
      <div
        className="seg"
        role="group"
        aria-labelledby="clipPolicySegLabel"
      >
        {CLIPBOARD_POLICY_OPTIONS.map((opt) => (
          <button
            key={opt.value}
            type="button"
            className={policy === opt.value ? 'on' : ''}
            aria-pressed={policy === opt.value}
            onClick={() => selectPolicy(opt.value)}
          >
            {opt.label}
          </button>
        ))}
      </div>
      <div className="od-hint">{CLIPBOARD_POLICY_HINT[policy]}；对本设备的所有终端生效</div>
    </div>
  );
}

function AppearancePanel() {
  const { preference, setPreference } = useTheme();

  // 判别式加载（spec「自动模式不展示不读取子开关」）：仅 mode 为 on 时读 caps key。
  const [mobile, setMobile] = useState<{ mode: MobileMode; caps: MobileCaps | null }>(() => {
    const mode = loadMobileMode();
    return { mode, caps: mode === 'on' ? loadMobileCaps() : null };
  });
  const [mobileError, setMobileError] = useState('');

  // 外部变更（恢复默认等）后按实际存储判别式重载收敛。
  useEffect(() => {
    const reload = () => {
      const mode = loadMobileMode();
      setMobile({ mode, caps: mode === 'on' ? loadMobileCaps() : null });
    };
    window.addEventListener(TERM_PREFS_CHANGED, reload);
    return () => window.removeEventListener(TERM_PREFS_CHANGED, reload);
  }, []);

  const selectMobileMode = (mode: MobileMode) => {
    setMobileError('');
    try {
      saveMobileMode(mode);
    } catch (err) {
      setMobileError(err instanceof Error ? err.message : '保存失败');
      return;
    }
    setMobile({ mode, caps: mode === 'on' ? loadMobileCaps() : null });
  };

  // 子开关统一走一次完整 caps JSON 写入；锁定开 → 手势同事务强制开（避免只能看不能滚）。
  const setMobileCap = (patch: Partial<MobileCaps>) => {
    const caps = mobile.caps;
    if (!caps) return;
    const next: MobileCaps = { ...caps, ...patch };
    if (next.lock) next.gestures = true;
    setMobileError('');
    try {
      saveMobileCaps(next);
    } catch (err) {
      setMobileError(err instanceof Error ? err.message : '保存失败');
      return;
    }
    setMobile({ mode: mobile.mode, caps: next });
  };

  return (
    <section className="od-card">
      <div className="od-card-head">
        <h2>终端外观</h2>
      </div>
      <div className="od-field">
        <span className="od-label" id="themeSegLabel">
          界面主题
        </span>
        <div
          className="seg"
          role="group"
          aria-labelledby="themeSegLabel"
        >
          {THEME_OPTIONS.map((opt) => (
            <button
              key={opt.value}
              type="button"
              className={preference === opt.value ? 'on' : ''}
              aria-pressed={preference === opt.value}
              onClick={() => setPreference(opt.value)}
            >
              {opt.label}
            </button>
          ))}
        </div>
        <div className="od-hint">默认跟随系统；终端配色随主题翻转（亮色主题 = 浅色终端）</div>
      </div>
      <div className="od-field">
        <span className="od-label" id="mobileModeSegLabel">
          移动端模式
        </span>
        <div
          className="seg"
          role="group"
          aria-labelledby="mobileModeSegLabel"
        >
          {MOBILE_MODE_OPTIONS.map((opt) => (
            <button
              key={opt.value}
              type="button"
              className={mobile.mode === opt.value ? 'on' : ''}
              aria-pressed={mobile.mode === opt.value}
              onClick={() => selectMobileMode(opt.value)}
            >
              {opt.label}
            </button>
          ))}
        </div>
        <div className="od-hint">{MOBILE_MODE_HINT[mobile.mode]}</div>
        {mobile.mode === 'on' && mobile.caps && (
          <>
            <label htmlFor="mobile-cap-lock" style={{ display: 'block', margin: '4px 0' }}>
              <input
                type="checkbox"
                id="mobile-cap-lock"
                checked={mobile.caps.lock}
                onChange={(e) => setMobileCap({ lock: e.target.checked })}
              />{' '}
              终端锁定
            </label>
            <div className="od-hint">接外接键盘时关闭可避免锁定遮挡输入</div>
            <label htmlFor="mobile-cap-gestures" style={{ display: 'block', margin: '4px 0' }}>
              <input
                type="checkbox"
                id="mobile-cap-gestures"
                checked={mobile.caps.lock || mobile.caps.gestures}
                disabled={mobile.caps.lock}
                onChange={(e) => setMobileCap({ gestures: e.target.checked })}
              />{' '}
              触控手势
            </label>
            {mobile.caps.lock && (
              <div className="od-hint">锁定开启时手势保持开启，否则终端无法滚动</div>
            )}
            <label htmlFor="mobile-cap-avoid" style={{ display: 'block', margin: '4px 0' }}>
              <input
                type="checkbox"
                id="mobile-cap-avoid"
                checked={mobile.caps.keyboardAvoid}
                onChange={(e) => setMobileCap({ keyboardAvoid: e.target.checked })}
              />{' '}
              键盘避让
            </label>
            <div className="od-hint">虚拟键盘弹出时收缩终端视口避免遮挡</div>
          </>
        )}
        {mobileError && <div className="error-line">{mobileError}</div>}
      </div>
      <ClipboardPolicyField />
      <TermAppearanceEditor />
    </section>
  );
}

/* ============================ 常用工具子标签（add-frontend-tool-quick-open 2.3） ============================
 * VSCode/GoLand/Cursor 本机编辑器启用开关（缺省关闭，仅持久化于 localStorage）：
 * 开启后任务详情页页头出现对应快捷打开入口。变更走 saveEditorTool（事务语义），
 * 面板监听 EDITOR_TOOLS_CHANGED + storage 收敛外部变更（同 ClipboardPolicyField 模式）。
 * 自定义工具（名称 + 唤起 URI 模板）：行内输入失焦保存整表，删除/添加即时持久化。 */
const EDITOR_TOOLS: { tool: EditorTool; label: string; hint: string }[] = [
  {
    tool: 'vscode',
    label: 'VSCode',
    hint: '在任务详情页页头显示 VSCode 快捷打开；需本机已安装 VSCode，使用本机路径打开',
  },
  {
    tool: 'goland',
    label: 'GoLand',
    hint: '在任务详情页页头显示 GoLand 快捷打开；需本机已安装 GoLand，使用本机路径打开',
  },
  {
    tool: 'cursor',
    label: 'Cursor',
    hint: '在任务详情页页头显示 Cursor 快捷打开；需本机已安装 Cursor，使用本机路径打开',
  },
];

/** 各工具内置默认模板（placeholder 展示；模板含 {path} 占位符，缺省/清空即走内置分支）。 */
const EDITOR_TOOL_DEFAULT_TEMPLATES: Record<EditorTool, string> = {
  vscode: 'vscode://file{path}/',
  goland: 'goland://open?file={path}',
  cursor: 'cursor://file{path}/',
};

/** 模板能力说明（spec「自定义唤起 URI 模板」末段原文）。 */
const EDITOR_TOOL_TEMPLATE_HINT =
  'VSCode 可配置 vscode://vscode-remote/ssh-remote+<主机>{path} 形式的模板打开远程开发机目录' +
  '（需本机已安装 Remote-SSH 扩展且 SSH 可达；该形式为社区实测、非官方文档化）；' +
  'GoLand 不存在通过 URL 打开远程项目的可用形式。';

/** 新增自定义工具行 id（时间戳 + 随机后缀，页面内唯一即可）。 */
function newCustomToolId(): string {
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
}

function EditorToolsPanel() {
  const [tools, setTools] = useState(() => loadEditorTools());
  const loadTemplates = (): Record<EditorTool, string> => ({
    vscode: loadEditorUriTemplate('vscode') ?? '',
    goland: loadEditorUriTemplate('goland') ?? '',
    cursor: loadEditorUriTemplate('cursor') ?? '',
  });
  const [templates, setTemplates] = useState(loadTemplates);
  // 最近一次同步到的存储快照：事件只收敛「草稿未被本地编辑」的字段，未失焦草稿不丢（评审 F1）。
  const storedRef = useRef(templates);

  // 自定义工具：草稿列表 + 存储快照（行内失焦保存整表；跨标签/入口下拉变更经事件收敛）。
  // 初始渲染时草稿与存储一致（同源加载）。
  const [customDrafts, setCustomDrafts] = useState<CustomEditorTool[]>(() => loadCustomEditorTools());
  const customStoredRef = useRef(customDrafts);

  // 外部变更（快捷入口下拉、跨标签页）后按实际存储收敛。
  useEffect(() => {
    const reload = () => {
      setTools(loadEditorTools());
      const prevStored = storedRef.current;
      const next = loadTemplates();
      // updater 惰性执行且必须纯净：与之比较的旧存储快照先捕获到局部变量（不读可变 ref）。
      setTemplates((draft) => ({
        vscode: draft.vscode === prevStored.vscode ? next.vscode : draft.vscode,
        goland: draft.goland === prevStored.goland ? next.goland : draft.goland,
        cursor: draft.cursor === prevStored.cursor ? next.cursor : draft.cursor,
      }));
      storedRef.current = next;
      // 自定义工具：逐字段收敛（草稿字段 === 上一存储快照值才收敛，未失焦编辑不丢）。
      // 旧存储快照里有而新存储没有的行 = 外部已删除：仅当本地草稿相对快照无修改时随之移除
      // （本地脏草稿独立保留，防误删正在编辑的内容）；从未入存的本地新增行保留。
      // 与 templates 相同：快照更新在 updater 之外完成。
      const prevCustom = customStoredRef.current;
      const nextCustom = loadCustomEditorTools();
      setCustomDrafts((drafts) => {
        const merged: CustomEditorTool[] = nextCustom.map((t) => {
          const prevRow = prevCustom.find((p) => p.id === t.id);
          const draftRow = drafts.find((d) => d.id === t.id);
          if (!prevRow || !draftRow) return t;
          return {
            id: t.id,
            name: draftRow.name === prevRow.name ? t.name : draftRow.name,
            template: draftRow.template === prevRow.template ? t.template : draftRow.template,
          };
        });
        for (const d of drafts) {
          if (nextCustom.some((t) => t.id === d.id)) continue;
          const prevRow = prevCustom.find((p) => p.id === d.id);
          if (prevRow && d.name === prevRow.name && d.template === prevRow.template) {
            continue; // 外部已删除且本地无修改 → 随之移除（不复活）
          }
          merged.push(d); // 本地脏草稿（外部删除时正在编辑）或未入存的本地新增行
        }
        return merged;
      });
      customStoredRef.current = nextCustom;
    };
    window.addEventListener(EDITOR_TOOLS_CHANGED, reload);
    window.addEventListener('storage', reload);
    return () => {
      window.removeEventListener(EDITOR_TOOLS_CHANGED, reload);
      window.removeEventListener('storage', reload);
    };
  }, []);

  // 写失败（向上抛出）不视为生效：UI 不翻转，存储不变。
  const setTool = (tool: EditorTool, enabled: boolean) => {
    try {
      saveEditorTool(tool, enabled);
    } catch {
      return;
    }
    setTools(loadEditorTools());
  };

  // 模板失焦保存：非空写原文、空值清除恢复内置；按实际已存值收敛该字段（写失败时回收为旧值，不视为生效）。
  const saveTemplate = (tool: EditorTool, value: string) => {
    try {
      saveEditorUriTemplate(tool, value);
    } catch {
      /* 写失败不派发事件，输入框收敛回已存值 */
    }
    const stored = loadEditorUriTemplate(tool) ?? '';
    storedRef.current = { ...storedRef.current, [tool]: stored };
    setTemplates((t) => ({ ...t, [tool]: stored }));
  };

  // 自定义工具草稿收敛：外部已删除但本地脏（正在编辑未失焦）的草稿行独立保留（C3），
  // 其余收敛为存储全表。updater 纯函数、幂等。
  const convergeCustomDrafts = (stored: CustomEditorTool[]) => {
    setCustomDrafts((drafts) => {
      const deletedDirty = drafts.filter((d) => !stored.some((t) => t.id === d.id));
      if (deletedDirty.length === 0) return stored;
      return [...stored, ...deletedDirty];
    });
    customStoredRef.current = stored;
  };

  // 自定义工具行失焦保存（C2）：以最新存储为基准应用本次行操作（不整表恢复草稿）。
  // 行已被外部删除（C3 脏草稿）：不写存储、不删除草稿——保留独立状态，由用户显式处置（放弃/另存）。
  // 写失败不视为生效：不派发事件，行内容收敛回已存值。
  const saveCustomRow = (id: string, patch: Partial<CustomEditorTool>) => {
    const stored = loadCustomEditorTools();
    const target = stored.find((t) => t.id === id);
    if (target) {
      const next = stored.map((t) => (t.id === id ? { ...t, ...patch } : t));
      try {
        saveCustomEditorTools(next);
      } catch {
        /* 写失败不派发事件，输入框收敛回已存值 */
      }
    }
    convergeCustomDrafts(loadCustomEditorTools());
  };

  // 添加：以最新存储为基准追加一条空行（占位草稿持久化，跨事件收敛不丢）；写失败不添加。
  const addCustomTool = () => {
    const next = [...loadCustomEditorTools(), { id: newCustomToolId(), name: '', template: '' }];
    try {
      saveCustomEditorTools(next);
    } catch {
      return;
    }
    convergeCustomDrafts(loadCustomEditorTools());
  };

  // 删除：以最新存储为基准移除并持久化；写失败不删除。
  const removeCustomTool = (id: string) => {
    const next = loadCustomEditorTools().filter((d) => d.id !== id);
    try {
      saveCustomEditorTools(next);
    } catch {
      return;
    }
    convergeCustomDrafts(loadCustomEditorTools());
  };

  // C3 出口一：放弃外部已删除行的草稿（存储中该行已不存在，仅移除本地草稿，无存储写入）。
  const discardCustomDraft = (id: string) => {
    setCustomDrafts((drafts) => drafts.filter((d) => d.id !== id));
  };

  // C3 出口二：草稿另存为新工具（新 id 追加到存储）；成功后原草稿行移除。
  const saveCustomDraftAsNew = (id: string) => {
    const draft = customDrafts.find((d) => d.id === id);
    if (!draft) return;
    const next = [...loadCustomEditorTools(), { ...draft, id: newCustomToolId() }];
    try {
      saveCustomEditorTools(next);
    } catch {
      return;
    }
    const stored = loadCustomEditorTools();
    customStoredRef.current = stored;
    setCustomDrafts((drafts) => {
      const storedIds = new Set(stored.map((t) => t.id));
      // 原草稿行移除；其余外部已删除脏草稿行保留
      const leftover = drafts.filter((d) => d.id !== id && !storedIds.has(d.id));
      return [...stored, ...leftover];
    });
  };

  return (
    <section className="od-card">
      <div className="od-card-head">
        <h2>常用工具</h2>
      </div>
      {EDITOR_TOOLS.map(({ tool, label, hint }) => (
        <div className="od-field" key={tool}>
          <label htmlFor={`editor-tool-${tool}`} style={{ display: 'block', margin: '4px 0' }}>
            <input
              type="checkbox"
              id={`editor-tool-${tool}`}
              checked={tools[tool]}
              onChange={(e) => setTool(tool, e.target.checked)}
            />{' '}
            在任务详情页显示「{label}」快捷打开
          </label>
          <div className="od-hint">{hint}</div>
          <label
            htmlFor={`editor-tool-template-${tool}`}
            style={{ display: 'block', margin: '10px 0 4px' }}
          >
            自定义唤起 URI 模板（可选）
          </label>
          <input
            className="od-input mono"
            id={`editor-tool-template-${tool}`}
            type="text"
            spellCheck={false}
            style={{ maxWidth: 420 }}
            placeholder={EDITOR_TOOL_DEFAULT_TEMPLATES[tool]}
            value={templates[tool]}
            onChange={(e) => setTemplates((t) => ({ ...t, [tool]: e.target.value }))}
            onBlur={(e) => saveTemplate(tool, e.target.value)}
          />
        </div>
      ))}
      <div className="od-hint">{EDITOR_TOOL_TEMPLATE_HINT}</div>
      <div className="od-field">
        <div style={{ display: 'block', margin: '10px 0 4px' }}>自定义工具</div>
        <div className="od-hint">
          通过自定义 URL scheme 唤起其他编辑器/工具；名称与模板齐全才出现在快捷打开列表
          （模板需含 {'{path}'}，scheme 不得为 javascript:/data:/vbscript:/file:）
        </div>
        {customDrafts.map((t) => {
          const usable = isUsableCustomTool(t);
          // C3：该草稿行已不在存储中 = 其他标签页删除了正在编辑的工具（脏草稿独立保留）
          const externallyDeleted = !customStoredRef.current.some((s) => s.id === t.id);
          return (
            <div key={t.id} style={{ margin: '8px 0' }}>
              {externallyDeleted && (
                <div className="od-hint">该工具已在其他标签页被删除；此处草稿已保留，可选择放弃或另存为新工具。</div>
              )}
              <label
                htmlFor={`custom-tool-name-${t.id}`}
                style={{ display: 'block', margin: '4px 0' }}
              >
                名称
              </label>
              <input
                className="od-input"
                id={`custom-tool-name-${t.id}`}
                type="text"
                style={{ maxWidth: 420 }}
                value={t.name}
                onChange={(e) => setCustomDrafts((d) => d.map((x) => (x.id === t.id ? { ...x, name: e.target.value } : x)))}
                onBlur={(e) => saveCustomRow(t.id, { name: e.target.value })}
              />
              <label
                htmlFor={`custom-tool-template-${t.id}`}
                style={{ display: 'block', margin: '8px 0 4px' }}
              >
                唤起 URI 模板
              </label>
              <input
                className="od-input mono"
                id={`custom-tool-template-${t.id}`}
                type="text"
                spellCheck={false}
                style={{ maxWidth: 420 }}
                placeholder="myeditor://open?path={path}"
                value={t.template}
                onChange={(e) =>
                  setCustomDrafts((d) => d.map((x) => (x.id === t.id ? { ...x, template: e.target.value } : x)))
                }
                onBlur={(e) => saveCustomRow(t.id, { template: e.target.value })}
              />{' '}
              <button type="button" className="btn btn-small" onClick={() => removeCustomTool(t.id)}>
                删除
              </button>
              {!usable && (
                <div className="od-hint">
                  名称或模板不完整：该工具暂不出现在快捷打开列表（草稿保留，可继续编辑）
                </div>
              )}
              {externallyDeleted && (
                <div style={{ margin: '4px 0' }}>
                  <button type="button" className="btn btn-small" onClick={() => discardCustomDraft(t.id)}>
                    放弃草稿
                  </button>{' '}
                  <button type="button" className="btn btn-small" onClick={() => saveCustomDraftAsNew(t.id)}>
                    另存为新工具
                  </button>
                </div>
              )}
            </div>
          );
        })}
        <button type="button" className="btn btn-small" onClick={addCustomTool}>
          添加自定义工具
        </button>
      </div>
    </section>
  );
}

/* ============================ opencode 配置子标签 ============================
 * 从 ConfigsPage 迁移：文件列表 + 编辑器 + mtime 显示 + 未保存标记 + 乐观锁 409 冲突模态。 */
function OcConfigPanel() {
  const [configs, setConfigs] = useState<OcConfigInfo[]>([]);
  const [listError, setListError] = useState('');
  const [loaded, setLoaded] = useState(false);

  const [current, setCurrent] = useState<OcConfigContent | null>(null);
  const [draft, setDraft] = useState('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [conflict, setConflict] = useState(false);

  useEffect(() => {
    api
      .listOcConfigs()
      .then((res) => {
        setConfigs(res.configs ?? []);
        setListError('');
      })
      .catch((err) =>
        setListError(err instanceof ApiError ? err.message : '加载配置列表失败'),
      )
      .finally(() => setLoaded(true));
  }, []);

  const open = async (name: string) => {
    setError('');
    setNotice('');
    setConflict(false);
    try {
      const c = await api.getOcConfig(name);
      setCurrent(c);
      setDraft(c.content);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '加载配置失败');
    }
  };

  /** 用给定的基线版本执行 PUT；返回 true 表示成功。 */
  const doSave = async (base: OcConfigContent, content: string): Promise<boolean> => {
    setSaving(true);
    setError('');
    setNotice('');
    try {
      const res = await api.putOcConfig(base.name, content, base.mtime, base.hash);
      setCurrent({ ...base, content, mtime: res.mtime, hash: res.hash });
      setDraft(content);
      setConflict(false);
      const n = res.affectedActiveTasks?.length ?? 0;
      setNotice(
        n > 0
          ? `已保存。${n} 个活跃任务受影响，需重启任务（挂起后激活）生效。`
          : '已保存。当前没有活跃任务使用该配置。',
      );
      return true;
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        setConflict(true);
      } else {
        setError(err instanceof ApiError ? `[${err.code}] ${err.message}` : '保存失败');
      }
      return false;
    } finally {
      setSaving(false);
    }
  };

  const save = () => {
    if (current) void doSave(current, draft);
  };

  /** 409 → 覆盖保存：重新 GET 最新 mtime/hash，用编辑器内容重放 PUT。 */
  const overwrite = async () => {
    if (!current) return;
    setConflict(false);
    try {
      const latest = await api.getOcConfig(current.name);
      await doSave(latest, draft);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '重新加载失败');
    }
  };

  /** 409 → 放弃本地编辑，重新加载服务端版本。 */
  const discard = async () => {
    if (!current) return;
    setConflict(false);
    setError('');
    try {
      const c = await api.getOcConfig(current.name);
      setCurrent(c);
      setDraft(c.content);
      setNotice('已重新加载服务端最新内容。');
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '重新加载失败');
    }
  };

  const dirty = current !== null && draft !== current.content;

  return (
    <section className="od-card" style={{ padding: 0, overflow: 'hidden' }}>
      <div className="od-card-head" style={{ padding: '18px 20px 0' }}>
        <h2>opencode 配置文件</h2>
        {/* 保存入口收敛在编辑器头部（文件名/mtime/未保存标记旁），此处不再重复 */}
      </div>

      {listError && (
        <div className="od-alert od-alert-danger" style={{ margin: '12px 16px 0' }}>
          <div className="od-alert-body">{listError}</div>
        </div>
      )}

      <div className="cfg-split">
        <div className="cfg-files" role="listbox" aria-label="配置文件列表">
          {loaded && configs.length === 0 && (
            <div className="od-empty">暂无配置文件。</div>
          )}
          {configs.map((c) => (
            <button
              key={c.name}
              className={`cfg-file ${current?.name === c.name ? 'on' : ''}`}
              role="option"
              aria-selected={current?.name === c.name}
              onClick={() => void open(c.name)}
            >
              {c.name}
            </button>
          ))}
        </div>

        <div className="cfg-editor">
          {!current && <div className="od-empty">选择左侧配置文件开始编辑。</div>}
          {current && (
            <>
              <div className="cfg-editor-head">
                <span className="mono">{current.name}</span>
                <span className="muted mono" style={{ fontSize: '12px' }}>
                  mtime {current.mtime}
                </span>
                {dirty && <span className="cfg-dirty">未保存</span>}
                <span style={{ flex: 1 }} />
                <button
                  className="od-btn od-btn-primary od-btn-sm"
                  disabled={saving || !dirty}
                  onClick={save}
                >
                  {saving ? '保存中…' : '保存'}
                </button>
              </div>
              {notice && <div className="od-hint" style={{ marginBottom: 8 }}>{notice}</div>}
              {error && (
                <div className="od-alert od-alert-danger" style={{ marginBottom: 8 }}>
                  <div className="od-alert-body">{error}</div>
                </div>
              )}
              <textarea
                className="od-textarea mono"
                spellCheck={false}
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                rows={14}
              />
              <p className="od-hint" style={{ marginTop: 8 }}>
                保存（PUT 整体替换），乐观锁冲突时需确认覆盖或重载。改动需重启任务（挂起后激活）生效。
              </p>
            </>
          )}
        </div>
      </div>

      {conflict && (
        <div className="od-modal-mask" onClick={() => setConflict(false)}>
          <div className="od-modal" role="dialog" aria-modal="true" onClick={(e) => e.stopPropagation()}>
            <h2>保存冲突</h2>
            <div className="od-modal-body">
              配置文件 <strong>{current?.name}</strong> 已被外部修改。
              <br />
              覆盖保存将以服务端最新版本为基线写入你的编辑；放弃则丢弃本地编辑并重新加载。
            </div>
            <div className="od-modal-actions">
              <button className="od-btn od-btn-ghost" onClick={() => setConflict(false)}>
                取消
              </button>
              <button className="od-btn" disabled={saving} onClick={() => void discard()}>
                放弃并重新加载
              </button>
              <button
                className="od-btn od-btn-danger"
                disabled={saving}
                onClick={() => void overwrite()}
              >
                覆盖保存
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}

/* ============================ AI 配置子标签 ============================
 * 从 AIConfigPage 迁移：provider 下拉 + 掩码 key + base_url + model + thinking 下拉 + 保存即生效 + load_error 引导。 */
const THINKING_HINT: Record<string, string> = {
  '': '默认：不下发参数，跟随模型/网关默认。',
  off: '关闭思考，响应最快。',
  low: '低强度思考，轻度延迟。',
  medium: '中强度思考，平衡速度与质量。',
  high: '思考强度越高响应越慢，「高」强度下分支命名可能超时并回退为自动生成。',
};

function AIConfigPanel() {
  const [loaded, setLoaded] = useState(false);
  const [configured, setConfigured] = useState(false);
  const [loadError, setLoadError] = useState('');

  const [provider, setProvider] = useState('openai');
  const [apiKey, setApiKey] = useState(''); // 仅保存用户新输入，绝不回显明文
  const [keyMasked, setKeyMasked] = useState('');
  const [baseURL, setBaseURL] = useState('');
  const [model, setModel] = useState('');
  const [thinking, setThinking] = useState('');

  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');

  const reload = () => {
    setLoaded(false);
    api
      .getAIConfig()
      .then((res) => {
        setConfigured(res.configured);
        setLoadError(res.load_error ?? '');
        setProvider(res.provider || 'openai');
        setBaseURL(res.base_url);
        setModel(res.model);
        setThinking(res.thinking ?? '');
        setKeyMasked(res.api_key_masked);
        setError('');
      })
      .catch((err) =>
        setError(err instanceof ApiError ? err.message : '加载配置失败'),
      )
      .finally(() => setLoaded(true));
  };

  useEffect(() => {
    reload();
  }, []);

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    setNotice('');
    if (model.trim() === '') {
      setError('模型不能为空');
      return;
    }
    if (!configured && apiKey.trim() === '') {
      setError('还没有保存过 API Key，请先填写');
      return;
    }
    setSaving(true);
    try {
      const res = await api.saveAIConfig({
        provider,
        api_key: apiKey, // 留空 = 保留原 key（服务端语义）
        base_url: baseURL.trim(),
        model: model.trim(),
        thinking,
      });
      setConfigured(res.configured);
      setLoadError(res.load_error ?? '');
      setKeyMasked(res.api_key_masked);
      setApiKey('');
      setNotice('保存成功，已即时生效');
    } catch (err) {
      // 422 invalid_input 等：message 即后端返回的原因
      setError(err instanceof ApiError ? err.message : '保存失败');
    } finally {
      setSaving(false);
    }
  };

  return (
    <>
      {loaded && !configured && !loadError && (
        <div className="od-alert od-alert-info" style={{ marginBottom: 14 }}>
          <div className="od-alert-body">尚未配置，AI 功能（如英文分支命名）当前处于降级状态。</div>
        </div>
      )}

      <section className="od-card">
        <div className="od-card-head">
          <h2>AI provider</h2>
          <span className="muted" style={{ fontSize: '12.5px' }}>
            保存即时生效
          </span>
        </div>

        {loadError && (
          <div className="od-alert od-alert-danger" style={{ marginBottom: 14 }}>
            <div className="od-alert-body">
              配置文件损坏或不可读：{loadError}
              <br />
              在下方重新填写并保存合法配置即可修复。
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

        <form onSubmit={save}>
          <div className="od-field">
            <label className="od-label" htmlFor="ai-provider">
              provider
            </label>
            <select
              className="od-select"
              id="ai-provider"
              style={{ maxWidth: 280 }}
              value={provider}
              onChange={(e) => setProvider(e.target.value)}
            >
              <option value="openai">openai</option>
              <option value="anthropic">anthropic</option>
            </select>
          </div>

          <div className="od-field">
            <label className="od-label" htmlFor="ai-apiKey">
              API Key
            </label>
            <input
              className="od-input mono"
              id="ai-apiKey"
              type="password"
              autoComplete="new-password"
              style={{ maxWidth: 420 }}
              placeholder={
                configured
                  ? keyMasked
                    ? `${keyMasked}（留空保持不变）`
                    : '留空保持不变'
                  : 'sk-...'
              }
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
            />
            {configured && keyMasked && (
              <div className="od-hint mono">当前已配置 {keyMasked}</div>
            )}
            <div className="od-hint">仅保存用户新输入，绝不回显明文；留空保持不变（服务端语义）</div>
          </div>

          <div className="od-field">
            <label className="od-label" htmlFor="ai-baseUrl">
              base URL
            </label>
            <input
              className="od-input mono"
              id="ai-baseUrl"
              spellCheck={false}
              style={{ maxWidth: 420 }}
              placeholder="留空使用默认端点"
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
            />
          </div>

          <div className="od-field">
            <label className="od-label" htmlFor="ai-model">
              模型
            </label>
            <input
              className="od-input mono"
              id="ai-model"
              spellCheck={false}
              style={{ maxWidth: 420 }}
              placeholder="如 gpt-4o-mini / claude-haiku-4-5"
              value={model}
              onChange={(e) => setModel(e.target.value)}
            />
          </div>

          <div className="od-field">
            <span className="od-label">思考强度</span>
            <select
              className="od-select"
              style={{ maxWidth: 280 }}
              value={thinking}
              onChange={(e) => setThinking(e.target.value)}
            >
              <option value="">默认</option>
              <option value="off">关闭</option>
              <option value="low">低</option>
              <option value="medium">中</option>
              <option value="high">高</option>
            </select>
            <div className="od-hint">{THINKING_HINT[thinking]}</div>
          </div>

          <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: 6 }}>
            <button className="od-btn od-btn-primary" type="submit" disabled={saving || !loaded}>
              {saving ? '保存中…' : '保存'}
            </button>
          </div>
        </form>
      </section>

      {loadError && (
        <details className="od-collapse" style={{ marginTop: 14 }}>
          <summary>
            <svg
              className="od-collapse-caret"
              width="12"
              height="12"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
            >
              <polyline points="9 5 16 12 9 19" />
            </svg>
            配置文件损坏或不可读？
          </summary>
          <div className="od-collapse-body">
            <p style={{ fontSize: '13.5px' }}>
              配置文件损坏或不可读：在上方重新填写并保存合法配置即可修复。
            </p>
            <div style={{ marginTop: 12 }}>
              <button
                className="od-btn od-btn-sm"
                disabled={!loaded}
                onClick={reload}
              >
                重试加载
              </button>
            </div>
          </div>
        </details>
      )}
    </>
  );
}
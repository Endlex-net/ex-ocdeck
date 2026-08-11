> VENDORED 设计源 · 固化于 2026-08-08（OpenSpec change: web-ui-redesign）
> 来源：Open Design 项目 2396c3bb-e795-41fb-9536-723f69c71734/brand-spec.md
> ⚠️ SUPERSEDED（2026-08-09，用户决策）：本文"终端固定深底"条款被取代——终端配色跟随应用主题（暗=深色终端、亮=浅色终端）。其余条款仍然有效；`--ink/--on-ink` 继续用于品牌标记、toast 等固定墨色组件。

# ocdeck 重设计 · 品牌契约（brand-spec）

一句话：亮壳驾驶舱 × 深色终端 —— 开发者工具的信息密度，配合清晰的层级与克制的强调色。

## 令牌（OKLch 与注册色值）

| Token | 值 | 用途 |
|---|---|---|
| `--bg` | `#ffffff` / `oklch(100% 0 0)` | 页面背景 |
| `--surface` | `#f7f8fa` / `oklch(97.5% 0.002 250)` | 卡片/面板底 |
| `--fg` | `#111111` / `oklch(21% 0.005 260)` | 主文字；同时作为终端面板深底 |
| `--muted` | `#6b7280` / `oklch(55% 0.02 260)` | 次级文字 |
| `--border` | `#d9dee7` / `oklch(88% 0.008 250)` | 1px 描边 |
| `--accent` | `#1677ff` / `oklch(57% 0.19 262)` | 唯一强调色（每屏 ≤2 处可见） |

派生色只允许用 `color-mix(in oklch, var(--x) …)` 从上述令牌派生；**禁止新增 hex 字面量**。
语义色（成功/警告/危险/信息）从 `--fg`/`--accent` 的 oklch 色相偏移派生并以用途命名（`--success` `--warn` `--danger` `--info`），不引入新色相字面量。

## 字体

- Display / Body / UI：`Inter, system-ui, -apple-system, "Segoe UI", "Helvetica Neue", Arial, sans-serif`（品牌锁定同族，用字重区分层级：400 正文 / 500 UI 标签 / 600 标题按钮）
- Mono（终端/代码/数据）：`ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas, monospace`

## 视觉语言规则

1. 8px 圆角、1px `--border` 描边、8px 基线网格；阴影克制（仅浮层用 0 4px 16px rgba 派生色）。
2. 终端/代码区域一律固定深底（`--ink` 派生）+ 白字，嵌在壳层中，构成产品唯一的「大胆一笔」；亮暗主题下均成立。
3. 状态即视觉语言：任务状态机（活跃/挂起/归档/过渡/失败）与 agent 脉冲点用一致的徽章体系呈现，过渡态必配 spinner。
4. 无渐变、无 emoji 功能图标（用 1.6px 描边单色 SVG，`currentColor`）、无装饰性色块。
5. 中文排版：标题行高 1.3–1.4，正文 1.7，字距 0；全大写拉丁标签加 0.06em 字距。

## 暗色主题（`[data-theme="dark"]`）

由 `assets/ocdeck-theme.js` 驱动：偏好存 `localStorage['od-theme']` ∈ `system | light | dark`，默认跟随系统，脚本在 `<head>` 内同步执行防闪烁。暗色令牌全部从注册六色板同色相派生（翻转明度轴），**不引入新 hex**：

| Token | 暗色值 | 说明 |
|---|---|---|
| `--bg` | `oklch(0.175 0.005 260)` | 深灰壳层（非纯黑，与 `--fg` 同色相） |
| `--surface` | `oklch(0.225 0.006 260)` | 卡片/面板底 |
| `--fg` | `oklch(0.92 0.005 260)` | 主文字（非纯白） |
| `--muted` | `oklch(0.68 0.015 250)` | 次级文字，对 `--bg` 对比度 ≥ 7:1 |
| `--border` | `oklch(0.92 0.005 260 / 0.14)` | 半透明白描边（暗色下优于实色深描边） |
| `--accent` | `oklch(0.68 0.19 258)` | 同色相提明度，保证深底对比度 |

固定墨色对 `--ink: #111111` / `--on-ink` 不随主题翻转，用于「永远是深色块」的组件：品牌标记、toast、分段控件选中态，以及终端面板（`--term-bg` 暗色下进一步压深至 `oklch(0.145 0.005 260)`，保持「壳层 × 更深的终端」层次）。派生层（`--accent-soft`、语义色 soft 变体、`--surface-hover` 等）基于 `color-mix` + `--bg`/`--fg`，随主题自动适配，无需逐条覆盖。

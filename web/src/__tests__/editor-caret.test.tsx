// @vitest-environment jsdom
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { EditorView } from '@codemirror/view';
import { editableExtensions, editorTheme } from '../components/editor/extensions';
import { mount, stubMatchMedia } from './cm-test-env';

/* ============================ 编辑器光标颜色（深色模式不可见修复） ============================
 * 根因：CM 基座主题 .cm-cursor 硬编码黑色 border-left，深色覆盖仅在其自带 dark 主题生效。
 * 修复：--editor-caret token（design-system.css 双主题）+ editorTheme 接管 .cm-cursor/caret-color。
 * 本测试从 token 文件解析真实色值并计算 WCAG 对比度（不硬编码期望色值）。 */

// vitest 以 web/ 为 cwd 运行；直接相对路径读 token 文件
const css = readFileSync('src/design-system.css', 'utf8');

/** 提取指定选择器块内的自定义属性值。 */
function tokenOf(blockSelector: string, name: string): string {
  const blockRe = new RegExp(
    `${blockSelector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\s*\\{([^}]*)\\}`,
  );
  const block = css.match(blockRe)?.[1] ?? '';
  const v = block.match(new RegExp(`${name}\\s*:\\s*([^;]+);`))?.[1]?.trim() ?? '';
  return v;
}

// ---------- OKLab → sRGB（标准矩阵，WCAG 相对亮度用） ----------
function oklchToSrgb(l: number, c: number, h: number): [number, number, number] {
  const hr = (h * Math.PI) / 180;
  const a = c * Math.cos(hr);
  const b = c * Math.sin(hr);
  const l_ = l + 0.3963377774 * a + 0.2158037573 * b;
  const m_ = l - 0.1055613458 * a - 0.0638541728 * b;
  const s_ = l - 0.0894841775 * a - 1.291485548 * b;
  const lc = l_ ** 3;
  const mc = m_ ** 3;
  const sc = s_ ** 3;
  const lin = (x: number) => x;
  const to255 = (x: number) =>
    Math.round(
      Math.max(
        0,
        Math.min(
          255,
          (x <= 0.0031308 ? 12.92 * x : 1.055 * Math.pow(x, 1 / 2.4) - 0.055) * 255,
        ),
      ),
    );
  return [
    to255(lin(4.0767416621 * lc - 3.3077115913 * mc + 0.2309699292 * sc)),
    to255(lin(-1.2684380046 * lc + 2.6097574011 * mc - 0.3413193965 * sc)),
    to255(lin(-0.0041960863 * lc - 0.7034186147 * mc + 1.707614701 * sc)),
  ];
}

/** 解析 token 值为 sRGB [r,g,b]（支持 #hex、oklch()、var() 一层解析）。 */
function resolveColor(blockSelector: string, name: string): [number, number, number] {
  let v = tokenOf(blockSelector, name);
  const varRef = v.match(/^var\((--[\w-]+)\)$/);
  if (varRef) v = tokenOf(blockSelector, varRef[1]);
  const hex = v.match(/^#([0-9a-f]{6})$/i);
  if (hex) {
    const n = parseInt(hex[1], 16);
    return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
  }
  const ok = v.match(/^oklch\(\s*([\d.]+)\s+([\d.]+)\s+([\d.]+)\s*\)$/);
  if (ok) return oklchToSrgb(Number(ok[1]), Number(ok[2]), Number(ok[3]));
  throw new Error(`无法解析色值：${name} = ${v}`);
}

/** WCAG 相对亮度与对比度。 */
function relLum([r, g, b]: [number, number, number]): number {
  const f = (x: number) => {
    const c = x / 255;
    return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
  };
  return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
}
function contrast(a: [number, number, number], b: [number, number, number]): number {
  const [la, lb] = [relLum(a), relLum(b)];
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

describe('--editor-caret token（design-system.css 双主题）', () => {
  it('深色主题：光标对深底对比度 ≥ 7:1（AAA，用户指定琥珀色 rgb(255,204,2)）', () => {
    const caret = resolveColor('[data-theme="dark"]', '--editor-caret');
    const bg = resolveColor('[data-theme="dark"]', '--bg');
    // 用户指定色值核对：token 解析结果应即 rgb(255,204,2)（oklch 等价允许 ±1 取整误差）
    expect(caret[0]).toBeGreaterThanOrEqual(254);
    expect(Math.abs(caret[1] - 204)).toBeLessThanOrEqual(2);
    expect(caret[2]).toBeLessThanOrEqual(4);
    expect(contrast(caret, bg)).toBeGreaterThanOrEqual(7);
  });

  it('浅色主题：光标对浅底对比度 ≥ 7:1（墨色不受影响）', () => {
    const caret = resolveColor(':root', '--editor-caret');
    const bg = resolveColor(':root', '--bg');
    expect(contrast(caret, bg)).toBeGreaterThanOrEqual(7);
  });
});

describe('editorTheme 光标接管', () => {
  it('挂载编辑器后注入的样式引用 --editor-caret（.cm-cursor 与 caret-color）', () => {
    stubMatchMedia(false);
    const { container, unmount } = mount(<div />);
    const view = new EditorView({
      parent: container,
      doc: 'a\nb\n',
      extensions: [...editableExtensions, editorTheme],
    });
    const styleText = [...document.querySelectorAll('style')]
      .map((s) => s.textContent ?? '')
      .join('\n');
    expect(styleText).toContain('--editor-caret');
    expect(styleText).toContain('caret-color');
    expect(styleText).toMatch(/\.cm-cursor[^{]*\{[^}]*border-left-color:\s*var\(--editor-caret\)/);
    view.destroy();
    unmount();
  });
});

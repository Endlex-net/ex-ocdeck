import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import {
  baseRefShortName,
  currentBranchTooltip,
  sourceBranchTooltip,
} from '../pages/workbench-branch';

/* ==================== base_ref 短名转换与页头分支 tooltip 模板（tasks 2.2 / design D2/D3/D5） ==================== */

describe('baseRefShortName（design D2/D5）', () => {
  it('refs/heads/<name> → <name>', () => {
    expect(baseRefShortName('refs/heads/main')).toBe('main');
    expect(baseRefShortName('refs/heads/feature/x')).toBe('feature/x');
  });

  it('refs/remotes/<name> → <name>（保留 remote 段）', () => {
    expect(baseRefShortName('refs/remotes/origin/main')).toBe('origin/main');
  });

  it('空串返回空串', () => {
    expect(baseRefShortName('')).toBe('');
  });

  it('未匹配前缀的输入原样返回，不抛错', () => {
    expect(baseRefShortName('main')).toBe('main');
    expect(baseRefShortName('refs/tags/v1.0')).toBe('refs/tags/v1.0');
    expect(baseRefShortName('refs/headsmain')).toBe('refs/headsmain');
  });

  it('前缀后为空（异常形态）原样返回', () => {
    expect(baseRefShortName('refs/heads/')).toBe('refs/heads/');
    expect(baseRefShortName('refs/remotes/')).toBe('refs/remotes/');
  });
});

describe('页头分支 tooltip 唯一模板集（design D3）', () => {
  it('来源 button：原值与短名不同时携带全限定 ref', () => {
    expect(sourceBranchTooltip('main', 'refs/heads/main')).toBe(
      '来源分支：main（refs/heads/main）（点击复制）',
    );
    expect(sourceBranchTooltip('origin/main', 'refs/remotes/origin/main')).toBe(
      '来源分支：origin/main（refs/remotes/origin/main）（点击复制）',
    );
  });

  it('来源 button：原值与短名相同（异常形态原样返回）不重复携带', () => {
    expect(sourceBranchTooltip('weird-ref', 'weird-ref')).toBe('来源分支：weird-ref（点击复制）');
  });

  it('当前 button 模板', () => {
    expect(currentBranchTooltip('feature-x')).toBe('当前分支：feature-x（点击复制）');
  });
});

/* 页头分支区 CSS 声明契约（两行试验方案）：jsdom 无法验证布局，这里只钉住实际检查的
 * CSS 声明——容器 inline-flex 且解除外层裁切、10px 两行、行容器列方向堆叠、按钮声明
 * 最小可见宽度与独立截断、来源弱化、当前提亮、任务标题声明可收缩截断。
 * 可见宽度/命中区域/焦点环/页头垂直平衡的实际视觉效果需人工浏览器核对，
 * 不以正则断言代替。 */
describe('页头分支区 CSS 声明契约（两行试验方案）', () => {
  const css = readFileSync(
    fileURLToPath(new URL('../legacy-components.css', import.meta.url)),
    'utf8',
  );
  /** 取 selector 首次出现处的规则体（同 workbench-overflow.test.ts 的 CSS 契约先例）。 */
  const ruleBody = (selector: string): string => {
    const idx = css.indexOf(selector);
    if (idx < 0) return '';
    const open = css.indexOf('{', idx);
    const close = css.indexOf('}', open);
    return css.slice(open + 1, close);
  };
  const minWidthCh = (selector: string): number =>
    Number(ruleBody(selector).match(/min-width:\s*(\d+)ch/)?.[1] ?? 0);

  it('.header-meta-branches 声明 display:inline-flex、overflow:visible、column-gap（ch）、font-size:10px 与 ch 单位 min-width 下限', () => {
    const body = ruleBody('.header-meta-branches {');
    expect(body).toMatch(/display:\s*inline-flex/);
    expect(body).toMatch(/overflow:\s*visible/);
    expect(body).toMatch(/column-gap:\s*[\d.]+ch/);
    expect(body).toMatch(/font-size:\s*10px/);
    expect(minWidthCh('.header-meta-branches {')).toBeGreaterThan(0);
  });

  it('.branch-rows 声明列方向 flex 堆叠（两行结构）', () => {
    const body = ruleBody('.header-meta-branches .branch-rows {');
    expect(body).toMatch(/display:\s*flex/);
    expect(body).toMatch(/flex-direction:\s*column/);
  });

  it('.branch-src-row 声明 display:flex、gap（ch）与 font-size:9px（来源行比上行 10px 再小一档）', () => {
    const body = ruleBody('.header-meta-branches .branch-src-row {');
    expect(body).toMatch(/display:\s*flex/);
    expect(body).toMatch(/gap:\s*[\d.]+ch/);
    expect(body).toMatch(/font-size:\s*9px/);
  });

  it('.workbench .page-title 声明 min-width:0 + overflow:hidden + text-overflow:ellipsis（标题收缩吸收）', () => {
    const body = ruleBody('.workbench .page-title {');
    expect(body).toMatch(/min-width:\s*0/);
    expect(body).toMatch(/overflow:\s*hidden/);
    expect(body).toMatch(/text-overflow:\s*ellipsis/);
  });

  it('.header-meta-branches svg 声明 flex:none（图标不收缩）', () => {
    expect(ruleBody('.header-meta-branches svg {')).toMatch(/flex:\s*none/);
  });

  it('.branch-copy-cur 声明 color:var(--fg)（当前分支提亮到正文色，与弱化来源形成层级对比）', () => {
    expect(ruleBody('.header-meta .branch-copy-cur {')).toMatch(/color:\s*var\(--fg\)/);
  });

  it('.branch-copy 声明最小可见宽度（ch 单位）与 max-width:100% + overflow:hidden + text-overflow:ellipsis', () => {
    const body = ruleBody('.header-meta .branch-copy {');
    expect(body).toMatch(/min-width:\s*\d+ch/);
    expect(body).toMatch(/max-width:\s*100%/);
    expect(body).toMatch(/overflow:\s*hidden/);
    expect(body).toMatch(/text-overflow:\s*ellipsis/);
  });
});

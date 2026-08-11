import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { shouldCloseOverflowOnBlur } from '../pages/workbench-overflow';

// 回归：工作台 ⋯ 菜单「删除任务」点击丢失（Bug 1）
// 根因：onBlur 在 relatedTarget=null 时关菜单——触屏（iOS Safari）/桌面 Safari 点击按钮
// 不转移焦点，focusout(null) 先于 click 派发，菜单卸载把点击目标一起带走，弹窗不出。
describe('WorkbenchOverflow 失焦关闭判定', () => {
  const el = (name: string) => ({ name }) as unknown as Node;
  const inside = el('menu-item');
  const outside = el('elsewhere');
  const contains = (n: Node) => n === inside;

  it('relatedTarget=null（触屏/Safari 点击的副作用失焦）→ 不关闭，交给 backdrop 兜底', () => {
    expect(shouldCloseOverflowOnBlur(null, contains)).toBe(false);
  });

  it('焦点落到溢出区内元素（点击菜单项/触发器）→ 不关闭', () => {
    expect(shouldCloseOverflowOnBlur(inside, contains)).toBe(false);
  });

  it('焦点真实落到溢出区外元素（键盘 Tab 等）→ 关闭', () => {
    expect(shouldCloseOverflowOnBlur(outside, contains)).toBe(true);
  });
});

// 回归：指挥中心页头下方大片空白（Bug 2）
// 根因：.cc-page 曾是 flex 列容器，flex item 间 margin 不折叠，页头 margin-bottom:22
// 与首分区 margin-top:26 由设计稿折叠值 26px 叠加成 48px（视觉空白带 ~74-90px）。
// 契约：.cc-page 保持块级流，恢复兄弟 margin 折叠（设计源 command-center.html 实测 gap=26px）。
describe('指挥中心 .cc-page 布局契约', () => {
  const ccCss = readFileSync(
    fileURLToPath(new URL('../pages/command-center.css', import.meta.url)),
    'utf8',
  );
  const ccPageRule = ccCss.match(/\.cc-page\s*\{([^}]*)\}/)?.[1] ?? '';

  it('.cc-page 规则存在', () => {
    expect(ccPageRule).not.toBe('');
  });

  it('.cc-page 不是 flex 列容器（否则页头与首分区 margin 叠加撑出空白带）', () => {
    expect(ccPageRule).not.toMatch(/display:\s*(flex|inline-flex)/);
  });
});

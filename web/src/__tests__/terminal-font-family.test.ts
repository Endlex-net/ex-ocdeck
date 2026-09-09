import { describe, expect, it, vi } from 'vitest';
import {
  DEFAULT_FONT_FAMILY,
  FONT_FAMILY_KEY,
  FONT_SIZE_KEY,
  resolveFontFamily,
  saveTermPrefs,
  validateFontFamily,
} from '../terminal/preferences';
import { stubStorage } from './dom-test-utils';

const { store, writes } = stubStorage(); // saveTermPrefs 校验测试需要 localStorage 桩（首调用注册 beforeEach/afterEach）

/**
 * D3 字体栈变换八例全集 + 幂等（terminal-links-emoji-icons 4.4，design D3 测试用例清单）。
 * 规则：① CSS family 列表语义解析（引号/转义/名字含逗号）；② emoji 三族已存在保留原位、
 * 缺失按固定顺序追加末尾；③ Symbols 移除全部已存在项后统一追加绝对末尾。
 * 伴随断言：不触碰 localStorage（纯函数无副作用，写入侧测试见 terminal-fonts-view.test.tsx）。
 */

describe('D3 resolveFontFamily 字体栈变换（design D3 八例全集）', () => {
  it('1. 默认栈恒等（emoji 三族已内置原位、Symbols 已在最末）', () => {
    expect(resolveFontFamily({})).toBe(DEFAULT_FONT_FAMILY);
    expect(resolveFontFamily({ fontFamily: DEFAULT_FONT_FAMILY })).toBe(DEFAULT_FONT_FAMILY);
  });

  it('2. 单字体自定义：原字体保留，emoji 按固定顺序追加，Symbols 绝对末尾', () => {
    expect(resolveFontFamily({ fontFamily: 'Fira Code' })).toBe(
      'Fira Code, "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('3. 含 generic（monospace）：用户栈在前，回退族在其后', () => {
    expect(resolveFontFamily({ fontFamily: '"Fira Code", monospace' })).toBe(
      '"Fira Code", monospace, "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('4. emoji 族已含部分：已有的保留原位，缺失的按固定顺序追加末尾', () => {
    expect(resolveFontFamily({ fontFamily: '"Apple Color Emoji", "Fira Code"' })).toBe(
      '"Apple Color Emoji", "Fira Code", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('5. Symbols 原在开头：移除后统一归绝对末尾', () => {
    expect(resolveFontFamily({ fontFamily: '"Symbols Nerd Font Mono", "Fira Code"' })).toBe(
      '"Fira Code", "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('6. Symbols 原在末尾但缺 emoji 族：emoji 追加后 Symbols 仍最末', () => {
    expect(resolveFontFamily({ fontFamily: '"Fira Code", "Symbols Nerd Font Mono"' })).toBe(
      '"Fira Code", "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('7. 单引号包裹的自定义族：引号形态原样保留，回退族以规范双引号追加', () => {
    expect(resolveFontFamily({ fontFamily: "'Fira Code', Arial" })).toBe(
      "'Fira Code', Arial, \"Apple Color Emoji\", \"Segoe UI Emoji\", \"Noto Color Emoji\", \"Symbols Nerd Font Mono\"",
    );
  });

  it('8. 名字含逗号的合法 family：不被逗号拆散、不改写', () => {
    expect(resolveFontFamily({ fontFamily: '"Example, Mono", Arial' })).toBe(
      '"Example, Mono", Arial, "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });
});

describe('D3 变换幂等性与规范化边界', () => {
  it('幂等：任意输入二次变换结果不变', () => {
    const inputs = [
      'Fira Code',
      '"Symbols Nerd Font Mono", "Fira Code"',
      '"Apple Color Emoji", "Fira Code"',
      "'Fira Code', Arial",
      '"Example, Mono", Arial',
      DEFAULT_FONT_FAMILY,
      'ui-monospace',
    ];
    for (const input of inputs) {
      const once = resolveFontFamily({ fontFamily: input });
      expect(resolveFontFamily({ fontFamily: once })).toBe(once);
    }
  });

  it('Symbols 引号/大小写变体同样被移除并规范追加（无论位置/引号形态）', () => {
    expect(resolveFontFamily({ fontFamily: "'symbols nerd font mono', Fira Code" })).toBe(
      'Fira Code, "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
    expect(resolveFontFamily({ fontFamily: 'SYMBOLS NERD FONT MONO' })).toBe(
      '"Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('未加引号的多词 emoji 族视为已存在、保留原位', () => {
    expect(resolveFontFamily({ fontFamily: 'Apple Color Emoji, Fira Code' })).toBe(
      'Apple Color Emoji, Fira Code, "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('空/空白输入回默认栈；尾逗号空项丢弃', () => {
    expect(resolveFontFamily({ fontFamily: '' })).toBe(DEFAULT_FONT_FAMILY);
    expect(resolveFontFamily({ fontFamily: '   ' })).toBe(DEFAULT_FONT_FAMILY);
    expect(resolveFontFamily({ fontFamily: 'Fira Code, ,' })).toBe(
      'Fira Code, "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('CSS 十六进制转义解码（ora-15 P1）：\\53 ymbols 前置的 Symbols 被识别并移除、规范追加到绝对末尾', () => {
    // \53 = 'S'（1 位 hex + 可选终止空白被消费）：浏览器识别为 Symbols 字体，
    // 比较键必须解码匹配，否则前置保留 + 末尾又追加（违反「移除所有已有项」契约）
    expect(resolveFontFamily({ fontFamily: '"\\53 ymbols Nerd Font Mono", "Fira Code"' })).toBe(
      '"Fira Code", "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
    // 无终止空白的 1 位 hex 转义（首个非 hex 字符即止）
    expect(resolveFontFamily({ fontFamily: '\\53ymbols Nerd Font Mono' })).toBe(
      '"Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('CSS 十六进制转义 emoji 族名：识别为已存在、保留原位原文不改写', () => {
    // \41 = 'A'：浏览器识别为 Apple Color Emoji → 保留原位，仅补缺失的两族
    expect(resolveFontFamily({ fontFamily: '"\\41 pple Color Emoji", Fira Code' })).toBe(
      '"\\41 pple Color Emoji", Fira Code, "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('转义输入二次变换幂等（输出含规范族名，再变换不变）', () => {
    const once = resolveFontFamily({ fontFamily: '"\\53 ymbols Nerd Font Mono", Fira Code' });
    expect(resolveFontFamily({ fontFamily: once })).toBe(once);
  });

  it('P4 超范围/零/代理项码点不抛异常：替换 U+FFFD、非目标原文保留、末尾规范追加', () => {
    const withMonospace = (family: string): string =>
      resolveFontFamily({ fontFamily: `"${family}", monospace` });
    const expectedTail =
      ', "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"';
    // 六位 hex 超出 U+10FFFF（原实现 String.fromCodePoint 抛 RangeError）
    expect(() => withMonospace('\\110000')).not.toThrow();
    expect(withMonospace('\\110000')).toBe(`"\\110000", monospace${expectedTail}`);
    expect(withMonospace('\\ffffff')).toBe(`"\\ffffff", monospace${expectedTail}`);
    // 零码点与代理项 → U+FFFD（同样不抛、不误判为回退族）
    expect(withMonospace('\\0')).toBe(`"\\0", monospace${expectedTail}`);
    expect(withMonospace('\\d800')).toBe(`"\\d800", monospace${expectedTail}`);
    // 有效上界：U+10FFFF 合法解码，不抛
    expect(withMonospace('\\10ffff')).toBe(`"\\10ffff", monospace${expectedTail}`);
    // 幂等
    for (const family of ['\\110000', '\\ffffff', '\\0', '\\d800', '\\10ffff']) {
      const once = withMonospace(family);
      expect(resolveFontFamily({ fontFamily: once })).toBe(once);
    }
  });

  it('P5 hex 终止 CRLF 整体消费（CSS 预处理 CRLF=一个换行）：前置 Symbols 漏识别修复', () => {
    // "\53" + 实际 CRLF + "ymbols…"：终止符 CR+LF 均消费 → 识别为 Symbols → 移除并归末尾
    expect(
      resolveFontFamily({ fontFamily: '"\\53\r\nymbols Nerd Font Mono", "Fira Code"' }),
    ).toBe(
      '"Fira Code", "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
    // 单独 CR / FF 作为终止符同样消费
    expect(
      resolveFontFamily({ fontFamily: '"\\53\rymbols Nerd Font Mono", "Fira Code"' }),
    ).toBe(
      '"Fira Code", "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('P5 hex 终止空白仅 CSS 空白集：NBSP 不是终止符、不得误判为 Symbols', () => {
    // \53 + NBSP + ymbols：NBSP 不被消费 → 解码为 "S\u00A0ymbols…" ≠ Symbols → 原文保留
    expect(resolveFontFamily({ fontFamily: '"\\53\u00A0ymbols Nerd Font Mono", "Fira Code"' })).toBe(
      '"\\53\u00A0ymbols Nerd Font Mono", "Fira Code", "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('P5 引号字符串转义续行（\\ + 实际 LF → 删除两者）：Sym\\␊bols 识别为 Symbols', () => {
    expect(resolveFontFamily({ fontFamily: '"Sym\\\nbols Nerd Font Mono", "Fira Code"' })).toBe(
      '"Fira Code", "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('P6 转义 NBSP 不是空白折叠对象：Symbols+NBSP 族名 ≠ 目标图标族，原项保留、真 Symbols 归末尾', () => {
    // \A0 = NBSP：实际族名是 Symbols<NBSP>Nerd Font Mono（≠ Symbols Nerd Font Mono）——
    // 比较键 MUST NOT 把 NBSP 折叠成普通空格后误判为图标族（原项保留 + 末尾又追加即违约）
    expect(resolveFontFamily({ fontFamily: '"Symbols\\A0 Nerd Font Mono", "Fira Code"' })).toBe(
      '"Symbols\\A0 Nerd Font Mono", "Fira Code", "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
    // 幂等
    const once = resolveFontFamily({ fontFamily: '"Symbols\\A0 Nerd Font Mono", "Fira Code"' });
    expect(resolveFontFamily({ fontFamily: once })).toBe(once);
  });

  it('P6 转义 NBSP 的 emoji 族名：识别为「非该系统族」，真正的系统族不被漏追加', () => {
    // Apple<NBSP>Color Emoji ≠ Apple Color Emoji → 三族缺失照常按固定顺序追加
    expect(resolveFontFamily({ fontFamily: '"Apple\\A0 Color Emoji", "Fira Code"' })).toBe(
      '"Apple\\A0 Color Emoji", "Fira Code", "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('P6 字面 NBSP 族名（无转义）同样保留：列表切片 trim 只裁 CSS 空白', () => {
    expect(resolveFontFamily({ fontFamily: '"Symbols\u00A0Nerd Font Mono", "Fira Code"' })).toBe(
      '"Symbols\u00A0Nerd Font Mono", "Fira Code", "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
    // 切片边界的 NBSP 不被裁掉（原文保持）
    expect(resolveFontFamily({ fontFamily: 'Fira Code\u00A0, Arial' })).toBe(
      'Fira Code\u00A0, Arial, "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
  });

  it('P7 引号内连续空格是名字内容：Symbols 双空格变体 ≠ 目标族，原项保留、真 Symbols 归末尾', () => {
    expect(resolveFontFamily({ fontFamily: '"Symbols  Nerd Font Mono", "Fira Code"' })).toBe(
      '"Symbols  Nerd Font Mono", "Fira Code", "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
    // 幂等
    const once = resolveFontFamily({ fontFamily: '"Symbols  Nerd Font Mono", "Fira Code"' });
    expect(resolveFontFamily({ fontFamily: once })).toBe(once);
  });

  it('P7 引号内前导空格是名字内容：" Apple Color Emoji" 不误判已有，真族仍追加', () => {
    expect(resolveFontFamily({ fontFamily: '" Apple Color Emoji", "Fira Code"' })).toBe(
      '" Apple Color Emoji", "Fira Code", "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
    // 幂等
    const once = resolveFontFamily({ fontFamily: '" Apple Color Emoji", "Fira Code"' });
    expect(resolveFontFamily({ fontFamily: once })).toBe(once);
  });

  it('P8 未引号转义前导空格（\\20 = 内容空格）：不误判已有 emoji 族、真族照常追加', () => {
    // 实际字体名是 " Apple Color Emoji"（前导内容空格）≠ Apple Color Emoji
    expect(resolveFontFamily({ fontFamily: '\\20 Apple Color Emoji, "Fira Code"' })).toBe(
      '\\20 Apple Color Emoji, "Fira Code", "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
    const once = resolveFontFamily({ fontFamily: '\\20 Apple Color Emoji, "Fira Code"' });
    expect(resolveFontFamily({ fontFamily: once })).toBe(once);
  });

  it('P8 未引号转义内容空格（两个 \\20 ）：不误判图标族、原项保留', () => {
    // 实际字体名是 "Symbols  Nerd Font Mono"（两个内容空格）≠ Symbols Nerd Font Mono
    expect(resolveFontFamily({ fontFamily: 'Symbols\\20 \\20 Nerd Font Mono, "Fira Code"' })).toBe(
      'Symbols\\20 \\20 Nerd Font Mono, "Fira Code", "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
    const once = resolveFontFamily({ fontFamily: 'Symbols\\20 \\20 Nerd Font Mono, "Fira Code"' });
    expect(resolveFontFamily({ fontFamily: once })).toBe(once);
  });

  it('P8 转义空格在列表项边界（Fira\\ ）不被裁切：原文与转义完整保留', () => {
    expect(resolveFontFamily({ fontFamily: 'Fira\\ , Arial' })).toBe(
      'Fira\\ , Arial, "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", "Symbols Nerd Font Mono"',
    );
    const once = resolveFontFamily({ fontFamily: 'Fira\\ , Arial' });
    expect(resolveFontFamily({ fontFamily: once })).toBe(once);
  });
});

describe('P9 字体族保存校验（输入域 = 无注释 CSS family 列表，design D3 输入域约束）', () => {
  it('validateFontFamily：含注释起止符判非法，正常值合法', () => {
    expect(validateFontFamily('Fira Code, monospace')).toBe(true);
    expect(validateFontFamily('/* evil')).toBe(false);
    expect(validateFontFamily('evil */')).toBe(false);
    expect(validateFontFamily('a /* b */ c')).toBe(false);
  });

  it('含注释符的 fontFamily 保存被拒：抛错、存储零写入、已有偏好原值不变；与合法 fontSize 组合同样整体不写入', () => {
    // 预置已有偏好：被拒保存不得误删/改写原值
    store.set(FONT_FAMILY_KEY, 'Fira Code');
    store.set(FONT_SIZE_KEY, '13');
    const save = vi.fn(() => saveTermPrefs({ fontFamily: 'Fira /* Code' }));
    expect(save).toThrow('字体族不能包含 CSS 注释符');
    expect(store.get(FONT_FAMILY_KEY)).toBe('Fira Code'); // 原值不变（无误删）
    expect(store.get(FONT_SIZE_KEY)).toBe('13');
    expect(writes.count).toBe(0);

    const saveBoth = vi.fn(() => saveTermPrefs({ fontFamily: 'a */ b', fontSize: 13 }));
    expect(saveBoth).toThrow('字体族不能包含 CSS 注释符');
    expect(store.get(FONT_FAMILY_KEY)).toBe('Fira Code');
    expect(store.get(FONT_SIZE_KEY)).toBe('13'); // fontSize 合法也整体不写入、原值不变
    expect(writes.count).toBe(0);
  });

  it('正常值不受影响：合法 fontFamily/fontSize 照常写入；trim 后空仍走删除 key 路径', () => {
    saveTermPrefs({ fontFamily: 'Fira Code', fontSize: 13 });
    expect(store.get(FONT_FAMILY_KEY)).toBe('Fira Code');
    expect(store.get(FONT_SIZE_KEY)).toBe('13');
    expect(writes.count).toBe(2);

    saveTermPrefs({ fontFamily: '  ' });
    expect(store.has(FONT_FAMILY_KEY)).toBe(false); // 未设置语义保持
    expect(store.has(FONT_SIZE_KEY)).toBe(false); // 未提供 → 删除 key（该项回默认）
    expect(writes.count).toBe(2); // 桩只计 setItem；两个 removeItem 不计入
  });
});

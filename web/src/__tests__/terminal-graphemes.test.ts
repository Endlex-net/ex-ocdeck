// @vitest-environment jsdom
import { describe, expect, it, beforeEach, afterEach } from 'vitest';
import { Terminal } from '@xterm/xterm';
import { UnicodeGraphemesAddon } from '@xterm/addon-unicode-graphemes';
import { stubMatchMedia } from './cm-test-env';

/**
 * D2 Emoji grapheme 真实 provider 缓冲宽度组件测试（terminal-links-emoji-icons 3.2）：
 * 真实 xterm Terminal + 真实 UnicodeGraphemesAddon（loadAddon 即激活 '15-graphemes' provider），
 * 写入样本序列断言缓冲单元格宽度/单簇合并/光标列：
 * - ASCII 宽 1、CJK 宽 2 不变（普通文本宽度承诺）；
 * - 👨‍👩‍👧（ZWJ）、🏳️‍🌈（VS16+ZWJ）、👍🏽（肤色修饰符）、🇨🇳（区域指示符配对）、é（e+U+0301
 *   组合附加符号）各为单个 grapheme cluster 占位；
 * - 连续 emoji 😀😀😀 三个独立簇各占 2 格，互不粘连（用户实证的修复前缺陷）。
 */

interface CellInfo {
  chars: string;
  width: number;
}

/** 读取缓冲第 y 行的全部非零宽单元格（零宽为合成簇的延续格，不计）。 */
function cellsOf(term: Terminal, y: number): CellInfo[] {
  const line = term.buffer.active.getLine(y);
  if (!line) return [];
  const cells: CellInfo[] = [];
  for (let x = 0; x < line.length; x++) {
    const cell = line.getCell(x);
    if (!cell || cell.getWidth() === 0) continue;
    cells.push({ chars: cell.getChars(), width: cell.getWidth() });
  }
  // 截去行尾空白填充
  while (cells.length > 0 && cells[cells.length - 1].chars === '' && cells[cells.length - 1].width === 1) {
    cells.pop();
  }
  return cells;
}

/** 等待 xterm write 缓冲落盘。 */
function written(term: Terminal, data: string): Promise<void> {
  return new Promise((resolve) => term.write(data, resolve));
}

let host: HTMLElement;
let term: Terminal;

beforeEach(() => {
  stubMatchMedia(false);
  host = document.createElement('div');
  document.body.appendChild(host);
  term = new Terminal({ allowProposedApi: true, cols: 80, rows: 24 });
  term.open(host);
  term.loadAddon(new UnicodeGraphemesAddon());
});

afterEach(() => {
  term.dispose();
  host.remove();
});

describe('D2 Emoji grapheme 真实 provider（UnicodeGraphemesAddon）', () => {
  it('loadAddon 即激活 15-graphemes provider（unicode.activeVersion 切换）', () => {
    expect(term.unicode.activeVersion).toBe('15-graphemes');
  });

  it('ASCII：宽 1 不变，光标按列推进', async () => {
    await written(term, 'ab');
    expect(cellsOf(term, 0)).toEqual([
      { chars: 'a', width: 1 },
      { chars: 'b', width: 1 },
    ]);
    expect(term.buffer.active.cursorX).toBe(2);
  });

  it('CJK：宽 2 不变（既有宽度行为承诺）', async () => {
    await written(term, '中文');
    expect(cellsOf(term, 0)).toEqual([
      { chars: '中', width: 2 },
      { chars: '文', width: 2 },
    ]);
    expect(term.buffer.active.cursorX).toBe(4);
  });

  it('ZWJ 组合 emoji 👨‍👩‍👧：单簇、占 2 格', async () => {
    await written(term, '👨‍👩‍👧');
    expect(cellsOf(term, 0)).toEqual([{ chars: '👨‍👩‍👧', width: 2 }]);
    expect(term.buffer.active.cursorX).toBe(2);
  });

  it('旗帜 + VS16 序列 🏳️‍🌈：单簇、占 2 格', async () => {
    await written(term, '🏳️‍🌈');
    expect(cellsOf(term, 0)).toEqual([{ chars: '🏳️‍🌈', width: 2 }]);
  });

  it('肤色修饰符 👍🏽：单簇、占 2 格', async () => {
    await written(term, '👍🏽');
    expect(cellsOf(term, 0)).toEqual([{ chars: '👍🏽', width: 2 }]);
  });

  it('区域指示符配对 🇨🇳：单簇、占 2 格', async () => {
    await written(term, '🇨🇳');
    expect(cellsOf(term, 0)).toEqual([{ chars: '🇨🇳', width: 2 }]);
  });

  it('组合附加符号 é（e+U+0301）：单簇、占 1 格（组合符号不另占列）', async () => {
    await written(term, 'e\u0301');
    expect(cellsOf(term, 0)).toEqual([{ chars: 'e\u0301', width: 1 }]);
    expect(term.buffer.active.cursorX).toBe(1);
  });

  it('连续 emoji 😀😀😀：三个独立簇各占 2 格，互不粘连（用户实证缺陷回归锚点）', async () => {
    await written(term, '😀😀😀');
    expect(cellsOf(term, 0)).toEqual([
      { chars: '😀', width: 2 },
      { chars: '😀', width: 2 },
      { chars: '😀', width: 2 },
    ]);
    expect(term.buffer.active.cursorX).toBe(6);
  });

  it('混合行：CJK 与 ZWJ/连续 emoji 混排各归各位、无宽度错位', async () => {
    await written(term, '中😀😀文👨‍👩‍👧');
    expect(cellsOf(term, 0)).toEqual([
      { chars: '中', width: 2 },
      { chars: '😀', width: 2 },
      { chars: '😀', width: 2 },
      { chars: '文', width: 2 },
      { chars: '👨‍👩‍👧', width: 2 },
    ]);
    expect(term.buffer.active.cursorX).toBe(10);
  });
});

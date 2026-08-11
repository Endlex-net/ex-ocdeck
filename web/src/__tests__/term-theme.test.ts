import { describe, it, expect, afterEach } from 'vitest';
import {
  readCurrentTermTheme,
  resolveXtermTheme,
  watchTermTheme,
  XTERM_THEME_DARK,
  XTERM_THEME_LIGHT,
} from '../terminal/theme';

const ANSI_KEYS = [
  'background',
  'foreground',
  'cursor',
  'cursorAccent',
  'selectionBackground',
  'black',
  'red',
  'green',
  'yellow',
  'blue',
  'magenta',
  'cyan',
  'white',
  'brightBlack',
  'brightRed',
  'brightGreen',
  'brightYellow',
  'brightBlue',
  'brightMagenta',
  'brightCyan',
  'brightWhite',
] as const;

/* ---- node 环境下的手动 DOM stub（项目无 jsdom，与 session-adapter 同风格） ---- */

let savedDocument: Document | undefined;
let savedMO: unknown;

class FakeMutationObserver {
  static last: FakeMutationObserver | null = null;
  disconnected = false;
  constructor(private cb: () => void) {
    FakeMutationObserver.last = this;
  }
  observe() {}
  disconnect() {
    this.disconnected = true;
  }
  /** 测试同步驱动（不等真实 MutationObserver 微任务）。 */
  trigger() {
    if (!this.disconnected) this.cb();
  }
}

function stubDom(attrs: Record<string, string>) {
  savedDocument = (globalThis as { document?: Document }).document;
  savedMO = (globalThis as { MutationObserver?: unknown }).MutationObserver;
  (globalThis as { document: Document }).document = {
    documentElement: {
      getAttribute: (k: string) => attrs[k] ?? null,
    },
  } as unknown as Document;
  (globalThis as { MutationObserver: unknown }).MutationObserver = FakeMutationObserver;
}

afterEach(() => {
  if (savedDocument === undefined) delete (globalThis as { document?: Document }).document;
  else (globalThis as { document: Document }).document = savedDocument;
  if (savedMO === undefined) delete (globalThis as { MutationObserver?: unknown }).MutationObserver;
  else (globalThis as { MutationObserver: unknown }).MutationObserver = savedMO;
  savedDocument = undefined;
  savedMO = undefined;
  FakeMutationObserver.last = null;
});

describe('resolveXtermTheme（主题→配色解析，纯函数）', () => {
  it('dark → 暗色终端配色（对齐 --term-* 暗色组）', () => {
    const t = resolveXtermTheme('dark');
    expect(t).toBe(XTERM_THEME_DARK);
    expect(t.background).toBe('#090a0c'); // --term-bg oklch(0.145 0.005 260)
    expect(t.foreground).toBe('#e9e9e9'); // --term-fg white 92% ink
  });

  it('light → 亮色终端配色（对齐 --term-* 亮色组）', () => {
    const t = resolveXtermTheme('light');
    expect(t).toBe(XTERM_THEME_LIGHT);
    expect(t.background).toBe('#f4f6f8'); // --term-bg oklch(0.972 0.004 260)
    expect(t.foreground).toBe('#202020'); // --term-fg ink 92% white
  });

  it('两套配色均覆盖完整 ITheme 键且亮暗基底相反', () => {
    for (const key of ANSI_KEYS) {
      expect(XTERM_THEME_DARK[key], `dark.${key}`).toBeTruthy();
      expect(XTERM_THEME_LIGHT[key], `light.${key}`).toBeTruthy();
    }
    expect(XTERM_THEME_DARK.background).not.toBe(XTERM_THEME_LIGHT.background);
    expect(XTERM_THEME_DARK.foreground).not.toBe(XTERM_THEME_LIGHT.foreground);
  });
});

describe('readCurrentTermTheme（data-theme channel）', () => {
  it('data-theme=dark → dark；缺省/light → light；无 DOM → light 降级', () => {
    stubDom({});
    expect(readCurrentTermTheme()).toBe('light');
    stubDom({ 'data-theme': 'dark' });
    expect(readCurrentTermTheme()).toBe('dark');
    stubDom({ 'data-theme': 'light' });
    expect(readCurrentTermTheme()).toBe('light');
    // 无 DOM（node 裸环境）：显式删除验证降级
    delete (globalThis as { document?: Document }).document;
    expect(readCurrentTermTheme()).toBe('light');
  });
});

describe('watchTermTheme（主题切换即时通知）', () => {
  it('属性变化触发回调（读当前值）；退订后不再触发', () => {
    const attrs: Record<string, string> = { 'data-theme': 'light' };
    stubDom(attrs);
    const seen: string[] = [];
    const unwatch = watchTermTheme((t) => seen.push(t));
    attrs['data-theme'] = 'dark'; // 模拟 useTheme 写 data-theme
    FakeMutationObserver.last?.trigger();
    expect(seen).toEqual(['dark']);
    unwatch();
    FakeMutationObserver.last?.trigger();
    expect(seen).toEqual(['dark']);
  });

  it('无 DOM/MutationObserver 环境降级为 no-op 退订', () => {
    delete (globalThis as { document?: Document }).document;
    delete (globalThis as { MutationObserver?: unknown }).MutationObserver;
    expect(() => watchTermTheme(() => {})()).not.toThrow();
  });
});

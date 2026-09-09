// @vitest-environment jsdom
import { describe, expect, it, vi, beforeEach, afterEach, type MockInstance } from 'vitest';
import { Terminal } from '@xterm/xterm';
import { openLink } from '../terminal/session';
import { stubMatchMedia } from './cm-test-env';

/**
 * D1 OSC 8 真实解析与激活路径（terminal-links-emoji-icons 2.2，design 测试策略表）：
 * 真实 xterm Terminal（jsdom DOM renderer）写入固定 OSC 8 序列
 * `\x1b]8;;https://example.com\x07example link\x1b]8;;\x07`，
 * 经真实 Linkifier 事件流（mousemove → provider 提供链接 → mousedown → mouseup 命中同一链接）
 * 驱动 session.openLink：修饰键 mouseup 恰好触发一次 window.open；无修饰键不打开。
 * 附：纯文本 URL 经真实 WebLinkProvider 路径的同流程验证（WebLinksAddon 入口端到端）。
 *
 * jsdom 无布局：仅注入渲染几何桩（charSize.hasValidSize / renderService.dimensions /
 * screen 元素 padding+rect），链接识别、分簇缓冲、事件流、激活门控全部走真实实现。
 */

// window.open 桩（openLink 唯一出口）
let openSpy: MockInstance<typeof window.open>;

function makeTerminal(): { term: Terminal; host: HTMLElement; dispose: () => void } {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const term = new Terminal({ allowProposedApi: true, cols: 80, rows: 24, linkHandler: { activate: openLink } });
  term.open(host);
  return { term, host, dispose: () => { term.dispose(); host.remove(); } };
}

/** 注入 jsdom 缺失的渲染几何（jsdom 无布局，cell 尺寸恒 0），并返回 screen 元素。 */
function injectGeometry(host: HTMLElement, term: Terminal): HTMLElement {
  const core = (term as unknown as {
    _core: {
      _charSizeService: Record<string, unknown>;
      _renderService: Record<string, unknown>;
    };
  })._core;
  Object.defineProperty(core._charSizeService, 'hasValidSize', { configurable: true, get: () => true });
  Object.defineProperty(core._renderService, 'dimensions', {
    configurable: true,
    get: () => ({ css: { cell: { width: 9, height: 18 }, canvas: { width: 720, height: 432 } } }),
  });
  const screen = host.querySelector<HTMLElement>('.xterm-screen')!;
  screen.style.paddingLeft = '0px';
  screen.style.paddingTop = '0px';
  return screen;
}

/** cell 坐标 → 合成鼠标事件（cellX/cellY 为 1-based 缓冲坐标）。 */
function mouseEvent(type: string, cellX: number, cellY: number, modifier: { metaKey?: boolean; ctrlKey?: boolean }): MouseEvent {
  return new MouseEvent(type, {
    clientX: (cellX - 1) * 9 + 4,
    clientY: (cellY - 1) * 18 + 9,
    bubbles: true,
    ...modifier,
  });
}

/** 真实 mousemove → mousedown → mouseup 事件流（真实 Linkifier 激活条件：mousedown 与 mouseup 命中同一链接）。 */
function clickLink(screen: HTMLElement, modifier: { metaKey?: boolean; ctrlKey?: boolean }, cellX = 3, cellY = 1): void {
  screen.dispatchEvent(mouseEvent('mousemove', cellX, cellY, modifier));
  screen.dispatchEvent(mouseEvent('mousedown', cellX, cellY, modifier));
  screen.dispatchEvent(mouseEvent('mouseup', cellX, cellY, modifier));
}

/** 等待 xterm write 缓冲落盘（WriteBuffer 异步刷新）。 */
function written(term: Terminal, data: string): Promise<void> {
  return new Promise((resolve) => term.write(data, resolve));
}

beforeEach(() => {
  stubMatchMedia(false); // jsdom 无 matchMedia（xterm open 的 CoreBrowserService 依赖）
  openSpy = vi.spyOn(window, 'open').mockReturnValue(null);
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('OSC 8 链接真实激活路径（真实 Terminal + session.openLink）', () => {
  it('Cmd（metaKey）+ mouseup 命中 OSC 8 链接 → window.open(uri) 恰一次，text 参数为 URI', async () => {
    const t = makeTerminal();
    const screen = injectGeometry(t.host, t.term);
    await written(t.term, '\x1b]8;;https://example.com\x07example link\x1b]8;;\x07');

    clickLink(screen, { metaKey: true });

    expect(openSpy).toHaveBeenCalledTimes(1);
    expect(openSpy).toHaveBeenCalledWith('https://example.com', '_blank', 'noopener,noreferrer');
    t.dispose();
  });

  it('Ctrl + mouseup 命中 OSC 8 链接 → 打开（非 macOS 平台语义）', async () => {
    const t = makeTerminal();
    const screen = injectGeometry(t.host, t.term);
    await written(t.term, '\x1b]8;;https://example.com\x07example link\x1b]8;;\x07');

    clickLink(screen, { ctrlKey: true });

    expect(openSpy).toHaveBeenCalledTimes(1);
    expect(openSpy).toHaveBeenCalledWith('https://example.com', '_blank', 'noopener,noreferrer');
    t.dispose();
  });

  it('无修饰键 mouseup 命中链接 → 不打开（真实 Linkifier 路径上的门控）', async () => {
    const t = makeTerminal();
    const screen = injectGeometry(t.host, t.term);
    await written(t.term, '\x1b]8;;https://example.com\x07example link\x1b]8;;\x07');

    clickLink(screen, {});

    expect(openSpy).not.toHaveBeenCalled();
    t.dispose();
  });

  it('OSC 8 非 http(s) 目标（allowNonHttpProtocols 不设）→ 点击无链接可激活、不打开', async () => {
    const t = makeTerminal();
    const screen = injectGeometry(t.host, t.term);
    await written(t.term, '\x1b]8;;javascript:alert(1)\x07evil\x1b]8;;\x07');

    clickLink(screen, { metaKey: true });

    expect(openSpy).not.toHaveBeenCalled();
    t.dispose();
  });

  it('纯文本 URL 经真实 WebLinkProvider 路径：Cmd+点击打开、普通点击不打开', async () => {
    const { WebLinksAddon } = await import('@xterm/addon-web-links');
    const t = makeTerminal();
    t.term.loadAddon(new WebLinksAddon(openLink));
    const screen = injectGeometry(t.host, t.term);
    await written(t.term, 'see https://example.com/path here');

    clickLink(screen, { metaKey: true }, 10, 1); // 'https://example.com/path' 起始于 col 5
    expect(openSpy).toHaveBeenCalledTimes(1);
    expect(openSpy).toHaveBeenCalledWith('https://example.com/path', '_blank', 'noopener,noreferrer');

    clickLink(screen, {}, 10, 1);
    expect(openSpy).toHaveBeenCalledTimes(1); // 无修饰键第二次点击不打开
    t.dispose();
  });
});

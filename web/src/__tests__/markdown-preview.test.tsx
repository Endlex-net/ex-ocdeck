// @vitest-environment jsdom
import { describe, it, expect } from 'vitest';
import { act } from 'react';
import MarkdownPreview from '../components/diff/MarkdownPreview';
import { mount, rerender } from './cm-test-env';

/* ============================ MarkdownPreview（tasks 2.4，design D1/D5） ============================
 * 全部用例真实经过组件渲染路径（jsdom + react-markdown 同步渲染）。
 * URL 判定层契约：作用于最终 href/src；仅 http/https 通过；缺失/空/解析异常不合格且不抛错。 */

function renderPreview(content: string) {
  return mount(<MarkdownPreview content={content} />);
}

/** 模拟图片网络加载失败（React 对媒体事件直接在元素上挂原生监听）。 */
function fireImgError(img: HTMLImageElement) {
  act(() => {
    img.dispatchEvent(new Event('error'));
  });
}

describe('GFM 元素渲染（remark-gfm）', () => {
  it('表格渲染为 table/th/td', () => {
    const { container, unmount } = renderPreview('| 左 | 右 |\n| --- | --- |\n| a1 | b1 |\n');
    const table = container.querySelector('table');
    expect(table).not.toBeNull();
    expect(container.querySelectorAll('th')).toHaveLength(2);
    expect(container.querySelectorAll('td')).toHaveLength(2);
    expect(table!.textContent).toContain('a1');
    unmount();
  });

  it('任务列表渲染 checkbox，勾选态保留', () => {
    const { container, unmount } = renderPreview('- [x] 已完成\n- [ ] 待办\n');
    const boxes = container.querySelectorAll<HTMLInputElement>('li input[type=checkbox]');
    expect(boxes).toHaveLength(2);
    expect(boxes[0].checked).toBe(true);
    expect(boxes[1].checked).toBe(false);
    unmount();
  });

  it('删除线渲染为 del', () => {
    const { container, unmount } = renderPreview('这是~~删掉的~~内容\n');
    const del = container.querySelector('del');
    expect(del).not.toBeNull();
    expect(del!.textContent).toBe('删掉的');
    unmount();
  });

  it('www 前缀自动链接经 GFM 产出 http:// href，判定通过可点击', () => {
    const { container, unmount } = renderPreview('访问 www.example.com 现在\n');
    const a = container.querySelector('a');
    expect(a).not.toBeNull();
    expect(a!.getAttribute('href')).toBe('http://www.example.com');
    expect(a!.getAttribute('target')).toBe('_blank');
    expect(a!.getAttribute('rel')).toBe('noopener noreferrer');
    unmount();
  });
});

describe('原始 HTML 不渲染（skipHtml）', () => {
  it('script 与内联 img 标签被整体忽略，其余内容正常渲染', () => {
    const { container, unmount } = renderPreview(
      '前文\n\n<script>window.__pwned = 1</script>\n\n<img src="https://evil.example/x.png">\n\n后文\n',
    );
    expect(container.querySelectorAll('script')).toHaveLength(0);
    expect(container.querySelectorAll('img')).toHaveLength(0);
    expect(container.textContent).not.toContain('window.__pwned');
    expect(container.textContent).toContain('前文');
    expect(container.textContent).toContain('后文');
    unmount();
  });
});

describe('URL 策略判定层（design D1）', () => {
  it('合格 https/http 链接可点击：target=_blank + rel=noopener noreferrer', () => {
    const { container, unmount } = renderPreview(
      '[文本](https://example.com/page) 与 [http](http://example.com)\n',
    );
    const links = container.querySelectorAll('a');
    expect(links).toHaveLength(2);
    for (const a of links) {
      expect(a.getAttribute('target')).toBe('_blank');
      expect(a.getAttribute('rel')).toBe('noopener noreferrer');
    }
    unmount();
  });

  it('相对路径、锚点、空 URL、解析异常（http://）、mailto 链接均降级为纯文本且不导航', () => {
    const { container, unmount } = renderPreview(
      [
        '[相对](./sub/page)',
        '[锚点](#section)',
        '[空]()',
        '[异常](http://)',
        '[邮件](mailto:a@b.com)',
      ].join(' 与 '),
    );
    expect(container.querySelectorAll('a')).toHaveLength(0);
    expect(container.textContent).toContain('相对');
    expect(container.textContent).toContain('锚点');
    expect(container.textContent).toContain('空');
    expect(container.textContent).toContain('异常');
    expect(container.textContent).toContain('邮件');
    unmount();
  });

  it('邮箱自动链接产出 mailto: href，降级为纯文本', () => {
    const { container, unmount } = renderPreview('发信给 a@example.com 即可\n');
    expect(container.querySelectorAll('a')).toHaveLength(0);
    expect(container.textContent).toContain('a@example.com');
    unmount();
  });

  it('嵌套图片链接（相对路径外层）整个后代树降级：图片不保留远程请求，显示 alt', () => {
    const { container, unmount } = renderPreview('[![替身](https://host.com/img.png)](./rel)\n');
    expect(container.querySelectorAll('a')).toHaveLength(0);
    expect(container.querySelectorAll('img')).toHaveLength(0);
    expect(container.textContent).toContain('替身');
    unmount();
  });

  it('合格图片渲染 img 且 loading=lazy；不合格图片统一占位并显示 alt、不保留 img/src', () => {
    const { container, unmount } = renderPreview(
      '![截图](https://host.com/i.png)\n\n![占位](./img/i.png)\n',
    );
    const img = container.querySelector('img');
    expect(img).not.toBeNull();
    expect(img!.getAttribute('src')).toBe('https://host.com/i.png');
    expect(img!.getAttribute('alt')).toBe('截图');
    expect(img!.getAttribute('loading')).toBe('lazy');
    // 相对路径图片：无 img 元素（不产生 src 请求），仅固定占位 + alt
    const fallback = container.querySelector('.md-preview-img-fallback');
    expect(fallback).not.toBeNull();
    expect(fallback!.textContent).toBe('占位');
    unmount();
  });
});

describe('图片加载失败按最终 src 隔离与重试', () => {
  it('同侧多图仅失败者降级，其余图片不受影响', () => {
    const { container, unmount } = renderPreview(
      '![甲](https://a.example/a.png)\n\n![乙](https://b.example/b.png)\n',
    );
    const imgs = container.querySelectorAll('img');
    expect(imgs).toHaveLength(2);
    fireImgError(imgs[0]);
    expect(container.querySelectorAll('img')).toHaveLength(1);
    expect(container.querySelector('img')!.getAttribute('src')).toBe('https://b.example/b.png');
    const fallbacks = container.querySelectorAll('.md-preview-img-fallback');
    expect(fallbacks).toHaveLength(1);
    expect(fallbacks[0].textContent).toBe('甲');
    unmount();
  });

  it('同 URL 图片在内容更新后允许重新加载，再次失败仍按 src 隔离降级', () => {
    const initial = '![甲](https://a.example/a.png)\n\n![乙](https://b.example/b.png)\n';
    const { container, root, unmount } = renderPreview(initial);
    fireImgError(container.querySelectorAll('img')[0]);
    expect(container.querySelectorAll('img')).toHaveLength(1);

    // 内容变化（同 src 仍在）：失败登记清空，甲重新以 img 加载
    rerender(root, <MarkdownPreview content={`${initial}\n\n新增一段\n`} />);
    const imgs = container.querySelectorAll('img');
    expect(imgs).toHaveLength(2);

    // 再次失败：仍按 src 隔离，只降级甲
    fireImgError(imgs[0]);
    expect(container.querySelectorAll('img')).toHaveLength(1);
    expect(container.querySelector('img')!.getAttribute('src')).toBe('https://b.example/b.png');
    unmount();
  });

  it('src 变化后允许重新加载，失败登记不误伤新 src', () => {
    const { container, root, unmount } = renderPreview('![图](https://x.example/1.png)\n');
    fireImgError(container.querySelector('img')!);
    expect(container.querySelectorAll('img')).toHaveLength(0);
    expect(container.querySelector('.md-preview-img-fallback')!.textContent).toBe('图');

    // src 变化：1.png 的失败登记不拦截 2.png
    rerender(root, <MarkdownPreview content="![图](https://x.example/2.png)\n" />);
    const img = container.querySelector('img');
    expect(img).not.toBeNull();
    expect(img!.getAttribute('src')).toBe('https://x.example/2.png');
    expect(container.querySelector('.md-preview-img-fallback')).toBeNull();
    unmount();
  });
});

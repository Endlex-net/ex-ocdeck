// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { readFileSync } from 'node:fs';
import { GitPanel, buildFileTree, listFileRows, compareBytewise } from '../components/GitPanel';
import { api } from '../api';
import type { GitFileEntry } from '../types';
import { mount, stubMatchMedia } from './cm-test-env';
// 测试运行时为 node（可读文件系统，vitest 以 web/ 为 cwd）；jsdom 不应用外部 CSS，故以规则文本为契约断言
const cssSource: string = readFileSync(
  `${(globalThis as unknown as { process: { cwd: () => string } }).process.cwd()}/src/legacy-components.css`,
  'utf8',
);
// 6.1 色值 token 契约：design-system.css 是 token 唯一事实来源
const dsSource: string = readFileSync(
  `${(globalThis as unknown as { process: { cwd: () => string } }).process.cwd()}/src/design-system.css`,
  'utf8',
);

/* ============================ git-page-enhancements 3.2/3.3/3.4 ============================
 * D5 混合文件列表（三色/byte-wise 排序/同路径双条目/统计与二进制展示/勾选路径级共享）、
 * D6 tree 形态（纯前端投影/目录折叠/显式切换默认 flat）、D7 容器级横向滚动 CSS 契约。 */

vi.mock('../api', () => ({
  api: {
    gitStatus: vi.fn(),
    gitDiff: vi.fn(),
    gitCommit: vi.fn(),
    gitPush: vi.fn(),
    gitBranchDiffFiles: vi.fn(),
    gitBranchDiffFile: vi.fn(),
    listAnnotations: vi.fn(async () => ({
      annotations: [],
      submitCapability: { state: 'supported', reason: '' },
    })),
    listSubmissions: vi.fn(async () => ({ queue: [], history: [], failures: [] })),
    gitFileRead: vi.fn(),
    gitFileWrite: vi.fn(),
    createAnnotation: vi.fn(),
    reportActivity: vi.fn(),
  },
  ApiError: class ApiError extends Error {
    constructor(
      public readonly status: number,
      public readonly code: string,
      message: string,
    ) {
      super(message);
    }
  },
}));

const gitStatusMock = vi.mocked(api.gitStatus);

function makeEntry(path: string, over: Partial<GitFileEntry>): GitFileEntry {
  return {
    path,
    x: 'M',
    y: ' ',
    staged: false,
    unstaged: true,
    untracked: false,
    additions: 1,
    deletions: 1,
    isBinary: false,
    ...over,
  };
}

function clickButton(container: HTMLElement, text: string) {
  const btn = [...container.querySelectorAll<HTMLButtonElement>('button')].find(
    (b) => b.textContent === text,
  );
  expect(btn, `按钮 ${text}`).toBeTruthy();
  act(() => btn!.click());
}

/** 取列表行路径（按渲染顺序）。flat 为两段式条目：.git-file-name（文件名）+
 *  .git-file-dir（目录路径，含末尾 /），重组为全路径；tree 文件节点仅文件名，返回 basename。 */
function rowPaths(container: HTMLElement): string[] {
  return [...container.querySelectorAll('.git-files .git-file-path')].map((n) => {
    const name = n.querySelector('.git-file-name')?.textContent ?? '';
    const dir = n.querySelector('.git-file-dir')?.textContent ?? '';
    return dir + name;
  });
}

async function until(cond: () => boolean, timeoutMs = 4000) {
  const start = Date.now();
  while (!cond()) {
    if (Date.now() - start > timeoutMs) throw new Error('condition timeout');
    // eslint-disable-next-line no-await-in-loop
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
  }
}

beforeEach(() => {
  vi.clearAllMocks();
  stubMatchMedia(false);
  gitStatusMock.mockResolvedValue({ branch: 'main', files: [] });
});

describe('D5：混合文件列表（单一列表替代分组）', () => {
  it('三色状态类 + UTF-8 byte-wise 排序（含非 ASCII）+ 同路径 staged 先于 unstaged 双条目', async () => {
    gitStatusMock.mockResolvedValue({
      branch: 'main',
      files: [
        // 故意乱序给出，验证排序
        makeEntry('文档/说明.md', { staged: false, unstaged: false, untracked: true }),
        makeEntry('src/app.ts', { staged: true, unstaged: true, additions: 10, deletions: 3 }),
        makeEntry('README.md', { additions: 2, deletions: 1 }),
        makeEntry('src/logo.png', { staged: true, unstaged: false, isBinary: true, additions: 0, deletions: 0 }),
      ],
    });
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => rowPaths(container).length === 5);
    // byte-wise：R(0x52) < s(0x73) < 文(U+6587)；同路径 src/app.ts staged 先于 unstaged
    expect(rowPaths(container)).toEqual([
      'README.md',
      'src/app.ts',
      'src/app.ts',
      'src/logo.png',
      '文档/说明.md',
    ]);
    const pathEls = [...container.querySelectorAll<HTMLElement>('.git-files .git-file-path')];
    expect(pathEls[0].className).toContain('git-file-unstaged');
    expect(pathEls[1].className).toContain('git-file-staged');
    expect(pathEls[2].className).toContain('git-file-unstaged');
    expect(pathEls[3].className).toContain('git-file-staged');
    expect(pathEls[4].className).toContain('git-file-untracked');
    // 颜色非唯一状态信息：title/aria-label 附状态文本
    expect(pathEls[0].getAttribute('aria-label')).toBe('README.md（未暂存）');
    expect(pathEls[4].getAttribute('aria-label')).toBe('文档/说明.md（未跟踪）');
    unmount();
  });

  it('统计与二进制展示保留在条目上；同路径双条目勾选共享（路径级 Set）', async () => {
    gitStatusMock.mockResolvedValue({
      branch: 'main',
      files: [
        makeEntry('src/app.ts', { staged: true, unstaged: true, additions: 10, deletions: 3 }),
        makeEntry('src/logo.png', { staged: true, unstaged: false, isBinary: true, additions: 0, deletions: 0 }),
      ],
    });
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => rowPaths(container).length === 3);
    const stats = [...container.querySelectorAll('.git-files .git-stat')].map((n) => n.textContent);
    expect(stats).toEqual(['+10−3', '+10−3', 'bin']);
    // 初始全选（路径级）：同路径两行的勾选状态一致
    const boxes = () =>
      [...container.querySelectorAll<HTMLInputElement>('.git-files input[type=checkbox]')];
    expect(boxes().map((b) => b.checked)).toEqual([true, true, true]);
    // 取消 src/app.ts 勾选 → 同路径 staged/unstaged 两行同时取消
    act(() => boxes()[0].click());
    expect(boxes().map((b) => b.checked)).toEqual([false, false, true]);
    expect(container.textContent).toContain('提交（1）');
    // 恢复勾选
    act(() => boxes()[0].click());
    expect(boxes().map((b) => b.checked)).toEqual([true, true, true]);
    unmount();
  });

  it('纯函数排序：非 ASCII byte-wise（MUST NOT localeCompare）与同路径固定次序', () => {
    const rows = listFileRows([
      makeEntry('文档.md', { staged: false, unstaged: false, untracked: true }),
      makeEntry('apple.ts', {}),
      makeEntry('Zebra.txt', {}),
      makeEntry('中文/文件.txt', { staged: false, unstaged: false, untracked: true }),
    ]);
    expect(rows.map((r) => r.path)).toEqual(['Zebra.txt', 'apple.ts', '中文/文件.txt', '文档.md']);
    const dual = listFileRows([makeEntry('a.txt', { staged: true, unstaged: true })]);
    expect(dual.map((r) => `${r.path}|${r.suffix}|${r.ref}`)).toEqual([
      'a.txt|staged|HEAD',
      'a.txt|unstaged|',
    ]);
    // 未跟踪单条目，ref 为空、untracked=true
    const unt = listFileRows([makeEntry('u.txt', { staged: false, unstaged: false, untracked: true })])[0];
    expect(unt).toMatchObject({ suffix: 'untracked', ref: '', untracked: true });
  });

  it('compareBytewise：ASCII 大写先于小写、CJK 码点序（非 locale 序）', () => {
    expect(compareBytewise('Zebra', 'apple')).toBe(-1);
    expect(compareBytewise('中文', '文档')).toBe(-1);
    expect(compareBytewise('a', 'a')).toBe(0);
  });
});

describe('D6：tree 形态（纯前端投影）', () => {
  const files = (): GitFileEntry[] => [
    makeEntry('文档/说明.md', { staged: false, unstaged: false, untracked: true }),
    makeEntry('src/app.ts', { staged: true, unstaged: true }),
    makeEntry('README.md', {}),
  ];

  it('投影：目录节点 key=目录路径、叶子 key=path+\\0+后缀（同路径双叶子唯一）、同级 byte-wise、仅渲染含变更目录', () => {
    const rows = listFileRows(files());
    const tree = buildFileTree(rows);
    expect(tree.map((n) => `${n.kind}:${n.name}`)).toEqual([
      'file:README.md',
      'dir:src',
      'dir:文档',
    ]);
    const srcDir = tree[1] as Extract<(typeof tree)[number], { kind: 'dir' }>;
    expect(srcDir.key).toBe('src');
    expect(srcDir.children.map((c) => c.key)).toEqual(['src/app.ts\0staged', 'src/app.ts\0unstaged']);
  });

  it('渲染：默认 flat；显式切目录树后目录行渲染；折叠为会话级状态', async () => {
    gitStatusMock.mockResolvedValue({ branch: 'main', files: files() });
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => rowPaths(container).length === 4);
    // 默认 flat：无目录节点
    expect(container.querySelectorAll('.git-tree-dir')).toHaveLength(0);
    clickButton(container, '目录树');
    // 视觉裁决：tree 文件节点仅展示文件名（目录层级由树结构表达），MUST NOT 跟随目录路径
    expect(rowPaths(container)).toEqual(['README.md', 'app.ts', 'app.ts', '说明.md']);
    const dirs = () => [...container.querySelectorAll<HTMLElement>('.git-tree-dir')];
    expect(dirs().map((d) => d.textContent)).toEqual(['▾src', '▾文档']);
    expect(dirs()[0].getAttribute('aria-expanded')).toBe('true');
    // 折叠 src：其叶子隐藏，其余不受影响
    act(() => dirs()[0].click());
    expect(dirs()[0].getAttribute('aria-expanded')).toBe('false');
    expect(rowPaths(container)).toEqual(['README.md', '说明.md']);
    // 再展开恢复
    act(() => dirs()[0].click());
    expect(rowPaths(container)).toEqual(['README.md', 'app.ts', 'app.ts', '说明.md']);
    // 切回列表形态
    clickButton(container, '列表');
    expect(container.querySelectorAll('.git-tree-dir')).toHaveLength(0);
    expect(rowPaths(container)).toEqual([
      'README.md',
      'src/app.ts',
      'src/app.ts',
      '文档/说明.md',
    ]);
    unmount();
  });

  it('branch 视图同样支持 tree 形态（叶子后缀固定 branch，不新增第四状态枚举）', async () => {
    const gitBranchDiffFilesMock = vi.mocked(api.gitBranchDiffFiles);
    gitBranchDiffFilesMock.mockResolvedValue({
      baseRef: 'main',
      files: [
        { path: 'src/app.ts', additions: 2, deletions: 0, isBinary: false },
        { path: 'README.md', additions: 0, deletions: 1, isBinary: false },
      ],
    });
    const { container, unmount } = mount(<GitPanel taskID="t1" active baseRef="main" />);
    await until(() => rowPaths(container).length === 0);
    clickButton(container, '分支对比');
    await until(() => rowPaths(container).length === 2);
    clickButton(container, '目录树');
    expect(rowPaths(container)).toEqual(['README.md', 'app.ts']);
    const dirs = () => [...container.querySelectorAll<HTMLElement>('.git-tree-dir')];
    expect(dirs().map((d) => d.textContent)).toEqual(['▾src']);
    // branch 行不套三色状态类
    expect(container.querySelector('.git-file-staged')).toBeNull();
    expect(container.querySelector('.git-file-unstaged')).toBeNull();
    expect(container.querySelector('.git-file-untracked')).toBeNull();
    // tasks 6.1 裁决 3：已提交条目视觉可区分——中性色类 + 非颜色提示（badge + title/aria-label）
    const branchPaths = [...container.querySelectorAll<HTMLElement>('.git-files .git-file-path')];
    expect(branchPaths).toHaveLength(2);
    for (const el of branchPaths) expect(el.className).toContain('git-file-committed');
    expect(branchPaths[0].getAttribute('aria-label')).toBe('README.md（已提交）');
    expect(branchPaths[1].getAttribute('aria-label')).toBe('src/app.ts（已提交）');
    expect(container.querySelectorAll('.git-files .git-file-badge')).toHaveLength(2);
    expect(
      [...container.querySelectorAll('.git-files .git-file-badge')].map((b) => b.textContent),
    ).toEqual(['已提交', '已提交']);
    unmount();
  });
});

describe('D7：列表容器级横向滚动（CSS 契约）', () => {
  // jsdom 不应用外部 CSS：以 CSS 规则文本为契约断言（还原 rtl 省略号或删掉内容层即变红）
  const css = cssSource;

  it('内容层 min-width:max-content、行 min-width:100% + width:max-content', () => {
    expect(css).toMatch(/\.git-files-inner\s*\{[^}]*min-width:\s*max-content/);
    const fileRule = css.match(/\.git-file\s*\{[^}]*\}/)![0];
    expect(fileRule).toContain('min-width: 100%');
    expect(fileRule).toContain('width: max-content');
  });

  it('.git-file-path 为正常 LTR 全路径（无 direction:rtl / ellipsis 截断）', () => {
    const pathRule = css.match(/\.git-file-path\s*\{[^}]*\}/)![0];
    expect(pathRule).not.toContain('direction');
    expect(pathRule).not.toContain('text-overflow');
    expect(pathRule).not.toContain('overflow: hidden');
  });

  it('渲染结构：.git-files 容器内存在 .git-files-inner 内容层包裹条目', async () => {
    gitStatusMock.mockResolvedValue({
      branch: 'main',
      files: [makeEntry('a.txt', {})],
    });
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => rowPaths(container).length === 1);
    const inner = container.querySelector('.git-files > .git-files-inner');
    expect(inner).not.toBeNull();
    expect(inner!.querySelector('.git-file')).not.toBeNull();
    unmount();
  });
});

describe('tasks 6.1：状态颜色重映射（用户裁决值，CSS token 契约）', () => {
  it('staged→成功绿（--success）/ unstaged→teal / untracked→警告琥珀（--warn）为裁决 token', () => {
    expect(dsSource).toContain('--git-staged: var(--success)');
    expect(dsSource).toContain('--git-unstaged: rgb(128, 203, 196)');
    expect(dsSource).toContain('--git-untracked: var(--warn)');
    // 三色 token 仅 :root 一处定义、暗色主题不覆盖
    // （staged/untracked 经 var(--success)/var(--warn) 随暗色主题提亮规则自动适配；teal 硬裁决 rgb 明暗同值）
    const darkBlock = dsSource.match(/\[data-theme="dark"\]\s*\{[^}]*\}/)![0];
    expect(darkBlock).not.toContain('--git-staged');
    expect(darkBlock).not.toContain('--git-unstaged');
    expect(darkBlock).not.toContain('--git-untracked');
  });

  it('三色类消费状态 token；已提交条目为中性色（--muted，与三色不冲突）', () => {
    expect(cssSource).toMatch(/\.git-file-staged\s*\{[^}]*color:\s*var\(--git-staged\)/);
    expect(cssSource).toMatch(/\.git-file-unstaged\s*\{[^}]*color:\s*var\(--git-unstaged\)/);
    expect(cssSource).toMatch(/\.git-file-untracked\s*\{[^}]*color:\s*var\(--git-untracked\)/);
    expect(cssSource).toMatch(/\.git-file-committed\s*\{[^}]*color:\s*var\(--muted\)/);
  });
});

describe('tasks 6.2：条目布局（flat 文件名+目录路径两段式；tree 文件节点仅文件名）', () => {
  it('flat 条目先文件名、后目录路径，根目录文件无目录段；tree 文件节点仅文件名、完整路径经 title/aria-label 保留', async () => {
    gitStatusMock.mockResolvedValue({
      branch: 'main',
      files: [
        makeEntry('src/app.ts', { staged: true, unstaged: false }),
        makeEntry('README.md', {}),
      ],
    });
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => rowPaths(container).length === 2);
    // byte-wise 排序：README.md（根目录）在前，src/app.ts 在后
    const [flatRoot, flatNested] = [
      ...container.querySelectorAll<HTMLElement>('.git-files .git-file-path'),
    ];
    expect(flatRoot.querySelector('.git-file-name')!.textContent).toBe('README.md');
    expect(flatRoot.querySelector('.git-file-dir')).toBeNull();
    const nameEl = flatNested.querySelector('.git-file-name')!;
    const dirEl = flatNested.querySelector('.git-file-dir')!;
    expect(nameEl.textContent).toBe('app.ts');
    expect(dirEl.textContent).toBe('src/');
    // 文件名先于目录路径（DOM 序即视觉序）
    expect(
      nameEl.compareDocumentPosition(dirEl) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(flatNested.getAttribute('title')).toBe('src/app.ts（已暂存）');
    // 视觉裁决：tree 文件节点仅展示文件名（目录层级由树结构表达），目录路径文本不出现在条目内
    clickButton(container, '目录树');
    const [treeRoot, treeNested] = [
      ...container.querySelectorAll<HTMLElement>('.git-files .git-file-path'),
    ];
    expect(treeRoot.querySelector('.git-file-name')!.textContent).toBe('README.md');
    expect(treeRoot.querySelector('.git-file-dir')).toBeNull();
    expect(treeNested.querySelector('.git-file-name')!.textContent).toBe('app.ts');
    expect(treeNested.querySelector('.git-file-dir')).toBeNull();
    expect(treeNested.textContent).not.toContain('src/');
    // title/aria-label 保留完整路径（两种形态一致，非视觉信息通道不变）
    expect(treeNested.getAttribute('title')).toBe('src/app.ts（已暂存）');
    expect(treeNested.getAttribute('aria-label')).toBe('src/app.ts（已暂存）');
    unmount();
  });

  it('目录段次要层级样式契约：muted 色 + 更小字号（不随条目状态色变化）', () => {
    const dirRule = cssSource.match(/\.git-file-dir\s*\{[^}]*\}/)![0];
    expect(dirRule).toContain('color: var(--muted)');
    expect(dirRule).toMatch(/font-size:\s*11px/);
  });
});

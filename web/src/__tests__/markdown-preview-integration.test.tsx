// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act } from 'react';
import { EditorView } from '@codemirror/view';
import DiffViewer from '../components/diff/DiffViewer';
import { GitPanel } from '../components/GitPanel';
import { api } from '../api';
import type { DiffViewMode } from '../components/diff/DiffViewer';
import type { FileEditRead, GitDiffResult, GitFileEntry } from '../types';
import { flushUI, mount, rerender, stubMatchMedia } from './cm-test-env';

/* ============================ DiffViewer × MarkdownPreview 集成（tasks 3.1-3.7 / 4.1-4.4） ============================
 * 真实 MarkdownPreview（react-markdown 同步渲染）+ 真实 CodeMirror 创建/销毁路径。
 * 谓词契约（spec 逐字）：canPreview = isMarkdown && merge && !truncated && view；previewActive = preview && canPreview。
 * 文件末尾 describe 为 GitPanel 级「预览不跨文件保留」（GitPanel 以三元组 key 重挂载 DiffViewer）。 */

vi.mock('../api', () => ({
  api: {
    gitStatus: vi.fn(),
    gitDiff: vi.fn(),
    gitCommit: vi.fn(),
    gitPush: vi.fn(),
    gitFileRead: vi.fn(),
    gitFileWrite: vi.fn(),
    listAnnotations: vi.fn().mockResolvedValue({
      annotations: [],
      submitCapability: { state: 'unknown', reason: '' },
    }),
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
const gitDiffMock = vi.mocked(api.gitDiff);

function makeMdDiff(over: Partial<GitDiffResult>): GitDiffResult {
  return {
    oldContent: '# 旧标题\n\n旧段落。\n',
    newContent: '# 新标题\n\n| 左 | 右 |\n| --- | --- |\n| a | b |\n',
    oldExists: true,
    newExists: true,
    oldMode: '100644',
    newMode: '100644',
    isBinary: false,
    truncated: false,
    ...over,
  };
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

function editorViews(container: HTMLElement): EditorView[] {
  return [...container.querySelectorAll('.cm-content')]
    .map((el) => EditorView.findFromDOM(el as HTMLElement))
    .filter((v): v is EditorView => v !== null && v !== undefined);
}

interface ViewerOptions {
  diff: GitDiffResult;
  path?: string;
  modeOverride?: DiffViewMode | null;
  wrapOverride?: boolean;
  editIO?: { read: ReturnType<typeof vi.fn>; write: ReturnType<typeof vi.fn> };
  editModePreferred?: boolean;
  onCreateAnnotation?: ReturnType<typeof vi.fn>;
}

function renderViewer(over: ViewerOptions) {
  const onModeChange = vi.fn();
  const onWrapChange = vi.fn();
  const mounted = mount(
    <DiffViewer
      diff={over.diff}
      path={over.path ?? 'a.md'}
      sourceRef=""
      untracked={false}
      annotations={[]}
      onCreateAnnotation={over.onCreateAnnotation ?? (async () => {})}
      editIO={over.editIO}
      editModePreferred={over.editModePreferred ?? false}
      modeOverride={over.modeOverride === undefined ? 'unified' : over.modeOverride}
      onModeChange={onModeChange}
      wrapOverride={over.wrapOverride ?? false}
      onWrapChange={onWrapChange}
    />,
  );
  return { ...mounted, onModeChange, onWrapChange };
}

function toolbarButton(container: HTMLElement, text: string): HTMLButtonElement | null {
  return (
    [...container.querySelectorAll<HTMLButtonElement>('.diff-toolbar button')].find(
      (b) => b.textContent?.trim() === text,
    ) ?? null
  );
}

function clickPreview(container: HTMLElement) {
  const btn = toolbarButton(container, '预览');
  expect(btn, '预览开关存在').toBeTruthy();
  act(() => btn!.click());
}

/** GET 编辑读取契约（D5）：content 已去除 BOM、CRLF 归一为 \n。 */
const editableRead: FileEditRead = {
  editable: true,
  content: '# 新标题\n\n新段落。\n',
  baseHash: 'h0',
  lineEnding: 'lf',
  hasBom: false,
  mode: '0644',
};

describe('预览开关资格（D3 谓词 + D2 扩展名判定）', () => {
  beforeEach(() => stubMatchMedia(false));

  it('扩展名判定：.md/.markdown/.MD 有开关；.txt/无后缀 README 无开关', async () => {
    const cases: Array<{ path: string; expectSwitch: boolean }> = [
      { path: 'a.md', expectSwitch: true },
      { path: 'b.markdown', expectSwitch: true },
      { path: 'C.MD', expectSwitch: true },
      { path: 'd.txt', expectSwitch: false },
      { path: 'README', expectSwitch: false },
    ];
    for (const c of cases) {
      const { container, unmount } = renderViewer({ diff: makeMdDiff({}), path: c.path });
      await until(() => editorViews(container).length === 1);
      expect(toolbarButton(container, '预览') !== null, `path=${c.path}`).toBe(c.expectSwitch);
      unmount();
    }
  });

  it('非 merge 状态无开关（二进制/空文件）；truncated 的 merge 状态亦无开关', async () => {
    const cases: Array<{ name: string; diff: GitDiffResult; banner?: string }> = [
      {
        name: '二进制',
        diff: makeMdDiff({ isBinary: true, oldContent: '', newContent: '' }),
      },
      {
        name: '空文件',
        diff: makeMdDiff({ oldContent: '', newContent: '' }),
      },
      {
        name: 'truncated merge',
        diff: makeMdDiff({ truncated: true, oldContent: 'a\n', newContent: 'b\n' }),
        banner: '内容过大',
      },
    ];
    for (const c of cases) {
      const { container, unmount } = renderViewer({ diff: c.diff });
      await until(() => container.querySelector('.cm-editor, .empty') !== null);
      expect(toolbarButton(container, '预览'), c.name).toBeNull();
      if (c.banner) expect(container.textContent).toContain(c.banner);
      unmount();
    }
  });

  it('编辑模式下不出现预览开关（editModePreferred 自动进入编辑）', async () => {
    const editIO = {
      read: vi.fn().mockResolvedValue(editableRead),
      write: vi.fn().mockResolvedValue({ baseHash: 'h1' }),
    };
    const { container, unmount } = renderViewer({
      diff: makeMdDiff({}),
      editIO,
      editModePreferred: true,
    });
    await until(() =>
      [...container.querySelectorAll<HTMLButtonElement>('button')].some(
        (b) => b.textContent === '退出编辑',
      ),
    );
    expect(toolbarButton(container, '预览')).toBeNull();
    expect(container.querySelectorAll('.md-diff')).toHaveLength(0);
    unmount();
  });
});

describe('进入/退出预览与渲染分支（D5/D6）', () => {
  beforeEach(() => stubMatchMedia(false));

  it('进入预览：双侧等宽渲染（旧侧/新侧标注）+ 无 CodeMirror；退出恢复源码', async () => {
    const { container, unmount } = renderViewer({ diff: makeMdDiff({}) });
    await until(() => editorViews(container).length === 1);

    clickPreview(container);
    expect(toolbarButton(container, '预览')!.getAttribute('aria-pressed')).toBe('true');
    expect(container.querySelectorAll('.cm-editor')).toHaveLength(0); // 不创建 CodeMirror 实例
    const panes = container.querySelectorAll('.md-diff-pane');
    expect(panes).toHaveLength(2);
    expect(panes[0].querySelector('.md-diff-pane-label')!.textContent).toBe('旧侧');
    expect(panes[1].querySelector('.md-diff-pane-label')!.textContent).toBe('新侧');
    expect(container.querySelectorAll('.md-diff-pane .md-preview')).toHaveLength(2);
    // GFM 内容真实渲染（新侧表格）
    expect(container.querySelectorAll('.md-diff-pane table')).toHaveLength(1);

    clickPreview(container);
    expect(toolbarButton(container, '预览')!.getAttribute('aria-pressed')).toBe('false');
    await until(() => editorViews(container).length === 1);
    expect(container.querySelectorAll('.md-diff')).toHaveLength(0);
    unmount();
  });

  it('单侧存在：纯新增仅渲染新侧、纯删除仅渲染旧侧，各占满宽度', async () => {
    const add = renderViewer({
      diff: makeMdDiff({ oldExists: false, oldContent: '', oldMode: '' }),
    });
    await until(() => editorViews(add.container).length === 1);
    clickPreview(add.container);
    const addPanes = add.container.querySelectorAll('.md-diff-pane');
    expect(addPanes).toHaveLength(1);
    expect(addPanes[0].querySelector('.md-diff-pane-label')!.textContent).toBe('新侧');
    add.unmount();

    const del = renderViewer({
      diff: makeMdDiff({ newExists: false, newContent: '', newMode: '' }),
    });
    await until(() => editorViews(del.container).length === 1);
    clickPreview(del.container);
    const delPanes = del.container.querySelectorAll('.md-diff-pane');
    expect(delPanes).toHaveLength(1);
    expect(delPanes[0].querySelector('.md-diff-pane-label')!.textContent).toBe('旧侧');
    del.unmount();
  });

  it('预览态工具栏仅保留「预览」开关（真实 checking 态被隐藏）；退出后单列/并排/换行与资格提示恢复', async () => {
    // editIO 存在且资格请求在途 → 源码模式「检查中…」真实出现（非平凡断言）
    const pending: Array<{ res: (r: FileEditRead) => void; rej: (e: Error) => void }> = [];
    const editIO = {
      read: vi.fn(() => new Promise<FileEditRead>((res, rej) => pending.push({ res, rej }))),
      write: vi.fn(),
    };
    const { container, unmount } = renderViewer({
      diff: makeMdDiff({}),
      modeOverride: 'unified',
      wrapOverride: true,
      editIO,
    });
    await until(() => editorViews(container).length === 1);
    await until(() => toolbarButton(container, '检查中…') !== null);
    expect(toolbarButton(container, '单列')).not.toBeNull();
    expect(toolbarButton(container, '换行')).not.toBeNull();

    clickPreview(container);
    const toolbarButtons = [
      ...container.querySelectorAll<HTMLButtonElement>('.diff-toolbar button'),
    ].map((b) => b.textContent?.trim());
    expect(toolbarButtons).toEqual(['预览']);
    expect(container.textContent).not.toContain('检查中');

    clickPreview(container);
    await until(() => editorViews(container).length === 1);
    // 退出预览：资格仍在途 → 「检查中…」按当前资格恢复显示；形态与换行偏好按进入前状态恢复（单列 + 折行）
    await until(() => toolbarButton(container, '检查中…') !== null);
    expect(editorViews(container)).toHaveLength(1);
    expect(container.querySelectorAll('.cm-content.cm-lineWrapping')).toHaveLength(1);
    expect(toolbarButton(container, '单列')).not.toBeNull();
    unmount();
  });

  it('动态失效：预览中刷新为 truncated → 立即退出预览；资格恢复不自动重进', async () => {
    const valid = makeMdDiff({});
    const mounted = renderViewer({ diff: valid });
    await until(() => editorViews(mounted.container).length === 1);
    clickPreview(mounted.container);
    expect(mounted.container.querySelectorAll('.md-diff-pane')).toHaveLength(2);

    // 同一视图内刷新：truncated=true → 资格失效，立即按源码渲染
    rerender(
      mounted.root,
      <DiffViewer
        diff={makeMdDiff({ truncated: true, oldContent: 'a\n', newContent: 'b\n' })}
        path="a.md"
        modeOverride="unified"
        onModeChange={mounted.onModeChange}
        wrapOverride={false}
        onWrapChange={mounted.onWrapChange}
      />,
    );
    await until(() => editorViews(mounted.container).length === 1);
    expect(mounted.container.querySelectorAll('.md-diff')).toHaveLength(0);
    expect(toolbarButton(mounted.container, '预览')).toBeNull();
    expect(mounted.container.textContent).toContain('内容过大');

    // 刷新恢复资格：开关回来，但 MUST NOT 自动重进预览
    rerender(
      mounted.root,
      <DiffViewer
        diff={valid}
        path="a.md"
        modeOverride="unified"
        onModeChange={mounted.onModeChange}
        wrapOverride={false}
        onWrapChange={mounted.onWrapChange}
      />,
    );
    await until(() => toolbarButton(mounted.container, '预览') !== null);
    expect(mounted.container.querySelectorAll('.md-diff')).toHaveLength(0);
    expect(toolbarButton(mounted.container, '预览')!.getAttribute('aria-pressed')).toBe('false');
    mounted.unmount();
  });

  it('预览中权限/类型变更横幅照常显示', async () => {
    const { container, unmount } = renderViewer({
      diff: makeMdDiff({ newMode: '100755' }),
    });
    await until(() => editorViews(container).length === 1);
    expect(container.textContent).toContain('权限/类型变更');
    clickPreview(container);
    expect(container.querySelectorAll('.md-diff-pane')).toHaveLength(2);
    expect(container.textContent).toContain('权限/类型变更');
    unmount();
  });

  it('源码模式不因 markdown 内容 URL 挂载任何图片/链接；进入预览后才出现', async () => {
    const { container, unmount } = renderViewer({
      diff: makeMdDiff({
        newContent:
          '![图](https://cdn.example/i.png)\n\n[链接](https://example.com)\n\n<video src="https://evil.example/v.mp4"></video>\n',
      }),
    });
    await until(() => editorViews(container).length === 1);
    // 源码模式：无 img/无 a（URL 不产生任何资源交付载体）
    expect(container.querySelectorAll('img')).toHaveLength(0);
    expect(container.querySelectorAll('a')).toHaveLength(0);

    clickPreview(container);
    const img = container.querySelector('.md-diff-pane img');
    expect(img).not.toBeNull();
    expect(img!.getAttribute('loading')).toBe('lazy');
    const a = container.querySelector('.md-diff-pane a');
    expect(a!.getAttribute('target')).toBe('_blank');
    expect(a!.getAttribute('rel')).toBe('noopener noreferrer');
    // 原始 HTML 仍不渲染
    expect(container.querySelectorAll('video')).toHaveLength(0);
    unmount();
  });

  it('图片 onError 仅降级为占位，不触发逐侧渲染失败提示（D6：网络失败≠渲染失败）', async () => {
    const { container, unmount } = renderViewer({
      diff: makeMdDiff({ newContent: '![图](https://cdn.example/i.png)\n' }),
    });
    await until(() => editorViews(container).length === 1);
    clickPreview(container);
    const img = container.querySelector('.md-diff-pane img');
    expect(img).not.toBeNull();
    act(() => {
      img!.dispatchEvent(new Event('error'));
    });
    expect(container.querySelectorAll('.md-preview-img-fallback')).toHaveLength(1);
    expect(container.querySelector('.md-preview-failed')).toBeNull();
    expect(container.textContent).not.toContain('渲染失败');
    unmount();
  });

  it('仅新侧内容变化：旧侧图片失败状态不重置（不重挂、不重新发起外部请求）', async () => {
    const oldContent = '![旧图](https://cdn.example/old.png)\n\n旧侧文本\n';
    const mounted = renderViewer({
      diff: makeMdDiff({ oldContent, newContent: '新侧初版\n' }),
    });
    await until(() => editorViews(mounted.container).length === 1);
    clickPreview(mounted.container);
    const img = mounted.container.querySelector('.md-diff-pane img');
    expect(img).not.toBeNull();
    act(() => {
      img!.dispatchEvent(new Event('error'));
    });
    expect(mounted.container.querySelectorAll('.md-preview-img-fallback')).toHaveLength(1);

    // 仅新侧内容变化：旧侧 MarkdownPreview 不重挂（Gate B F1：reset identity 逐侧收敛），
    // 失败登记保留 → 占位保持，不得重新出现 img 发起外部请求
    rerender(
      mounted.root,
      <DiffViewer
        diff={makeMdDiff({ oldContent, newContent: '新侧更新\n' })}
        path="a.md"
        modeOverride="unified"
        onModeChange={mounted.onModeChange}
        wrapOverride={false}
        onWrapChange={mounted.onWrapChange}
      />,
    );
    await until(() => mounted.container.textContent?.includes('新侧更新') ?? false);
    expect(mounted.container.querySelectorAll('.md-preview-img-fallback')).toHaveLength(1);
    expect(mounted.container.querySelectorAll('.md-diff-pane img')).toHaveLength(0);
    mounted.unmount();
  });

  it('D7 smoke：双侧 2×524288 字节输入进入预览渲染无异常', async () => {
    const big = '# 大文件\n\n' + 'lorem ipsum '.repeat(43691);
    const { container, unmount } = renderViewer({
      diff: makeMdDiff({ oldContent: big, newContent: `${big}\n尾行\n` }),
    });
    await until(() => editorViews(container).length === 1);
    clickPreview(container);
    await until(() => container.querySelectorAll('.md-diff-pane .md-preview').length === 2);
    expect(container.querySelectorAll('.md-preview-failed')).toHaveLength(0);
    unmount();
  }, 30000);
});

describe('预览与编辑/批注互斥（D4，tasks 4.4）', () => {
  beforeEach(() => stubMatchMedia(false));

  it('预览中资格预取完成（eligible）不进入编辑；退出预览后偏好仍成立自动进入', async () => {
    let resolveRead!: (r: FileEditRead) => void;
    const editIO = {
      read: vi.fn(
        () =>
          new Promise<FileEditRead>((res) => {
            resolveRead = res;
          }),
      ),
      write: vi.fn().mockResolvedValue({ baseHash: 'h1' }),
    };
    const { container, unmount } = renderViewer({
      diff: makeMdDiff({}),
      editIO,
      editModePreferred: true,
    });
    await until(() => editorViews(container).length === 1);

    // 资格在途时进入预览
    clickPreview(container);
    expect(container.querySelectorAll('.md-diff-pane')).toHaveLength(2);

    // 预览期间资格请求完成且结果为可编辑 → 保持预览，不进入编辑
    await act(async () => {
      resolveRead(editableRead);
    });
    await flushUI();
    expect(container.querySelectorAll('.md-diff-pane')).toHaveLength(2);
    expect(container.textContent).not.toContain('退出编辑');

    // 退出预览：偏好与资格仍成立 → 自动进入编辑恢复
    clickPreview(container);
    await until(() =>
      [...container.querySelectorAll<HTMLButtonElement>('button')].some(
        (b) => b.textContent === '退出编辑',
      ),
    );
    unmount();
  });

  it('预览中 eligible → 刷新 truncated：不进入编辑、资格作废；恢复后重新预取且旧基线不被复用', async () => {
    // 完整竞态：editModePreferred=true + deferred read——进入预览后资格请求才完成
    const pending: Array<{ res: (r: FileEditRead) => void; rej: (e: Error) => void }> = [];
    const editIO = {
      read: vi.fn(() => new Promise<FileEditRead>((res, rej) => pending.push({ res, rej }))),
      write: vi.fn().mockResolvedValue({ baseHash: 'h1' }),
    };
    const resolveEligible = (content: string) => {
      const p = pending.shift()!;
      act(() => {
        p.res({
          editable: true,
          content,
          baseHash: `h-${content}`,
          lineEnding: 'lf',
          hasBom: false,
          mode: '0644',
        });
      });
    };

    const mounted = renderViewer({
      diff: makeMdDiff({}),
      editIO,
      editModePreferred: true,
    });
    await until(() => editorViews(mounted.container).length === 1);

    // 资格在途时进入预览；资格于预览中完成（eligible）→ 保持预览，不进入编辑
    clickPreview(mounted.container);
    expect(mounted.container.querySelectorAll('.md-diff-pane')).toHaveLength(2);
    resolveEligible('基线v1\n');
    await flushUI();
    expect(mounted.container.querySelectorAll('.md-diff-pane')).toHaveLength(2);
    expect(mounted.container.textContent).not.toContain('退出编辑');

    // 同 key 刷新为 truncated：立即退出预览；不进入编辑；门禁不成立 → 编辑入口禁用
    rerender(
      mounted.root,
      <DiffViewer
        diff={makeMdDiff({ truncated: true, oldContent: 'a\n', newContent: 'b\n' })}
        path="a.md"
        editIO={editIO}
        editModePreferred={true}
        modeOverride="unified"
        onModeChange={mounted.onModeChange}
        wrapOverride={false}
        onWrapChange={mounted.onWrapChange}
      />,
    );
    await until(() => editorViews(mounted.container).length === 1);
    expect(mounted.container.querySelectorAll('.md-diff')).toHaveLength(0);
    expect(mounted.container.textContent).not.toContain('退出编辑');
    const editBtn = toolbarButton(mounted.container, '编辑');
    expect(editBtn).not.toBeNull();
    expect(editBtn!.disabled).toBe(true);

    // 恢复合法 diff：作废后的资格必须重新读取（读取次数增加），MUST NOT 沿用旧缓存
    const readsBefore = editIO.read.mock.calls.length;
    rerender(
      mounted.root,
      <DiffViewer
        diff={makeMdDiff({})}
        path="a.md"
        editIO={editIO}
        editModePreferred={true}
        modeOverride="unified"
        onModeChange={mounted.onModeChange}
        wrapOverride={false}
        onWrapChange={mounted.onWrapChange}
      />,
    );
    await until(() => editIO.read.mock.calls.length > readsBefore);

    // 新预取完成且可编辑 → 偏好驱动自动进入编辑；编辑基线 = 新读取内容（旧基线 v1 未被复用）
    resolveEligible('基线v2\n');
    await until(() =>
      [...mounted.container.querySelectorAll<HTMLButtonElement>('button')].some(
        (b) => b.textContent === '退出编辑',
      ),
    );
    expect(editorViews(mounted.container)[0].state.doc.toString()).toBe('基线v2\n');
    mounted.unmount();
  });

  it('diff 刷新使门禁失效时作废缓存资格：denied/error 提示不残留', async () => {
    const truncated = makeMdDiff({ truncated: true, oldContent: 'a\n', newContent: 'b\n' });

    // denied 态显示原因条 → 刷新 truncated（作废缓存资格）→ 原因条不再残留
    const deniedIO = {
      read: vi
        .fn()
        .mockResolvedValue({ editable: false, reasonCode: 'read_only', reason: '文件只读' }),
      write: vi.fn(),
    };
    const denied = renderViewer({ diff: makeMdDiff({}), editIO: deniedIO });
    await until(() => denied.container.textContent?.includes('不可编辑：文件只读') ?? false);
    rerender(
      denied.root,
      <DiffViewer
        diff={truncated}
        path="a.md"
        editIO={deniedIO}
        modeOverride="unified"
        onModeChange={denied.onModeChange}
        wrapOverride={false}
        onWrapChange={denied.onWrapChange}
      />,
    );
    await until(() => editorViews(denied.container).length === 1);
    expect(denied.container.textContent).not.toContain('文件只读');
    denied.unmount();

    // error 态显示进入编辑失败条 → 刷新 truncated → 作废后不再残留
    const errorIO = { read: vi.fn().mockRejectedValue(new Error('网络中断')), write: vi.fn() };
    const errorView = renderViewer({ diff: makeMdDiff({}), editIO: errorIO });
    await until(() => errorView.container.textContent?.includes('进入编辑失败') ?? false);
    rerender(
      errorView.root,
      <DiffViewer
        diff={truncated}
        path="a.md"
        editIO={errorIO}
        modeOverride="unified"
        onModeChange={errorView.onModeChange}
        wrapOverride={false}
        onWrapChange={errorView.onWrapChange}
      />,
    );
    await until(() => editorViews(errorView.container).length === 1);
    expect(errorView.container.textContent).not.toContain('进入编辑失败');
    expect(errorView.container.textContent).not.toContain('网络中断');
    errorView.unmount();
  });

  it('预览按资格态隐藏并恢复：eligible/denied/error 于预览中完成后，退出按当前资格显示', async () => {
    const pending: Array<{ res: (r: FileEditRead) => void; rej: (e: Error) => void }> = [];
    const editIO = {
      read: vi.fn(() => new Promise<FileEditRead>((res, rej) => pending.push({ res, rej }))),
      write: vi.fn(),
    };
    const mounted = renderViewer({ diff: makeMdDiff({}), editIO });
    await until(() => editorViews(mounted.container).length === 1);
    await until(() => toolbarButton(mounted.container, '检查中…') !== null);

    // eligible：预览中资格完成 → 预览不显示编辑入口，退出后按当前资格恢复
    clickPreview(mounted.container);
    act(() => {
      pending.shift()!.res({ ...editableRead, content: '资格内容\n' });
    });
    expect(mounted.container.textContent).not.toContain('检查中');
    expect(toolbarButton(mounted.container, '编辑')).toBeNull();
    clickPreview(mounted.container);
    await until(() => {
      const b = toolbarButton(mounted.container, '编辑');
      return b !== null && !b.disabled;
    });

    // denied：预览中完成 → 预览不显示原因条，退出后恢复
    rerender(
      mounted.root,
      <DiffViewer
        diff={makeMdDiff({})}
        path="a.md"
        editIO={editIO}
        modeOverride="unified"
        onModeChange={mounted.onModeChange}
        wrapOverride={false}
        onWrapChange={mounted.onWrapChange}
      />,
    );
    await until(() => pending.length > 0);
    clickPreview(mounted.container);
    act(() => {
      pending.shift()!.res({ editable: false, reasonCode: 'read_only', reason: '文件只读' });
    });
    expect(mounted.container.textContent).not.toContain('文件只读');
    clickPreview(mounted.container);
    await until(() => mounted.container.textContent?.includes('不可编辑：文件只读') ?? false);

    // error：预览中完成 → 预览不显示重试入口与失败条，退出后恢复
    rerender(
      mounted.root,
      <DiffViewer
        diff={makeMdDiff({})}
        path="a.md"
        editIO={editIO}
        modeOverride="unified"
        onModeChange={mounted.onModeChange}
        wrapOverride={false}
        onWrapChange={mounted.onWrapChange}
      />,
    );
    await until(() => pending.length > 0);
    clickPreview(mounted.container);
    act(() => {
      pending.shift()!.rej(new Error('网络中断'));
    });
    expect(mounted.container.textContent).not.toContain('进入编辑失败');
    expect(toolbarButton(mounted.container, '重试')).toBeNull();
    clickPreview(mounted.container);
    await until(() => toolbarButton(mounted.container, '重试') !== null);
    expect(mounted.container.textContent).toContain('进入编辑失败');
    mounted.unmount();
  });

  it('进入预览清除批注瞬态 UI（草稿），退出后不恢复', async () => {
    const { container, unmount } = renderViewer({ diff: makeMdDiff({}) });
    await until(() => editorViews(container).length === 1);
    const view = editorViews(container)[0];

    // 源码模式手势打开内联批注草稿（选区须为非空行，空行是点击不是拖选）
    act(() => {
      view.dispatch({
        selection: { anchor: view.state.doc.line(1).from, head: view.state.doc.line(1).to },
      });
      container
        .querySelectorAll('.cm-line')[0]
        .dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(container.querySelector('.ann-inline')).toBeTruthy();

    // 进入预览：草稿关闭、瞬态 UI 消失
    clickPreview(container);
    expect(container.querySelector('.ann-inline')).toBeNull();
    expect(container.querySelectorAll('.md-diff-pane')).toHaveLength(2);

    // 退出预览：草稿已被清除，不恢复
    clickPreview(container);
    await until(() => editorViews(container).length === 1);
    expect(container.querySelector('.ann-inline')).toBeNull();
    unmount();
  });

  it('进入预览清除跨侧选区提示（真实跨侧选择路径），退出后不恢复', async () => {
    const { container, unmount } = renderViewer({
      diff: makeMdDiff({}),
      modeOverride: 'side-by-side',
    });
    await until(() => editorViews(container).length === 2);
    await until(() => container.querySelector('.cm-merge-a .cm-line') !== null);

    // F15 同款真实路径：A 侧按下、B 侧释放 → 跨侧选区提示
    const aLine = container.querySelector('.cm-merge-a .cm-line')!;
    const bLine = container.querySelector('.cm-merge-b .cm-line')!;
    act(() => {
      aLine.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
      bLine.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    expect(container.textContent).toContain('选区跨越两侧');

    // 进入预览：提示清除（togglePreview 执行 setCrossSideHint('')）
    clickPreview(container);
    expect(container.textContent).not.toContain('选区跨越两侧');
    expect(container.querySelectorAll('.md-diff-pane')).toHaveLength(2);

    // 退出预览：提示已被清除，不恢复
    clickPreview(container);
    await until(() => editorViews(container).length === 2);
    expect(container.textContent).not.toContain('选区跨越两侧');
    unmount();
  });
});

describe('GitPanel 集成：预览选择不跨文件保留（D3）', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    stubMatchMedia(false); // 宽屏默认并排
    const files: GitFileEntry[] = [
      {
        path: 'doc/a.md',
        x: 'M',
        y: ' ',
        staged: false,
        unstaged: true,
        untracked: false,
        additions: 1,
        deletions: 1,
        isBinary: false,
      },
      {
        path: 'doc/b.md',
        x: 'M',
        y: ' ',
        staged: false,
        unstaged: true,
        untracked: false,
        additions: 1,
        deletions: 1,
        isBinary: false,
      },
    ];
    gitStatusMock.mockResolvedValue({ branch: 'main', files });
    gitDiffMock.mockImplementation((_taskID, _ref, path) => {
      const content =
        path === 'doc/a.md'
          ? makeMdDiff({})
          : makeMdDiff({ oldContent: 'b 旧\n', newContent: 'b 新\n' });
      return Promise.resolve(content);
    });
    vi.mocked(api.gitFileRead).mockResolvedValue({
      editable: false,
      reasonCode: 'read_only',
      reason: '锁定',
    } as FileEditRead);
  });

  it('a.md 进入预览后切换 b.md → 回到源码模式；切回 a.md 仍为源码', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => container.querySelectorAll('.git-file-path').length === 2);

    // F4：交互与 async diff 加载同轮 flush 在 act 内，避免未包裹 act 的状态更新警告
    const clickFile = async (path: string) => {
      const el = [...container.querySelectorAll('.git-file-path')].find((n) =>
        n.textContent?.includes(path),
      );
      expect(el, `file entry ${path}`).toBeTruthy();
      await act(async () => {
        el!.dispatchEvent(new MouseEvent('click', { bubbles: true }));
        await Promise.resolve();
      });
    };

    await clickFile('doc/a.md');
    await until(() => editorViews(container).length === 2);
    clickPreview(container);
    await until(() => container.querySelectorAll('.md-diff-pane').length === 2);

    // 切换文件：三元组 key 重挂载 → 源码模式（预览选择不跨文件保留）
    await clickFile('doc/b.md');
    await until(() => editorViews(container).length === 2);
    expect(container.querySelectorAll('.md-diff')).toHaveLength(0);
    expect(container.textContent).toContain('b 旧');

    // 切回 a.md：仍为源码模式
    await clickFile('doc/a.md');
    await until(() => editorViews(container).length === 2);
    expect(container.querySelectorAll('.md-diff')).toHaveLength(0);
    expect(toolbarButton(container, '预览')!.getAttribute('aria-pressed')).toBe('false');
    unmount();
  });

  it('切换文件丢弃未提交的块级草稿（重挂载，不产生记录）', async () => {
    const { container, unmount } = mount(<GitPanel taskID="t1" active />);
    await until(() => container.querySelectorAll('.git-file-path').length === 2);

    const clickFile = async (path: string) => {
      const el = [...container.querySelectorAll('.git-file-path')].find((n) =>
        n.textContent?.includes(path),
      );
      expect(el, `file entry ${path}`).toBeTruthy();
      await act(async () => {
        el!.dispatchEvent(new MouseEvent('click', { bubbles: true }));
        await Promise.resolve();
      });
    };

    await clickFile('doc/a.md');
    await until(() => editorViews(container).length === 2);
    clickPreview(container);
    await until(() => container.querySelectorAll('.md-diff-pane').length === 2);

    // 打开块级草稿
    const entries = container.querySelectorAll<HTMLButtonElement>('.md-block-annotate');
    expect(entries.length).toBeGreaterThanOrEqual(2);
    act(() => entries[1].click());
    expect(container.querySelector('.md-block-draft-box')).not.toBeNull();

    // 切换文件：草稿被丢弃（重挂载），新视图无任何草稿残留
    await clickFile('doc/b.md');
    await until(() => editorViews(container).length === 2);
    expect(container.querySelectorAll('.md-diff')).toHaveLength(0);
    expect(container.querySelector('.md-block-draft-box')).toBeNull();

    // 切回 a.md（源码模式重挂载）：草稿不恢复
    await clickFile('doc/a.md');
    await until(() => editorViews(container).length === 2);
    expect(container.querySelector('.md-block-draft-box')).toBeNull();
    unmount();
  });
});

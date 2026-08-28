import { useEffect, useRef } from 'react';
import { Text, type Extension } from '@codemirror/state';
import { EditorView } from '@codemirror/view';
import { MergeView, unifiedMergeView } from '@codemirror/merge';
import { editorTheme, readOnlyExtensions } from '../editor/extensions';
import { loadLanguage } from '../editor/language';
import type { GitDiffResult } from '../../types';

export type DiffViewMode = 'unified' | 'side-by-side';

interface DiffViewerProps {
  diff: GitDiffResult;
  /** 用于语法高亮语言选择的文件路径。 */
  path: string;
  /** 用户形态选择；null 时按视口默认（>1024px 并排、≤1024px 单列，design D6/DR1）。 */
  modeOverride: DiffViewMode | null;
  onModeChange: (mode: DiffViewMode) => void;
  /** 换行开关（design D6 换行开关 bullet）：false=横向滚动（默认），true=自动折行。
   *  与 modeOverride 同生命周期：跨文件切换与视口变化保留、GitPanel 卸载丢弃。 */
  wrapOverride: boolean;
  onWrapChange: (wrap: boolean) => void;
}

/** merge 视图共享配置（design D6）。timeout 为显式配置——merge 默认仅 scanLimit，无超时。 */
const collapseUnchanged = { margin: 3, minSize: 4 };
const diffConfig = { scanLimit: 500, timeout: 500 };
/** git mode 常量（spec「文件 diff 查看」）：120000 symlink、160000 gitlink。 */
const MODE_GITLINK = '160000';

type DiffState =
  | { kind: 'gitlink' }
  | { kind: 'message'; text: string }
  | { kind: 'merge' };

/**
 * 渲染优先级唯一链（spec「文件 diff 查看」派生规则段，I1 修订）：
 * isBinary → gitlink（任一侧 mode=160000，不渲染 merge、不落入不存在/无变更）→ 双侧不存在
 * → 空文件（至少一侧存在 + 内容均空 + mode 相同）→ 无变更（两侧均存在 + truncated=false + 内容与 mode 均相同）
 * → 截断范围内无可见差异 → merge 视图。
 * mode 相同的比较仅在两侧均存在时参与（缺失侧 mode 恒为空串；与 mode 变更横幅「两侧均存在」前提一致，
 * spec scenario「查看空的新文件」「内容为空的已删除文件」一侧缺失仍展示空文件状态）。
 */
function deriveDiffState(diff: GitDiffResult): DiffState {
  if (diff.isBinary) return { kind: 'message', text: '二进制文件，暂不支持查看 diff。' };
  if (diff.oldMode === MODE_GITLINK || diff.newMode === MODE_GITLINK) return { kind: 'gitlink' };
  if (!diff.oldExists && !diff.newExists) return { kind: 'message', text: '文件已不存在。' };
  const modeSame = !diff.oldExists || !diff.newExists || diff.oldMode === diff.newMode;
  if ((diff.oldExists || diff.newExists) && diff.oldContent === '' && diff.newContent === '' && modeSame) {
    return { kind: 'message', text: '空文件。' };
  }
  if (diff.oldExists && diff.newExists && diff.oldContent === diff.newContent) {
    if (diff.truncated) return { kind: 'message', text: '截断范围内无可见差异。' };
    if (modeSame) return { kind: 'message', text: '内容无变化。' };
    // 内容相同但 mode 不同（chmod-only）：不落「无变更」，走 merge 视图 + mode 变更横幅
  }
  return { kind: 'merge' };
}

/** 单文件 diff 渲染（codemirror-git-diff design D6）：并排 MergeView / 单列 unifiedMergeView。 */
export default function DiffViewer({
  diff,
  path,
  modeOverride,
  onModeChange,
  wrapOverride,
  onWrapChange,
}: DiffViewerProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const state = deriveDiffState(diff);
  const mode: DiffViewMode =
    modeOverride ??
    (window.matchMedia('(max-width: 1024px)').matches ? 'unified' : 'side-by-side');
  // mode 变更横幅独立叠加（spec：两侧均存在且 mode 不同；可与 merge 视图或状态提示同时出现）
  const modeChanged = diff.oldExists && diff.newExists && diff.oldMode !== diff.newMode;

  useEffect(() => {
    if (state.kind !== 'merge' || !containerRef.current) return;
    const container = containerRef.current;
    let destroyed = false;
    let view: MergeView | EditorView | null = null;
    void (async () => {
      const lang = await loadLanguage(path);
      if (destroyed) return;
      const extensions: Extension[] = [
        ...readOnlyExtensions,
        editorTheme,
        ...(wrapOverride ? [EditorView.lineWrapping] : []),
        ...(lang ? [lang] : []),
      ];
      view =
        mode === 'side-by-side'
          ? // a/b 传 EditorStateConfig（构造器自行创建 state，传预建 state 会丢 extensions）；
            // revertControls 省略即无 revert 控件。
            new MergeView({
              parent: container,
              a: { doc: diff.oldContent, extensions },
              b: { doc: diff.newContent, extensions },
              highlightChanges: true,
              gutter: true,
              collapseUnchanged,
              diffConfig,
            })
          : new EditorView({
              parent: container,
              doc: diff.newContent,
              extensions: [
                ...extensions,
                unifiedMergeView({
                  // Text.of 保留旧侧行尾字符；string original 会被 merge 规范化，吞掉 CRLF（design D6）。
                  original: Text.of(diff.oldContent.split('\n')),
                  mergeControls: false,
                  collapseUnchanged,
                  diffConfig,
                }),
              ],
            });
    })();
    return () => {
      destroyed = true;
      view?.destroy();
    };
    // state 为每次 render 新建对象，deps 取 kind 原始值避免无关 rerender 重建编辑器；
    // wrapOverride 变化与形态切换走同一销毁-重建路径（destroy 后重建）
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [state.kind, mode, wrapOverride, diff.oldContent, diff.newContent, path]);

  return (
    <div className="diff-view">
      {modeChanged && (
        <div className="alert-bar alert-info">
          权限/类型变更：{diff.oldMode} → {diff.newMode}
        </div>
      )}
      {diff.truncated && (
        <div className="alert-bar alert-notice">内容过大，已被服务端截断。</div>
      )}
      {state.kind === 'gitlink' && (
        <div className="empty">
          子模块（gitlink）变更，不支持内容 diff。
          <div className="diff-gitlink mono">
            <span>{diff.oldContent || '（空）'}</span>
            <span>→</span>
            <span>{diff.newContent || '（空）'}</span>
          </div>
        </div>
      )}
      {state.kind === 'message' && <div className="empty">{state.text}</div>}
      {state.kind === 'merge' && (
        <>
          <div className="diff-toolbar">
            <button
              type="button"
              className="btn btn-small btn-ghost"
              aria-pressed={mode === 'unified'}
              onClick={() => onModeChange('unified')}
            >
              单列
            </button>
            <button
              type="button"
              className="btn btn-small btn-ghost"
              aria-pressed={mode === 'side-by-side'}
              onClick={() => onModeChange('side-by-side')}
            >
              并排
            </button>
            <button
              type="button"
              className="btn btn-small btn-ghost"
              aria-pressed={wrapOverride}
              onClick={() => onWrapChange(!wrapOverride)}
            >
              换行
            </button>
          </div>
          <div ref={containerRef} className="diff-editor" />
        </>
      )}
    </div>
  );
}

import {
  createContext,
  memo,
  useContext,
  useEffect,
  useMemo,
  useState,
  createElement,
} from 'react';
import type { ComponentPropsWithoutRef, HTMLAttributes, ReactNode } from 'react';
import { isValidElement } from 'react';
import Markdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import type { Annotation } from '../../types';
import { sortAnnotations } from './review-utils';

/* ============================ markdown 渲染预览（markdown-diff-preview design D1/D5/D8） ============================
 * 受控组件：仅按 content 渲染 GFM，不感知 diff 上下文（path/ref/truncated 由 owner 侧处理）；
 * 不自定义 urlTransform，URL 策略由自定义 a/img 组件对渲染交付的最终 href/src 值判定（design D1）。
 * D8 块级批注：side/annotations/draft + 四个回调组成受控接口；批注数据与草稿状态的 owner 是 DiffViewer，
 * 本组件只负责行号映射（唯一口径见 lineAt/blockRange）、块入口/徽章/草稿 UI 的受控渲染。 */

/** URL 判定层（design D1 契约句）：以无 base 的 URL 解析判定协议，仅 http/https 通过；
 * URL 缺失、为空或解析抛异常判为不合格，不向外抛错。 */
function isQualifiedUrl(url: string | undefined): url is string {
  if (!url) return false;
  try {
    const protocol = new URL(url).protocol;
    return protocol === 'http:' || protocol === 'https:';
  } catch {
    return false;
  }
}

/** 不合格链接的后代树降级：递归铺平为纯文本等价物，其中图片仅保留 alt（design D1）。 */
function flattenToText(node: ReactNode): string {
  if (node == null || typeof node === 'boolean') return '';
  if (typeof node === 'string' || typeof node === 'number') return String(node);
  if (Array.isArray(node)) return node.map(flattenToText).join('');
  if (isValidElement(node)) {
    const props = node.props as { alt?: string; children?: ReactNode };
    if (typeof props.alt === 'string') return props.alt;
    return flattenToText(props.children);
  }
  return '';
}

/** 不合格/加载失败图片的统一占位：固定样式并显示 alt，不保留 img/src（design D1）。 */
function ImageFallback({ alt }: { alt?: string }) {
  return <span className="md-preview-img-fallback">{alt}</span>;
}

/** 图片加载失败登记：状态按最终 src 隔离。经 Context 下发，保证 SafeLink/SafeImage
 * 是稳定组件定义，不因失败集合变化导致 img 子树整体重挂载。 */
interface ImageFailureRegistry {
  failed: ReadonlySet<string>;
  onError: (src: string) => void;
}

const noopRegistry: ImageFailureRegistry = { failed: new Set<string>(), onError: () => {} };
const ImageFailureContext = createContext<ImageFailureRegistry>(noopRegistry);

/** 合格链接：新标签页安全打开；不合格链接整个后代树降级纯文本等价物（design D1）。 */
function SafeLink({ href, title, children }: ComponentPropsWithoutRef<'a'>) {
  if (!isQualifiedUrl(href)) {
    return <span>{flattenToText(children)}</span>;
  }
  return (
    <a href={href} title={title} target="_blank" rel="noopener noreferrer">
      {children}
    </a>
  );
}

/** 合格图片：lazy 加载；加载失败按最终 src 登记后降级占位，src 或内容变化后允许重新加载（design D1）。 */
function SafeImage({ src, alt, title }: ComponentPropsWithoutRef<'img'>) {
  const registry = useContext(ImageFailureContext);
  if (!isQualifiedUrl(src) || registry.failed.has(src)) {
    return <ImageFallback alt={alt} />;
  }
  return (
    <img
      src={src}
      alt={alt}
      title={title}
      loading="lazy"
      onError={() => registry.onError(src)}
    />
  );
}

/* ============================ D8 块级批注：行号映射（唯一口径） ============================ */

/** hast 位置/节点最小结构（@types/hast 是 react-markdown 的传递依赖，不直接 import）。 */
interface HastPosition {
  start?: { offset?: number | null } | undefined;
  end?: { offset?: number | null } | undefined;
}

interface HastNode {
  type?: string | undefined;
  tagName?: string | undefined;
  position?: HastPosition | undefined;
  children?: HastNode[] | undefined;
}

export interface BlockLineRange {
  startLine: number;
  endLine: number;
}

/** D8 行模型唯一口径：lineAt(o) = 1 + count('\n', content.slice(0, o))。
 *  offset 为 0-based UTF-16 code-unit，直接传 slice、不得加减一；行模型仅以 \n 分行（\r 是文档字符），
 *  与源码行级批注同一行模型，MUST NOT 使用解析器行号。
 *  本实现供单测对拍与语义文档；组件内走 buildLineIndex + lineAtByIndex（F1，避免每块 slice 退化 O(N²)）。 */
export function lineAt(content: string, offset: number): number {
  return content.slice(0, offset).split('\n').length;
}

/** F6：换行 offset 有序索引——生产路径与测试共用的纯实现（每 content 构造一次 O(N)）。 */
export type LineIndex = readonly number[];

export function buildLineIndex(content: string): LineIndex {
  const offsets: number[] = [];
  for (let i = 0; i < content.length; i++) {
    if (content.charCodeAt(i) === 10) offsets.push(i);
  }
  return offsets;
}

/** 索引版行映射：二分统计 < offset 的换行数，复杂度与文档长度无关（O(log N)）。 */
export function lineAtByIndex(index: LineIndex, offset: number): number {
  return 1 + bisectCount(index, offset);
}

/** blockRange 的核心公式（lineAt 由调用方提供：朴素实现或索引二分实现）。 */
function blockRangeAt(
  lineAtFn: (offset: number) => number,
  position: HastPosition | undefined,
): BlockLineRange | null {
  const start = position?.start?.offset;
  const end = position?.end?.offset;
  if (typeof start !== 'number' || typeof end !== 'number') return null;
  if (end <= start) return null;
  return {
    startLine: lineAtFn(start),
    endLine: lineAtFn(Math.max(start, end - 1)),
  };
}

/** D8 锚定映射（朴素包装，供单测对拍）：startLine = lineAt(start.offset)；position 的 end point
 *  排他 → endLine = lineAt(max(start.offset, end.offset - 1))；position/offset 缺失或零宽 → null。 */
export function blockRange(
  content: string,
  position: HastPosition | undefined,
): BlockLineRange | null {
  return blockRangeAt((o) => lineAt(content, o), position);
}

/** 索引版锚定映射（组件内生产路径）。 */
export function blockRangeByIndex(
  index: LineIndex,
  position: HastPosition | undefined,
): BlockLineRange | null {
  return blockRangeAt((o) => lineAtByIndex(index, o), position);
}

/** 二分统计有序数组中 < value 的元素个数（= value 所在行号 - 1）。 */
function bisectCount(sorted: readonly number[], value: number): number {
  let lo = 0;
  let hi = sorted.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (sorted[mid] < value) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

/** 可批注块集合（D8）：p、h1-h6、li、table（整块）、pre、blockquote；容器（ul/ol 等）与 hr 不可批注。 */
const ANNOTATABLE_TAGS = new Set([
  'p',
  'h1',
  'h2',
  'h3',
  'h4',
  'h5',
  'h6',
  'li',
  'table',
  'pre',
  'blockquote',
]);

type BlockTag =
  | 'p'
  | 'h1'
  | 'h2'
  | 'h3'
  | 'h4'
  | 'h5'
  | 'h6'
  | 'li'
  | 'table'
  | 'pre'
  | 'blockquote';

/** 内容模型仅 phrasing content 的可批注标签：其子代不得出现 div（F4，草稿态需外层包装）。 */
const PHRASING_ONLY_TAGS = new Set<BlockTag>(['p', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'pre']);

/** 块级批注 API：经 Context 下发给模块级块组件（组件实例稳定，不因状态变化重挂子树）。 */
interface BlockAnnotationApi {
  resolveRange: (position: HastPosition | undefined) => BlockLineRange | null;
  /** 精确范围匹配的同侧批注（key=`startLine:endLine`；三元组+侧别已由 owner/本组件过滤，(createdAt,id) 升序）。 */
  badges: ReadonlyMap<string, Annotation[]>;
  /** 当前块级草稿（owner 持有）；范围匹配的块进入草稿态。 */
  draft: { startLine: number; endLine: number; comment: string } | null;
  canAnnotate: boolean;
  onAnnotateBlock: (range: BlockLineRange) => void;
  onDraftCommentChange: (comment: string) => void;
  onSubmitDraft: () => void;
  onCancelDraft: () => void;
  onLocateAnnotations: (ids: string[]) => void;
}

const BlockAnnotationContext = createContext<BlockAnnotationApi | null>(null);

/** D8 嵌套归属：本块 range 与某个子孙可批注块的 range 相等 → 徽章让位给最内层（本块不显示）。 */
function hasSameRangeDescendant(
  api: BlockAnnotationApi,
  node: HastNode,
  range: BlockLineRange,
): boolean {
  for (const child of node.children ?? []) {
    if (ANNOTATABLE_TAGS.has(child.tagName ?? '')) {
      const r = api.resolveRange(child.position);
      if (r && r.startLine === range.startLine && r.endLine === range.endLine) return true;
    }
    if (hasSameRangeDescendant(api, child, range)) return true;
  }
  return false;
}

/** 聚合徽章（D8）：带数量、悬停按（createdAt、id）顺序显示评论摘要、点击在批注列表定位全部匹配条目；
 *  含任一已漂移批注时显示漂移标识。 */
function BlockBadge({
  annotations,
  onLocate,
}: {
  annotations: Annotation[];
  onLocate: (ids: string[]) => void;
}) {
  const stale = annotations.some((a) => a.stale);
  const locate = () => onLocate(annotations.map((a) => a.id));
  return (
    <span
      className={`md-block-badge${stale ? ' md-block-badge-stale' : ''}`}
      role="button"
      tabIndex={0}
      title={`${annotations.length} 条批注`}
      onClick={locate}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') locate();
      }}
    >
      {annotations.length}
      <span className="md-block-badge-tip">
        {annotations.map((a) => (
          <span key={a.id} className="md-block-badge-tip-item">
            {a.stale ? '（已漂移）' : ''}
            {a.comment}
          </span>
        ))}
      </span>
    </span>
  );
}

/** 块级草稿盒（受控）：评论输入状态由 owner 持有；Esc 关闭即丢弃，⌘/Ctrl+Enter 快速提交。 */
function BlockDraftBox({
  range,
  draft,
  onDraftCommentChange,
  onSubmitDraft,
  onCancelDraft,
}: {
  range: BlockLineRange;
  draft: { startLine: number; endLine: number; comment: string };
  onDraftCommentChange: (comment: string) => void;
  onSubmitDraft: () => void;
  onCancelDraft: () => void;
}) {
  return (
    <div
      className="md-block-draft-box"
      onKeyDown={(e) => {
        if (e.key === 'Escape') onCancelDraft();
      }}
    >
      <textarea
        className="input md-block-draft-input"
        autoFocus
        rows={3}
        placeholder="添加此更改的上下文"
        value={draft.comment}
        onChange={(e) => onDraftCommentChange(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
            e.preventDefault();
            onSubmitDraft();
          }
        }}
      />
      <div className="md-block-draft-actions">
        <span className="md-block-draft-range">
          第 {range.startLine}
          {range.endLine > range.startLine ? `-${range.endLine}` : ''} 行
        </span>
        <button type="button" className="btn btn-small btn-ghost" onClick={onCancelDraft}>
          取消
        </button>
        <button type="button" className="btn btn-small btn-primary" onClick={onSubmitDraft}>
          发布评论
        </button>
      </div>
    </div>
  );
}

/** 可批注块组件工厂：包裹目标标签，附加批注入口（块边缘弱存在感）、聚合徽章与草稿高亮。
 *  返回的组件在模块级稳定，避免父渲染导致子树重挂。 */
function makeAnnotatableBlock(tag: BlockTag) {
  function AnnotatableBlock({
    node,
    children,
    className,
    ...rest
  }: HTMLAttributes<HTMLElement> & { node?: unknown }) {
    const api = useContext(BlockAnnotationContext);
    const el = node as HastNode | undefined;
    if (!api || !el) {
      return createElement(tag, { ...rest, className }, children);
    }
    const range = api.resolveRange(el.position);
    const matched = range ? api.badges.get(`${range.startLine}:${range.endLine}`) : undefined;
    // D8 嵌套归属（徽章与草稿统一 owner）：本块 range 与子孙可批注块相等 → 让位最内层，
    // 徽章与草稿 UI（高亮/输入/草稿盒）均仅由最内层块渲染
    const isInnermost = !!(range && !hasSameRangeDescendant(api, el, range));
    const isDraft = !!(
      range &&
      isInnermost &&
      api.draft &&
      api.draft.startLine === range.startLine &&
      api.draft.endLine === range.endLine
    );
    const showBadge = !!(range && matched && matched.length > 0) && isInnermost;
    const cls =
      [className, range ? 'md-annotatable' : null, isDraft ? 'md-block-draft' : null]
        .filter(Boolean)
        .join(' ') || undefined;
    const draftBox =
      isDraft &&
      range &&
      api.draft && (
        <BlockDraftBox
          range={range}
          draft={api.draft}
          onDraftCommentChange={api.onDraftCommentChange}
          onSubmitDraft={api.onSubmitDraft}
          onCancelDraft={api.onCancelDraft}
        />
      );

    // F4（HTML 内容模型）：批注 UI（draftBox 为 div）不得作 p/h1-h6/pre 的后代（其内容模型仅
    // phrasing content），此类块的草稿态改由外层 wrap 承载批注 UI 与高亮；li/blockquote 允许 flow
    // content，无需包装。table 直接子元素同样不得含非表格元素。
    const overlay = (
      <>
        {showBadge && matched && (
          <BlockBadge annotations={matched} onLocate={api.onLocateAnnotations} />
        )}
        {range && api.canAnnotate && (
          <button
            type="button"
            className="md-block-annotate"
            title="批注此块"
            onClick={() => api.onAnnotateBlock(range)}
          >
            批注
          </button>
        )}
        {draftBox}
      </>
    );
    if (tag === 'table' || (isDraft && PHRASING_ONLY_TAGS.has(tag))) {
      return (
        <div className={cls ? `${cls} md-block-wrap` : 'md-block-wrap'}>
          {createElement(tag, { ...rest, className }, children)}
          {overlay}
        </div>
      );
    }
    return createElement(
      tag,
      { ...rest, className: cls },
      children,
      overlay,
    );
  }
  return AnnotatableBlock;
}

/** 模块级稳定 components/插件映射（design D8 可批注块集合 + D1 URL 判定层）。 */
const remarkPlugins = [remarkGfm];
const markdownComponents = {
  a: SafeLink,
  img: SafeImage,
  p: makeAnnotatableBlock('p'),
  h1: makeAnnotatableBlock('h1'),
  h2: makeAnnotatableBlock('h2'),
  h3: makeAnnotatableBlock('h3'),
  h4: makeAnnotatableBlock('h4'),
  h5: makeAnnotatableBlock('h5'),
  h6: makeAnnotatableBlock('h6'),
  li: makeAnnotatableBlock('li'),
  table: makeAnnotatableBlock('table'),
  pre: makeAnnotatableBlock('pre'),
  blockquote: makeAnnotatableBlock('blockquote'),
};

export interface MarkdownPreviewProps {
  /** 本侧完整 markdown 源文本 */
  content: string;
  /** D8：本侧别（块级批注所在栏语义）。未提供则不启用块级批注 UI。 */
  side?: 'old' | 'new';
  /** D8：本侧批注（owner 已按视图身份三元组过滤）。 */
  annotations?: Annotation[];
  /** D8：块级草稿（owner 持有：锚定范围 + 评论文本）。 */
  draft?: { startLine: number; endLine: number; comment: string } | null;
  /** D8：点击块批注入口（owner 端绑定侧别）。 */
  onAnnotateBlock?: (range: BlockLineRange) => void;
  /** D8：草稿评论文本变化（owner 持有输入状态）。 */
  onDraftCommentChange?: (comment: string) => void;
  /** D8：提交块级草稿（owner 复用既有批注创建链路）。 */
  onSubmitDraft?: () => void;
  /** D8：取消草稿（Esc/取消按钮；空评论关闭即丢弃）。 */
  onCancelDraft?: () => void;
  /** D8：徽章点击 → 批注列表定位全部匹配条目（复用既有定位机制）。 */
  onLocateAnnotations?: (ids: string[]) => void;
}

/** F2：解析子树按 content memoize——评论按键只经 Context 更新批注块（消费者穿透 memo 强制更新），
 *  `<Markdown>` 不重新执行 → 不重新 parse；content 变化时才重新解析。 */
const ParsedMarkdown = memo(function ParsedMarkdown({ content }: { content: string }) {
  return (
    <Markdown remarkPlugins={remarkPlugins} skipHtml components={markdownComponents}>
      {content}
    </Markdown>
  );
});

/** markdown 渲染预览：react-markdown v10 + remark-gfm，skipHtml 显式忽略原始 HTML（design D1 双保险）。 */
export default function MarkdownPreview({
  content,
  side,
  annotations,
  draft,
  onAnnotateBlock,
  onDraftCommentChange,
  onSubmitDraft,
  onCancelDraft,
  onLocateAnnotations,
}: MarkdownPreviewProps) {
  const [failedSrcs, setFailedSrcs] = useState<ReadonlySet<string>>(() => new Set<string>());

  // 内容变化后清空失败登记，允许同 src 图片重新加载（design D1：失败状态按最终 src 隔离）
  useEffect(() => {
    setFailedSrcs((prev) => (prev.size > 0 ? new Set<string>() : prev));
  }, [content]);

  const registry = useMemo<ImageFailureRegistry>(
    () => ({
      failed: failedSrcs,
      onError: (src) =>
        setFailedSrcs((prev) => {
          if (prev.has(src)) return prev;
          const next = new Set(prev);
          next.add(src);
          return next;
        }),
    }),
    [failedSrcs],
  );

  // F1/F6：换行索引由纯 helper 构造（每 content 一次 O(N)），生产 resolveRange 直接引用同一实现
  const lineIndex = useMemo(() => buildLineIndex(content), [content]);

  // D8 徽章预聚合：三元组由 owner 过滤，此处再按侧别过滤并按精确范围聚合（(createdAt,id) 升序）
  const blockApi = useMemo<BlockAnnotationApi | null>(() => {
    if (!side) return null;
    const badges = new Map<string, Annotation[]>();
    for (const a of sortAnnotations((annotations ?? []).filter((x) => x.side === side))) {
      const key = `${a.startLine}:${a.endLine}`;
      const list = badges.get(key);
      if (list) list.push(a);
      else badges.set(key, [a]);
    }
    return {
      resolveRange: (position) => blockRangeByIndex(lineIndex, position),
      badges,
      draft: draft ?? null,
      canAnnotate: !!onAnnotateBlock,
      onAnnotateBlock: onAnnotateBlock ?? (() => {}),
      onDraftCommentChange: onDraftCommentChange ?? (() => {}),
      onSubmitDraft: onSubmitDraft ?? (() => {}),
      onCancelDraft: onCancelDraft ?? (() => {}),
      onLocateAnnotations: onLocateAnnotations ?? (() => {}),
    };
  }, [
    content,
    lineIndex,
    side,
    annotations,
    draft,
    onAnnotateBlock,
    onDraftCommentChange,
    onSubmitDraft,
    onCancelDraft,
    onLocateAnnotations,
  ]);

  return (
    <div className="md-preview">
      <ImageFailureContext.Provider value={registry}>
        <BlockAnnotationContext.Provider value={blockApi}>
          <ParsedMarkdown content={content} />
        </BlockAnnotationContext.Provider>
      </ImageFailureContext.Provider>
    </div>
  );
}

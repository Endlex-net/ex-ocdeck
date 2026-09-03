import { useEffect, useRef, useState } from 'react';
import { Compartment, EditorState, Text, type Extension } from '@codemirror/state';
import { EditorView } from '@codemirror/view';
import { MergeView, unifiedMergeView } from '@codemirror/merge';
import { editableExtensions, editorTheme, readOnlyExtensions } from '../editor/extensions';
import { loadLanguage } from '../editor/language';
import type {
  Annotation,
  AnnotationCreateInput,
  FileEditRead,
  FileEditWriteInput,
  FileEditWriteResult,
  GitDiffResult,
} from '../../types';
import {
  annotationCandidateExtension,
  annotationGestures,
  annotationGutter,
  setAnnotationsEffect,
  setCandidateEffect,
  sideMarkerMap,
  unifiedMarkerMap,
  type AnnotationGesture,
} from './annotation-ext';
import { EditSession } from './edit-session';
import {
  buildSnapshot,
  editGateFor,
  filterByTriple,
  sideContent,
  sortAnnotations,
} from './review-utils';

export type DiffViewMode = 'unified' | 'side-by-side';

/** 编辑写回 IO（由 GitPanel 绑定 taskID 后注入，便于测试替换）。 */
export interface EditIO {
  read: () => Promise<FileEditRead>;
  write: (input: FileEditWriteInput) => Promise<FileEditWriteResult>;
}

interface DiffViewerProps {
  diff: GitDiffResult;
  /** 用于语法高亮语言选择的文件路径。 */
  path: string;
  /** 视图三元组（design D8）：sourceRef 即 diff 来源 ref（prop 名避开 React 保留字 ref）。 */
  sourceRef?: string;
  untracked?: boolean;
  /** 当前任务全部活动批注；本组件按三元组 + side 过滤后打标记。 */
  annotations?: Annotation[];
  /** agent 会话 busy/retry（design D6 编辑警告横幅）。 */
  agentBusy?: boolean;
  /** 创建批注（查看模式手势）；未提供则不挂批注手势。 */
  onCreateAnnotation?: (input: AnnotationCreateInput) => Promise<void>;
  /** 行内标记点击 → 批注列表定位高亮。 */
  onLocateAnnotations?: (ids: string[]) => void;
  /** 离开守卫注册（切换文件/退出编辑前 flush 并等待在途；阻塞未解决 → false）。 */
  onRegisterLeaveGuard?: (fn: (() => Promise<boolean>) | null) => void;
  /** 退出编辑模式前按当前三元组重新拉取 GitDiffResult（F3：查看模式不得停留在旧 diff，
   *  批注快照必须构造自最新原始侧内容，MUST NOT 用规范化后的编辑 GET 文本替代）。 */
  onRefreshDiff?: () => Promise<void>;
  /** 编辑读写端点；未提供则无编辑入口。 */
  editIO?: EditIO;
  modeOverride: DiffViewMode | null;
  onModeChange: (mode: DiffViewMode) => void;
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

interface BubbleState extends AnnotationGesture {
  editorKey: 'a' | 'b' | 'u';
}

/** 单文件 diff 渲染 + 查看模式批注手势 + 编辑模式写回（diff-review-workbench tasks 5.2/5.3）。 */
export default function DiffViewer({
  diff,
  path,
  sourceRef = '',
  untracked = false,
  annotations = [],
  agentBusy = false,
  onCreateAnnotation,
  onLocateAnnotations,
  onRegisterLeaveGuard,
  onRefreshDiff,
  editIO,
  modeOverride,
  onModeChange,
  wrapOverride,
  onWrapChange,
}: DiffViewerProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const wrapRef = useRef<HTMLDivElement>(null);
  const editorsRef = useRef<{ a?: EditorView; b?: EditorView; u?: EditorView }>({});
  const sessionRef = useRef<EditSession | null>(null);
  /** F3：退出事务期间经 compartment 把新侧编辑器切只读过渡态（保留 session 直至刷新完成）。 */
  const editLockCompartment = useRef(new Compartment());

  const [bubble, setBubble] = useState<BubbleState | null>(null);
  const [bubbleComment, setBubbleComment] = useState('');
  const [bubbleBusy, setBubbleBusy] = useState(false);
  const [bubbleError, setBubbleError] = useState('');
  const [crossSideHint, setCrossSideHint] = useState('');

  // 编辑模式：view（查看）→ checking（GET 编辑读取中）→ edit
  const [editPhase, setEditPhase] = useState<'view' | 'checking' | 'edit'>('view');
  /** 持久资格拒绝（GET editable=false）：禁用入口直至 diff 刷新（F6/F8）。 */
  const [deniedReason, setDeniedReason] = useState('');
  /** 瞬时进入失败（网络/任务锁等）：提示可重试，不禁用入口（F8）。 */
  const [enterError, setEnterError] = useState('');
  /** F3：退出事务进行中（flush + 刷新 diff），编辑器只读锁定、操作禁用。 */
  const [exiting, setExiting] = useState(false);
  const [exitError, setExitError] = useState('');
  /** F9：编辑器创建 effect 不经 exiting 依赖重建；重建时经 ref 读取当前锁态初始化 compartment。 */
  const exitingRef = useRef(false);
  const [sessionTick, setSessionTick] = useState(0);
  const [docEpoch, setDocEpoch] = useState(0); // discard/restore 后强制重建编辑器
  const [restoreArmed, setRestoreArmed] = useState(false);
  const [restoreBusy, setRestoreBusy] = useState(false);

  const state = deriveDiffState(diff);
  const mode: DiffViewMode =
    modeOverride ??
    (window.matchMedia('(max-width: 1024px)').matches ? 'unified' : 'side-by-side');
  // mode 变更横幅独立叠加（spec：两侧均存在且 mode 不同；可与 merge 视图或状态提示同时出现）
  const modeChanged = diff.oldExists && diff.newExists && diff.oldMode !== diff.newMode;

  const gate = editGateFor(
    diff,
    state.kind === 'merge',
    state.kind === 'message' ? state.text : null,
  );

  const session = sessionRef.current;
  void sessionTick; // session 事件驱动重渲染

  // ---------- 批注 ----------

  const clearCandidates = () => {
    for (const v of Object.values(editorsRef.current)) {
      v?.dispatch({ effects: setCandidateEffect.of(null) });
    }
  };

  const closeBubble = () => {
    setBubble(null);
    setBubbleComment('');
    setBubbleError('');
    setBubbleBusy(false);
    clearCandidates();
  };

  const openGesture = (editorKey: 'a' | 'b' | 'u', g: AnnotationGesture) => {
    if (editPhase !== 'view' || !onCreateAnnotation) return;
    if (g.startLine > g.endLine) {
      // 防御：跨侧/倒置映射不创建批注（spec 跨侧选区拒绝）
      setCrossSideHint('选区跨越两侧，请改为单侧选择。');
      return;
    }
    setCrossSideHint('');
    // 气泡 fixed 定位：钳制在视口内，避免贴边溢出（气泡宽 300px）
    const x = Math.max(8, Math.min(g.x, window.innerWidth - 316));
    const y = Math.max(8, Math.min(g.y, window.innerHeight - 200));
    setBubble({ ...g, editorKey, x, y });
    setBubbleComment('');
    setBubbleError('');
    // 选区候选高亮（空行范围 from==to 时回退到前一行，mark decoration 不允许空区间）
    const v = editorsRef.current[editorKey];
    if (v) {
      const s = Math.min(Math.max(1, g.startLine), v.state.doc.lines);
      const e = Math.min(Math.max(s, g.endLine), v.state.doc.lines);
      let from = v.state.doc.line(s).from;
      const to = v.state.doc.line(e).to;
      if (from >= to) from = v.state.doc.line(Math.max(1, s - 1)).from;
      if (from < to) {
        v.dispatch({ effects: setCandidateEffect.of({ from, to }) });
      }
    }
  };

  const submitBubble = async () => {
    // fail-closed：仅查看模式可提交批注（F7：气泡不得带入编辑模式）
    if (!bubble || !onCreateAnnotation || editPhase !== 'view') {
      if (editPhase !== 'view') closeBubble();
      return;
    }
    // 空评论丢弃（spec：未输入任何评论的批注不留存）
    if (!bubbleComment.trim()) {
      closeBubble();
      return;
    }
    setBubbleBusy(true);
    setBubbleError('');
    try {
      const win = buildSnapshot(sideContent(diff, bubble.side), bubble.startLine, bubble.endLine);
      await onCreateAnnotation({
        path,
        side: bubble.side,
        ref: sourceRef,
        untracked,
        startLine: bubble.startLine,
        endLine: bubble.endLine,
        snapshotStartLine: win.snapshotStartLine,
        snapshotLineCount: win.snapshotLineCount,
        snapshot: win.snapshot,
        comment: bubbleComment,
      });
      closeBubble();
    } catch (err) {
      setBubbleError(err instanceof Error ? err.message : '创建批注失败');
      setBubbleBusy(false);
    }
  };

  /** 把当前 props 的批注（三元组过滤 + 排序后）下发到各编辑器 gutter。 */
  const applyAnnotations = () => {
    const eds = editorsRef.current;
    const filtered = sortAnnotations(filterByTriple(annotations, { path, ref: sourceRef, untracked }));
    if (eds.a) {
      eds.a.dispatch({
        effects: setAnnotationsEffect.of(
          sideMarkerMap(filtered.filter((x) => x.side === 'old'), eds.a.state.doc.lines),
        ),
      });
    }
    if (eds.b) {
      eds.b.dispatch({
        effects: setAnnotationsEffect.of(
          sideMarkerMap(filtered.filter((x) => x.side === 'new'), eds.b.state.doc.lines),
        ),
      });
    }
    if (eds.u) {
      eds.u.dispatch({ effects: setAnnotationsEffect.of(unifiedMarkerMap(filtered, eds.u)) });
    }
  };
  const applyAnnotationsRef = useRef(applyAnnotations);
  applyAnnotationsRef.current = applyAnnotations;

  // 批注 prop 变化 → 仅 dispatch 更新 gutter，不重建编辑器
  useEffect(() => {
    applyAnnotationsRef.current();
  }, [annotations]);

  // ---------- 编辑模式 ----------

  const enterEdit = async () => {
    if (!editIO || !gate.ok || editPhase !== 'view') return;
    closeBubble(); // F7：进入编辑前关闭批注气泡与候选高亮（编辑模式无批注手势）
    setCrossSideHint('');
    setEditPhase('checking');
    setDeniedReason('');
    setEnterError('');
    try {
      const r = await editIO.read();
      if (!r.editable) {
        setDeniedReason(r.reason); // F6：资格拒绝持久生效，直至 diff 刷新（下方 effect 重置）
        setEditPhase('view');
        return;
      }
      sessionRef.current = new EditSession({
        path,
        firstRead: r,
        io: editIO,
        onChange: () => setSessionTick((t) => t + 1),
      });
      setEditPhase('edit');
    } catch (err) {
      // F8：瞬时请求错误（网络/任务锁冲突等）不写资格态——提示但允许直接重试
      setEnterError(err instanceof Error ? err.message : '读取文件失败');
      setEditPhase('view');
    }
  };

  /** 退出事务开始：新侧编辑器切只读过渡态（session 保留，输入不会静默丢失）。 */
  const lockEditors = (locked: boolean) => {
    const v = editorsRef.current.u ?? editorsRef.current.b;
    v?.dispatch({
      effects: editLockCompartment.current.reconfigure(
        locked ? [EditorView.editable.of(false), EditorState.readOnly.of(true)] : [],
      ),
    });
  };

  const exitEdit = async () => {
    const s = sessionRef.current;
    if (!s || exiting) return;
    setExiting(true);
    exitingRef.current = true;
    setExitError('');
    lockEditors(true);
    if (!(await s.canLeave())) {
      // 阻塞未解决：解除过渡锁定，冲突横幅引导重试或放弃
      lockEditors(false);
      setExiting(false);
      exitingRef.current = false;
      return;
    }
    // F3：回查看模式前按当前三元组刷新原始 diff；失败保持编辑模式（可重试退出），
    // MUST NOT 静默开放旧 diff 的查看与批注（会创建立即漂移的批注）
    try {
      await onRefreshDiff?.();
    } catch (err) {
      lockEditors(false);
      setExiting(false);
      exitingRef.current = false;
      setExitError(
        `刷新 diff 失败（${err instanceof Error ? err.message : '未知错误'}），仍停留在编辑模式。可重试退出或继续编辑。`,
      );
      return;
    }
    s.dispose();
    sessionRef.current = null;
    setEditPhase('view');
    setRestoreArmed(false);
    setExiting(false);
    exitingRef.current = false;
  };

  const retrySave = async () => {
    await sessionRef.current?.retry();
  };

  const discardChanges = async () => {
    const s = sessionRef.current;
    if (!s) return;
    const content = await s.discard();
    if (content !== null) setDocEpoch((e) => e + 1); // 重建编辑器为服务端内容
  };

  const restoreSnapshot = async () => {
    const s = sessionRef.current;
    if (!s || restoreBusy) return;
    setRestoreBusy(true);
    try {
      const content = await s.restore();
      if (content !== null) {
        setRestoreArmed(false);
        setDocEpoch((e) => e + 1); // 编辑器重建为 session.latest（还原快照或在途补发后的最新文本）
      }
    } finally {
      setRestoreBusy(false);
    }
  };

  // F6：GET editable=false 的拒绝理由与瞬时进入错误随 diff 刷新（重新拉取原始内容）而作废
  useEffect(() => {
    setDeniedReason('');
    setEnterError('');
  }, [diff]);

  // 离开守卫注册：GitPanel 切换文件前调用
  useEffect(() => {
    onRegisterLeaveGuard?.(async () => {
      const s = sessionRef.current;
      if (!s) return true;
      return s.canLeave();
    });
    return () => onRegisterLeaveGuard?.(null);
  }, [onRegisterLeaveGuard]);

  // 卸载：清理 debounce 计时器
  useEffect(() => {
    return () => sessionRef.current?.dispose();
  }, []);

  // ---------- 编辑器创建（销毁-重建路径：形态/换行/编辑模式/文档纪元 变化） ----------

  useEffect(() => {
    if (state.kind !== 'merge' || !containerRef.current) return;
    const container = containerRef.current;
    let destroyed = false;
    let view: MergeView | EditorView | null = null;
    void (async () => {
      const lang = await loadLanguage(path);
      if (destroyed) return;
      const s = sessionRef.current;
      const editing = editPhase === 'edit' && s !== null;
      const wrapExt = wrapOverride ? [EditorView.lineWrapping] : [];
      const langExt = lang ? [lang] : [];

      const updateListener = EditorView.updateListener.of((u) => {
        if (u.docChanged) sessionRef.current?.onEdit(u.state.doc.toString());
      });

      const viewExtensions = (side: 'old' | 'new' | 'unified'): Extension[] => [
        ...readOnlyExtensions,
        editorTheme,
        ...wrapExt,
        ...langExt,
        annotationCandidateExtension,
        ...(onCreateAnnotation
          ? [
              annotationGutter((ids) => onLocateAnnotations?.(ids)),
              annotationGestures({
                unified: side === 'unified',
                side: side === 'old' ? 'old' : 'new',
                onGesture: (g) => openGesture(side === 'unified' ? 'u' : side === 'old' ? 'a' : 'b', g),
                onCrossSide: () => setCrossSideHint('选区跨越两侧，请改为单侧选择。'),
              }),
            ]
          : []),
      ];

      const editExtensions: Extension[] = [
        ...editableExtensions,
        editorTheme,
        ...wrapExt,
        ...langExt,
        editLockCompartment.current.of(
          // F9：退出事务期间的任何重建（形态/换行/prop 变化）仍以只读锁态初始化
          exitingRef.current
            ? [EditorView.editable.of(false), EditorState.readOnly.of(true)]
            : [],
        ),
        updateListener,
      ];

      if (mode === 'side-by-side') {
        view = new MergeView({
          parent: container,
          a: { doc: diff.oldContent, extensions: editing ? [...readOnlyExtensions, editorTheme, ...wrapExt, ...langExt] : viewExtensions('old') },
          b: editing
            ? { doc: s.latest, extensions: editExtensions }
            : { doc: diff.newContent, extensions: viewExtensions('new') },
          highlightChanges: true,
          gutter: true,
          collapseUnchanged,
          diffConfig,
        });
        editorsRef.current = { a: view.a, b: view.b };
      } else {
        view = new EditorView({
          parent: container,
          doc: editing ? s.latest : diff.newContent,
          extensions: [
            ...(editing ? editExtensions : viewExtensions('unified')),
            unifiedMergeView({
              // Text.of 保留旧侧行尾字符；string original 会被 merge 规范化，吞掉 CRLF（design D6）。
              original: Text.of(diff.oldContent.split('\n')),
              mergeControls: false,
              collapseUnchanged,
              diffConfig,
            }),
          ],
        });
        editorsRef.current = { u: view };
      }
      if (!editing) applyAnnotationsRef.current();
    })();
    return () => {
      destroyed = true;
      view?.destroy();
      editorsRef.current = {};
    };
    // state 为每次 render 新建对象，deps 取 kind 原始值避免无关 rerender 重建编辑器；
    // 编辑模式进出与 discard/restore（docEpoch）走同一销毁-重建路径
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [state.kind, mode, wrapOverride, diff.oldContent, diff.newContent, path, editPhase, docEpoch]);

  const editStatusText = (s: EditSession | null): string => {
    if (!s) return '';
    switch (s.status) {
      case 'saving':
        return '保存中…';
      case 'pending':
        return '待保存…';
      case 'blocked':
        return '保存已暂停';
      default:
        return '已保存';
    }
  };

  return (
    <div className="diff-view" ref={wrapRef}>
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
          {gate.reason && <div className="edit-gate-note">不可编辑：{gate.reason}</div>}
        </div>
      )}
      {state.kind === 'message' && (
        <div className="empty">
          {state.text}
          {gate.reason && <div className="edit-gate-note">不可编辑：{gate.reason}</div>}
        </div>
      )}
      {state.kind === 'merge' && (
        <>
          <div className="diff-toolbar">
            <button
              type="button"
              className="btn btn-small btn-ghost"
              aria-pressed={mode === 'unified'}
              disabled={exiting}
              onClick={() => onModeChange('unified')}
            >
              单列
            </button>
            <button
              type="button"
              className="btn btn-small btn-ghost"
              aria-pressed={mode === 'side-by-side'}
              disabled={exiting}
              onClick={() => onModeChange('side-by-side')}
            >
              并排
            </button>
            <button
              type="button"
              className="btn btn-small btn-ghost"
              aria-pressed={wrapOverride}
              disabled={exiting}
              onClick={() => onWrapChange(!wrapOverride)}
            >
              换行
            </button>
            <span className="header-spacer" />
            {editIO && editPhase === 'view' && (
              <button
                type="button"
                className="btn btn-small btn-ghost"
                disabled={!gate.ok || deniedReason !== ''}
                title={
                  !gate.ok ? gate.reason : deniedReason !== '' ? deniedReason : '直接编辑工作区文件'
                }
                onClick={() => void enterEdit()}
              >
                编辑
              </button>
            )}
            {editIO && editPhase === 'checking' && (
              <button type="button" className="btn btn-small btn-ghost" disabled>
                检查中…
              </button>
            )}
            {editPhase === 'edit' && session && (
              <>
                <span className="edit-status">
                  {exiting ? '退出中…' : editStatusText(session)}
                </span>
                {!restoreArmed ? (
                  <button
                    type="button"
                    className="btn btn-small btn-ghost"
                    disabled={exiting}
                    title="恢复到本次编辑会话开始前的内容（仅本次会话内有效）"
                    onClick={() => setRestoreArmed(true)}
                  >
                    还原我的改动
                  </button>
                ) : (
                  <>
                    <span className="edit-restore-confirm">还原到进入编辑前的内容？</span>
                    <button
                      type="button"
                      className="btn btn-small"
                      disabled={restoreBusy || exiting}
                      onClick={() => void restoreSnapshot()}
                    >
                      {restoreBusy ? '还原中…' : '确认还原'}
                    </button>
                    <button
                      type="button"
                      className="btn btn-small btn-ghost"
                      disabled={exiting}
                      onClick={() => setRestoreArmed(false)}
                    >
                      取消
                    </button>
                  </>
                )}
                <button
                  type="button"
                  className="btn btn-small btn-ghost"
                  disabled={exiting}
                  onClick={() => void exitEdit()}
                >
                  {exiting ? '退出中…' : '退出编辑'}
                </button>
              </>
            )}
            {editIO && editPhase === 'view' && !gate.ok && (
              <span className="edit-gate-reason">{gate.reason}</span>
            )}
          </div>
          {deniedReason && editPhase === 'view' && (
            <div className="alert-bar alert-notice">
              <span>不可编辑：{deniedReason}</span>
            </div>
          )}
          {enterError && editPhase === 'view' && (
            <div className="alert-bar alert-notice">
              <span>进入编辑失败：{enterError}（可直接重试）</span>
            </div>
          )}
          {exitError && editPhase === 'edit' && (
            <div className="alert-bar alert-error">
              <span>{exitError}</span>
            </div>
          )}
          {crossSideHint && editPhase === 'view' && (
            <div className="alert-bar alert-notice">
              <span>{crossSideHint}</span>
            </div>
          )}
          {editPhase === 'edit' && agentBusy && (
            <div className="alert-bar alert-warn">
              <span>agent 正在修改代码，你的保存可能因冲突被拒。</span>
            </div>
          )}
          {editPhase === 'edit' && session?.status === 'blocked' && (
            <div className="alert-bar alert-error edit-conflict-bar">
              <span>{session.blockedReason}</span>
              <span className="edit-conflict-actions">
                <button
                  type="button"
                  className="btn btn-small"
                  onClick={() => void retrySave()}
                >
                  重试保存
                </button>
                <button
                  type="button"
                  className="btn btn-small btn-ghost"
                  onClick={() => void discardChanges()}
                >
                  放弃本地改动
                </button>
              </span>
            </div>
          )}
          <div ref={containerRef} className="diff-editor" />
          {bubble && editPhase === 'view' && (
            <div
              className="ann-bubble"
              style={{ left: bubble.x, top: bubble.y }}
              onKeyDown={(e) => {
                if (e.key === 'Escape') closeBubble();
              }}
            >
              <div className="ann-bubble-title">
                批注 · {bubble.side === 'old' ? '旧侧' : '新侧'} L{bubble.startLine}
                {bubble.endLine > bubble.startLine ? `-${bubble.endLine}` : ''}
              </div>
              <textarea
                className="input ann-bubble-input"
                autoFocus
                rows={3}
                placeholder="评论（留空关闭则丢弃）"
                value={bubbleComment}
                onChange={(e) => setBubbleComment(e.target.value)}
              />
              {bubbleError && <div className="error-line">{bubbleError}</div>}
              <div className="ann-bubble-actions">
                <button
                  type="button"
                  className="btn btn-small btn-ghost"
                  onClick={closeBubble}
                >
                  取消
                </button>
                <button
                  type="button"
                  className="btn btn-small btn-primary"
                  disabled={bubbleBusy}
                  onClick={() => void submitBubble()}
                >
                  {bubbleBusy ? '添加中…' : '添加批注'}
                </button>
              </div>
            </div>
          )}
        </>
      )}
    </div>
  );
}

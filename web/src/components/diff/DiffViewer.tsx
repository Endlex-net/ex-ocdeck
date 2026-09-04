import { useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { Compartment, EditorState, Text, type Extension } from '@codemirror/state';
import { EditorView } from '@codemirror/view';
import { MergeView, unifiedMergeView } from '@codemirror/merge';
import { editableExtensions, editorTheme, readOnlyExtensions } from '../editor/extensions';
import { loadLanguage } from '../editor/language';
import type {
  Annotation,
  AnnotationCreateInput,
  DiffSide,
  FileEditRead,
  FileEditReadEditable,
  FileEditWriteInput,
  FileEditWriteResult,
  GitDiffResult,
} from '../../types';
import {
  annotationCandidateExtension,
  annotationGestures,
  annotationGutter,
  inlineRegionExtension,
  setAnnotationsEffect,
  setCandidateEffect,
  setInlineRegionEffect,
  sideMarkerMap,
  syncInlineHostWidth,
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
  /** 批注 3：编辑模式偏好提升到 key 之外（GitPanel）——切文件不丢编辑/查看模式。
   *  true 时本视图资格 eligible 即自动进入编辑（仍走完整预取/门禁/冻结 metadata 流程）；
   *  不可编辑文件自然落回查看并显示原因。 */
  editModePreferred?: boolean;
  /** 模式变更回调（进入/退出编辑时同步给 GitPanel 持久偏好）。 */
  onEditModeChange?: (editing: boolean) => void;
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

interface InlineDraftState {
  editorKey: 'a' | 'b' | 'u';
  side: DiffSide;
  startLine: number;
  endLine: number;
  /** 内联批注区的宿主节点（React portal 挂载点，插入最后选中行下方的 CM block widget）。 */
  host: HTMLDivElement;
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
  editModePreferred = false,
  onEditModeChange,
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

  const [draft, setDraft] = useState<InlineDraftState | null>(null);
  const [draftComment, setDraftComment] = useState('');
  const [draftBusy, setDraftBusy] = useState(false);
  const [draftError, setDraftError] = useState('');
  const [crossSideHint, setCrossSideHint] = useState('');

  // 编辑模式：view（查看）→ edit
  const [editPhase, setEditPhase] = useState<'view' | 'edit'>('view');
  /** F7：编辑资格态（按视图三元组预取 GET 编辑读取）——仅 eligible 提供编辑命令；
   *  denied=服务端持久拒绝（editable=false，显示原因不提供命令）；
   *  error=瞬时请求错误（提示 + 重试入口）。deniedReason/enterError 双态语义沿用。 */
  const [eligibility, setEligibility] = useState<'checking' | 'eligible' | 'denied' | 'error'>(
    'checking',
  );
  /** eligible 时的 GET 结果：进入编辑直接复用为编辑基线（不重复 GET）。 */
  const firstReadRef = useRef<FileEditReadEditable | null>(null);
  /** F10：资格请求代际——仅最新代际的响应可提交状态（旧响应丢弃）。 */
  const eligibilitySeq = useRef(0);
  /** 持久资格拒绝（GET editable=false）：不提供编辑命令，直至 diff 刷新重新判定。 */
  const [deniedReason, setDeniedReason] = useState('');
  /** 瞬时资格请求失败（网络/任务锁等）：提示 + 重试，不写资格态（F8）。 */
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
  const singleSided = diff.oldExists !== diff.newExists;
  // 形态选择保持用户语义：null = 未选择（按视口默认）；单侧存在不再默认切单列——
  // 并排形态下空侧坍缩铺满（见 collapseSide），视觉像单列但语义仍是并排 MergeView。
  const mode: DiffViewMode =
    modeOverride ??
    (window.matchMedia('(max-width: 1024px)').matches ? 'unified' : 'side-by-side');
  /* 单侧存在（纯新增/纯删除）+ 并排：不存在侧坍缩为零宽不可见，存在侧编辑器铺满全宽。
     MergeView 无单侧配置面（merge 包恒建 a/b 两 EditorView），坍缩走 CSS（diff-collapse-*）：
     空侧 wrapper flex:0 0 0 + visibility:hidden——保留高度测量维持 merge 对齐计算，
     但不可见不占宽、不可交互不成为焦点目标；手势/编辑/内联批注区全走既有并排路径。 */
  const collapseSide: 'a' | 'b' | null =
    mode === 'side-by-side' && singleSided ? (diff.oldExists ? 'b' : 'a') : null;
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
      v?.dispatch({
        effects: [setCandidateEffect.of(null), setInlineRegionEffect.of(null)],
      });
    }
  };

  const closeDraft = () => {
    setDraft(null);
    setDraftComment('');
    setDraftError('');
    setDraftBusy(false);
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
    setDraftComment('');
    setDraftError('');
    // 批注 6：内联批注区插入最后选中行下方（CM block widget），随滚动自然移动
    const v = editorsRef.current[editorKey];
    const host = document.createElement('div');
    if (v) {
      const line = Math.min(Math.max(1, g.endLine), v.state.doc.lines);
      v.dispatch({ effects: setInlineRegionEffect.of({ line, host }) });
      // 选区候选高亮（空行范围 from==to 时回退到前一行，mark decoration 不允许空区间）
      const s = Math.min(Math.max(1, g.startLine), v.state.doc.lines);
      const e = Math.min(Math.max(s, g.endLine), v.state.doc.lines);
      let from = v.state.doc.line(s).from;
      const to = v.state.doc.line(e).to;
      if (from >= to) from = v.state.doc.line(Math.max(1, s - 1)).from;
      if (from < to) {
        v.dispatch({ effects: setCandidateEffect.of({ from, to }) });
      }
    }
    setDraft({
      editorKey,
      side: g.side,
      startLine: g.startLine,
      endLine: g.endLine,
      host,
    });
  };

  /* 不换行长行修复：批注区宿主宽度跟随 scroller 可视宽度（而非内容宽度），
   * 配合 .ann-inline-host 的 sticky left:0，保证取消/发布按钮恒在可视区右缘内。
   * 窗口 resize / gutter 宽度变化（行数位数增长）时经 ResizeObserver 重量。 */
  useEffect(() => {
    if (!draft) return;
    const view = editorsRef.current[draft.editorKey];
    if (!view) return;
    const host = draft.host;
    const sync = () => syncInlineHostWidth(view, host);
    sync();
    const observer = new ResizeObserver(sync);
    observer.observe(view.scrollDOM);
    const gutters = view.scrollDOM.querySelector('.cm-gutters');
    if (gutters) observer.observe(gutters);
    return () => {
      observer.disconnect();
      host.style.width = '';
      host.style.left = '';
    };
  }, [draft]);

  const submitDraft = async () => {
    // fail-closed：仅查看模式可提交批注（F7：草稿不得带入编辑模式）
    if (!draft || !onCreateAnnotation || editPhase !== 'view') {
      if (editPhase !== 'view') closeDraft();
      return;
    }
    // 空评论丢弃（spec：未输入任何评论的批注不留存）
    if (!draftComment.trim()) {
      closeDraft();
      return;
    }
    setDraftBusy(true);
    setDraftError('');
    try {
      const win = buildSnapshot(sideContent(diff, draft.side), draft.startLine, draft.endLine);
      await onCreateAnnotation({
        path,
        side: draft.side,
        ref: sourceRef,
        untracked,
        startLine: draft.startLine,
        endLine: draft.endLine,
        snapshotStartLine: win.snapshotStartLine,
        snapshotLineCount: win.snapshotLineCount,
        snapshot: win.snapshot,
        comment: draftComment,
      });
      closeDraft();
    } catch (err) {
      setDraftError(err instanceof Error ? err.message : '创建批注失败');
      setDraftBusy(false);
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

  /** F7：资格预取（进入编辑前完成服务端判定）。eligible 结果缓存为编辑基线。
   *  F10：代际防护——effect 可因 diff/editIO 变化重复启动，旧代际响应一律丢弃。 */
  const checkEligibility = async () => {
    if (!editIO) return;
    const seq = ++eligibilitySeq.current;
    setEligibility('checking');
    setDeniedReason('');
    setEnterError('');
    firstReadRef.current = null;
    try {
      const r = await editIO.read();
      if (seq !== eligibilitySeq.current) return; // 已有更新请求，丢弃本响应
      if (!r.editable) {
        setEligibility('denied');
        setDeniedReason(r.reason);
        return;
      }
      firstReadRef.current = r;
      setEligibility('eligible');
    } catch (err) {
      if (seq !== eligibilitySeq.current) return;
      setEligibility('error');
      setEnterError(err instanceof Error ? err.message : '读取文件失败');
    }
  };

  // F7：按视图身份预取资格（diff 刷新/视图变化即重新判定，资格拒绝随之作废）
  useEffect(() => {
    if (!editIO || !gate.ok || state.kind !== 'merge') return;
    void checkEligibility();
    return () => {
      eligibilitySeq.current++; // effect 重跑/卸载：作废旧代际在途请求
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editIO, gate.ok, state.kind, diff]);

  /** 进入编辑：仅 eligible 可入；复用预取 GET 结果作为编辑基线（不重复 GET）。 */
  const enterEdit = () => {
    const firstRead = firstReadRef.current;
    if (!editIO || editPhase !== 'view' || eligibility !== 'eligible' || !firstRead) return;
    closeDraft(); // F7(旧)：进入编辑前关闭内联批注区与候选高亮（编辑模式无批注手势）
    setCrossSideHint('');
    sessionRef.current = new EditSession({
      path,
      firstRead,
      io: editIO,
      onChange: () => setSessionTick((t) => t + 1),
    });
    setEditPhase('edit');
    onEditModeChange?.(true); // 批注 3：编辑偏好同步到 GitPanel（跨文件保持）
  };

  // 批注 3：编辑偏好为 true 且本视图资格 eligible → 自动进入编辑（切文件免再选）
  useEffect(() => {
    if (editModePreferred && editPhase === 'view' && eligibility === 'eligible') {
      enterEdit();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editModePreferred, editPhase, eligibility]);

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
    if (!s || exiting || discardBusyRef.current) return; // discard 在途：退出被拒（按钮已禁用）
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
    onEditModeChange?.(false); // 批注 3：显式退出 → 偏好回查看
  };

  const retrySave = async () => {
    await sessionRef.current?.retry();
  };

  /** F11：discard 在途锁（state 驱动按钮禁用，ref 防重入）。 */
  const [discardBusy, setDiscardBusy] = useState(false);
  const discardBusyRef = useRef(false);

  const discardChanges = async () => {
    const s = sessionRef.current;
    if (!s || !editIO || discardBusyRef.current) return; // 重入拒绝
    discardBusyRef.current = true;
    setDiscardBusy(true);
    lockEditors(true); // F11：discard 在途期间编辑器只读过渡态（输入不得被服务端内容覆盖）
    try {
      // F8/F11：discard 返回完整新读取 + 编辑代际栅栏
      const res = await s.discard();
      if (res === null) {
        lockEditors(false); // 重读请求失败：保持阻塞态，可重试
        return;
      }
      if (sessionRef.current !== s) {
        lockEditors(false); // F11：session 已被替换，迟到结果丢弃
        return;
      }
      const { read: r, editedDuring } = res;
      if (r.editable && editedDuring) {
        // F11：discard 等待期间有新编辑——保留用户文本，以新 session（新冻结元数据）补发，
        // MUST NOT 用服务端内容覆盖；旧 session 不 flush 销毁（补发唯一 owner 是新 session）
        const preserved = s.latest;
        s.dispose({ flush: false });
        sessionRef.current = null;
        firstReadRef.current = r;
        const ns = new EditSession({
          path,
          firstRead: r,
          io: editIO,
          onChange: () => setSessionTick((t) => t + 1),
        });
        sessionRef.current = ns;
        ns.onEdit(preserved); // 新基线排程补发
        setEligibility('eligible');
        setDocEpoch((e) => e + 1); // 重建编辑器显示 preserved（新实例未锁）
        return;
      }
      // 无在途编辑：旧 session 无未确认内容，常规销毁
      s.dispose();
      sessionRef.current = null;
      if (r.editable) {
        // 以完整新读取创建新 session（新冻结元数据 + 新基线），编辑器重建为服务端内容
        firstReadRef.current = r;
        sessionRef.current = new EditSession({
          path,
          firstRead: r,
          io: editIO,
          onChange: () => setSessionTick((t) => t + 1),
        });
        setEligibility('eligible');
        setDocEpoch((e) => e + 1);
      } else {
        // 安全退出路径：文件已不可编辑 → 回查看模式并显示原因（用户不被困在编辑态）
        try {
          await onRefreshDiff?.();
        } catch {
          /* 工具栏刷新可补救 */
        }
        setEligibility('denied');
        setDeniedReason(r.reason);
        setEditPhase('view');
        setRestoreArmed(false);
      }
    } finally {
      discardBusyRef.current = false;
      setDiscardBusy(false);
    }
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

  // 离开守卫注册：GitPanel 切换文件前调用
  useEffect(() => {
    onRegisterLeaveGuard?.(async () => {
      if (discardBusyRef.current) return false; // F11：discard 在途拒绝切换（无双 session 窗口）
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
    // 批注 6：编辑器销毁-重建时关闭内联批注区——宿主 block widget 随旧编辑器销毁，
    // 草稿锚点无法跨重建保留（进入编辑前 enterEdit 已先行关闭，此处覆盖形态/换行切换）
    closeDraft();
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
        inlineRegionExtension,
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
          // F9/F18：退出或 discard 事务期间的任何重建（形态/换行/prop 变化）仍以只读锁态初始化
          exitingRef.current || discardBusyRef.current
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
              disabled={exiting || discardBusy}
              onClick={() => onModeChange('unified')}
            >
              单列
            </button>
            <button
              type="button"
              className="btn btn-small btn-ghost"
              aria-pressed={mode === 'side-by-side'}
              disabled={exiting || discardBusy}
              onClick={() => onModeChange('side-by-side')}
            >
              并排
            </button>
            <button
              type="button"
              className="btn btn-small btn-ghost"
              aria-pressed={wrapOverride}
              disabled={exiting || discardBusy}
              onClick={() => onWrapChange(!wrapOverride)}
            >
              换行
            </button>
            <span className="header-spacer" />
            {editIO && editPhase === 'view' && !gate.ok && (
              <button
                type="button"
                className="btn btn-small btn-ghost"
                disabled
                title={gate.reason}
              >
                编辑
              </button>
            )}
            {/* F7：仅 eligible 提供编辑命令；denied 不提供命令（原因见下方提示条） */}
            {editIO && editPhase === 'view' && gate.ok && eligibility === 'eligible' && (
              <button
                type="button"
                className="btn btn-small btn-ghost"
                title="直接编辑工作区文件"
                onClick={enterEdit}
              >
                编辑
              </button>
            )}
            {editIO && editPhase === 'view' && gate.ok && eligibility === 'checking' && (
              <button type="button" className="btn btn-small btn-ghost" disabled>
                检查中…
              </button>
            )}
            {editIO && editPhase === 'view' && gate.ok && eligibility === 'error' && (
              <button
                type="button"
                className="btn btn-small btn-ghost"
                title={enterError}
                onClick={() => void checkEligibility()}
              >
                重试
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
                    disabled={exiting || discardBusy}
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
                      disabled={restoreBusy || exiting || discardBusy}
                      onClick={() => void restoreSnapshot()}
                    >
                      {restoreBusy ? '还原中…' : '确认还原'}
                    </button>
                    <button
                      type="button"
                      className="btn btn-small btn-ghost"
                      disabled={exiting || discardBusy}
                      onClick={() => setRestoreArmed(false)}
                    >
                      取消
                    </button>
                  </>
                )}
                <button
                  type="button"
                  className="btn btn-small btn-ghost"
                  disabled={exiting || discardBusy}
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
                  disabled={discardBusy}
                  onClick={() => void retrySave()}
                >
                  重试保存
                </button>
                <button
                  type="button"
                  className="btn btn-small btn-ghost"
                  disabled={discardBusy}
                  onClick={() => void discardChanges()}
                >
                  {discardBusy ? '放弃中…' : '放弃本地改动'}
                </button>
              </span>
            </div>
          )}
          <div
            ref={containerRef}
            className={collapseSide ? `diff-editor diff-collapse-${collapseSide}` : 'diff-editor'}
          />
          {/* 批注 6：内联批注区（参考 GitLab 变更评论）——portal 挂进 CM block widget 宿主，
              在该侧最后选中行下方切开，随编辑器滚动，无悬浮浮层 */}
          {draft &&
            editPhase === 'view' &&
            createPortal(
              <div
                className="ann-inline"
                onKeyDown={(e) => {
                  if (e.key === 'Escape') closeDraft();
                }}
              >
                <div className="ann-inline-head">
                  <span className="ann-inline-title">发布评论</span>
                  <span className="ann-inline-side">
                    {draft.side === 'old' ? '旧侧' : '新侧'}
                  </span>
                </div>
                <textarea
                  className="input ann-inline-input"
                  autoFocus
                  rows={3}
                  placeholder="添加此更改的上下文"
                  value={draftComment}
                  onChange={(e) => setDraftComment(e.target.value)}
                  onKeyDown={(e) => {
                    // 批注 4：⌘/Ctrl+Enter 快速提交（与点击同语义，空评论仍丢弃）；Esc 由外层关闭
                    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                      e.preventDefault();
                      void submitDraft();
                    }
                  }}
                />
                {draftError && <div className="error-line">{draftError}</div>}
                <div className="ann-inline-actions">
                  <span className="ann-inline-range">
                    第 {draft.startLine}
                    {draft.endLine > draft.startLine ? `-${draft.endLine}` : ''} 行
                  </span>
                  <span className="ann-inline-hint">⌘/Ctrl+Enter 提交 · Esc 取消</span>
                  <button
                    type="button"
                    className="btn btn-small btn-ghost"
                    onClick={closeDraft}
                  >
                    取消
                  </button>
                  <button
                    type="button"
                    className="btn btn-small btn-primary"
                    disabled={draftBusy}
                    onClick={() => void submitDraft()}
                  >
                    {draftBusy ? '发布中…' : '发布评论'}
                  </button>
                </div>
              </div>,
              draft.host,
            )}
        </>
      )}
    </div>
  );
}

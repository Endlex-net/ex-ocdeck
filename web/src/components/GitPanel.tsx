import { lazy, Suspense, useEffect, useMemo, useRef, useState, type CSSProperties, type ReactNode } from 'react';
import { api, ApiError } from '../api';
import { useMediaQuery } from '../hooks';
import type {
  Annotation,
  AnnotationCreateInput,
  GitBranchDiffFileEntry,
  GitDiffResult,
  GitFileEntry,
  GitStatus,
  SubmitCapability,
} from '../types';
import type { DiffViewMode } from './diff/DiffViewer';
import { BranchIcon, SidebarCollapseIcon, SidebarExpandIcon } from '../icons';
import { ResizeHandle, usePersistedSize } from './resize';
import { ReviewPanel } from './ReviewPanel';

// 编辑器代码（CodeMirror 及语言包）整 chunk 懒加载，与主 bundle 分离（design D8）。
const DiffViewerLazy = lazy(() => import('./diff/DiffViewer'));

const textEncoder = new TextEncoder();

/** UTF-8 byte-wise 字典序（与 D3 服务端排序同一比较规则；MUST NOT localeCompare；
 *  也 MUST NOT 用 JS 字符串 `<`——其按 UTF-16 code unit 比较，增补平面字符与 BMP
 *  PUA 的次序和 UTF-8 字节序相反，如 \u{10000} < \uE000）。 */
export function compareBytewise(a: string, b: string): number {
  const ua = textEncoder.encode(a);
  const ub = textEncoder.encode(b);
  const n = Math.min(ua.length, ub.length);
  for (let i = 0; i < n; i++) {
    if (ua[i] !== ub[i]) return ua[i] < ub[i] ? -1 : 1;
  }
  if (ua.length === ub.length) return 0;
  return ua.length < ub.length ? -1 : 1;
}

/** 展示条目状态（D5）：已提交条目无暂存状态概念，为 null（不套三色）。 */
type FileRowStatus = 'staged' | 'unstaged' | 'untracked';

/** D5 展示条目：每个 GitFileDTO 展开为 1-2 条（staged+unstaged 同路径出两条），
 *  保留 additions/deletions/isBinary 供统计与二进制标记展示。 */
export interface FileRow {
  path: string;
  status: FileRowStatus | null;
  /** D6 tree 叶子身份后缀：uncommitted 为 staged|unstaged|untracked，已提交固定字面量 'branch'。 */
  suffix: string;
  /** 6.3：已提交条目（branch 视图 three-dot 差异）——只读，点击走 gitBranchDiffFile。 */
  committed: boolean;
  /** diff 来源三元组（未提交条目）；已提交条目不使用。 */
  ref: string;
  untracked: boolean;
  additions: number;
  deletions: number;
  isBinary: boolean;
}

const STATUS_RANK: Record<FileRowStatus, number> = { staged: 0, unstaged: 1, untracked: 2 };
const STATUS_LABELS: Record<FileRowStatus, string> = {
  staged: '已暂存',
  unstaged: '未暂存',
  untracked: '未跟踪',
};

/** 同路径多条目固定次序（tasks 6.3）：已提交 → 已暂存 → 未暂存 → 未跟踪。 */
const rowOrder = (row: FileRow) => (row.committed ? 0 : 1 + STATUS_RANK[row.status as FileRowStatus]);

/** D5：GitFileDTO → 展示条目；路径 UTF-8 byte-wise 排序，同路径按固定次序
 *  （tasks 6.3：branch 视图传入已提交条目，与未提交条目混合为单一列表，不去重）。 */
export function listFileRows(files: GitFileEntry[], committedFiles?: GitBranchDiffFileEntry[]): FileRow[] {
  const rows: FileRow[] = [];
  for (const f of committedFiles ?? []) {
    rows.push({
      path: f.path, status: null, suffix: 'branch', committed: true, ref: '', untracked: false,
      additions: f.additions, deletions: f.deletions, isBinary: f.isBinary,
    });
  }
  for (const f of files) {
    if (f.staged) {
      rows.push({
        path: f.path, status: 'staged', suffix: 'staged', committed: false, ref: 'HEAD', untracked: false,
        additions: f.additions, deletions: f.deletions, isBinary: f.isBinary,
      });
    }
    if (f.unstaged && !f.untracked) {
      rows.push({
        path: f.path, status: 'unstaged', suffix: 'unstaged', committed: false, ref: '', untracked: false,
        additions: f.additions, deletions: f.deletions, isBinary: f.isBinary,
      });
    }
    if (f.untracked) {
      rows.push({
        path: f.path, status: 'untracked', suffix: 'untracked', committed: false, ref: '', untracked: true,
        additions: f.additions, deletions: f.deletions, isBinary: f.isBinary,
      });
    }
  }
  return rows.sort((a, b) => compareBytewise(a.path, b.path) || rowOrder(a) - rowOrder(b));
}

export type FileTreeItem =
  | { kind: 'dir'; key: string; name: string; children: FileTreeItem[] }
  | { kind: 'file'; key: string; name: string; row: FileRow };

/** D6 同名叶子固定次序（tasks 6.3）：已提交 → 已暂存 → 未暂存 → 未跟踪。 */
const TREE_SUFFIX_RANK: Record<string, number> = { branch: 0, staged: 1, unstaged: 2, untracked: 3 };

/** D6 纯前端投影：目录节点 key=目录路径，文件叶子 key=`path + "\0" + 后缀`（同路径多条目不去重）；
 *  同级按路径段 UTF-8 byte-wise 排序，同路径按固定次序（已提交→已暂存→未暂存→未跟踪）。 */
export function buildFileTree(rows: FileRow[]): FileTreeItem[] {
  type Dir = Extract<FileTreeItem, { kind: 'dir' }>;
  const root: Dir = { kind: 'dir', key: '', name: '', children: [] };
  for (const row of rows) {
    const segs = row.path.split('/');
    let dir = root;
    for (let i = 0; i < segs.length - 1; i++) {
      const key = segs.slice(0, i + 1).join('/');
      let child = dir.children.find((c): c is Dir => c.kind === 'dir' && c.key === key);
      if (!child) {
        child = { kind: 'dir', key, name: segs[i], children: [] };
        dir.children.push(child);
      }
      dir = child;
    }
    dir.children.push({
      kind: 'file',
      key: `${row.path}\0${row.suffix}`,
      name: segs[segs.length - 1],
      row,
    });
  }
  const sortDir = (d: Dir) => {
    d.children.sort((a, b) => {
      const byName = compareBytewise(a.name, b.name);
      if (byName !== 0) return byName;
      // 同名（仅同路径多条目可能）：目录在前，文件按已提交→已暂存→未暂存固定次序
      if (a.kind !== b.kind) return a.kind === 'dir' ? -1 : 1;
      return (
        TREE_SUFFIX_RANK[a.kind === 'file' ? a.row.suffix : ''] -
        TREE_SUFFIX_RANK[b.kind === 'file' ? b.row.suffix : '']
      );
    });
    for (const c of d.children) if (c.kind === 'dir') sortDir(c);
  };
  sortDir(root);
  return root.children;
}

type GitViewKind = 'uncommitted' | 'branch';

/** 任务工作台 git 面板（design.md 2.6）：status 分组 + CodeMirror merge diff 渲染 + commit/push
 *  + diff review 批注/提交（diff-review-workbench tasks 5.4）；
 *  git-page-enhancements D5-D8 + tasks 6.3-6.5：混合三色列表（branch 视图为已提交+未提交混合
 *  单列表）、容器级横向滚动、tree 形态投影、未提交/分支对比双视图切换与按条目类型门控
 *  （已提交条目只读，未提交条目两视图完整能力，未提交状态跨视图共享）。 */
export function GitPanel({
  taskID,
  active,
  agentBusy = false,
  baseRef = '',
}: {
  taskID: string;
  active: boolean;
  /** agent 会话 busy/retry（design D6 编辑警告横幅）。 */
  agentBusy?: boolean;
  /** 任务 base_ref（分支对比默认基础 ref）：manual 首次手改置位后，prop 后续变化不覆盖。 */
  baseRef?: string;
}) {
  const [status, setStatus] = useState<GitStatus | null>(null);
  const [loadError, setLoadError] = useState('');
  const [refreshing, setRefreshing] = useState(false);
  // selected 为 null 表示「全选」（初始状态）；刷新时保留仍存在的选中项
  const [selected, setSelected] = useState<Set<string> | null>(null);
  const [message, setMessage] = useState('');
  const [committing, setCommitting] = useState(false);
  const [pushing, setPushing] = useState(false);
  const [opError, setOpError] = useState(''); // git stderr 原样透传，保留换行
  const [opResult, setOpResult] = useState('');
  // selFile 为视图身份三元组（design D8）：同路径 staged/unstaged/untracked 是不同视图
  const [selFile, setSelFile] = useState<{ path: string; ref: string; untracked: boolean } | null>(
    null,
  );
  const [diff, setDiff] = useState<GitDiffResult | null>(null);
  const [diffLoading, setDiffLoading] = useState(false);
  const [diffError, setDiffError] = useState('');
  // 用户 diff 形态选择（DR1）：null = 未选择（按视口默认）；跨文件切换与 resize 保留，面板卸载丢弃。
  const [modeOverride, setModeOverride] = useState<DiffViewMode | null>(null);
  // 换行开关：默认关（canonical「代码行默认 MUST NOT 折行」，长行横向滚动）；用户可手动切换换行，生命周期与 modeOverride 一致（跨文件/视口保留，卸载丢弃）。
  const [wrapOverride, setWrapOverride] = useState(false);
  // 批注 3：编辑/查看模式偏好提升到三元组 key 之外——切文件保持模式（新文件经完整资格预取/门禁）
  const [editModePreferred, setEditModePreferred] = useState(false);
  // openDiff 请求序号：仅最新请求可写 diff/diffError/diffLoading（I2 乱序防护）
  const diffReqSeq = useRef(0);
  // diff review：批注/提交能力/列表定位高亮
  const [annotations, setAnnotations] = useState<Annotation[]>([]);
  const [capability, setCapability] = useState<SubmitCapability>({ state: 'unknown', reason: '' });
  const [highlightIDs, setHighlightIDs] = useState<Set<string>>(new Set());
  // 编辑模式离开守卫（DiffViewer 注册）：切换文件/视图消失前 flush 并等待在途写；阻塞未解决 → 拒绝
  const leaveGuard = useRef<(() => Promise<boolean>) | null>(null);
  // D6：文件面板宽度持久化（ocdeck:git-side-width，int px ∈ [240,600]，默认 340）+
  // 收起态（会话内 useState 不持久化；≤1024px 堆叠态收起偏好不生效，返回桌面恢复）
  const gitSide = usePersistedSize('ocdeck:git-side-width', 340, 240, 600, true);
  const [gitSideCollapsed, setGitSideCollapsed] = useState(false);
  const narrow = useMediaQuery('(max-width: 1024px)');
  const sideCollapsed = gitSideCollapsed && !narrow;
  // loadStatus 是 async，setState updater 内不能 await——用 ref 拿当前 selFile。
  // X5：单写源——ref 只在赋值点（openDiff 设置、loadStatus 清除）与 state 同步更新，
  // 不再于 render 阶段回写（旧双写会让渲染用旧 state 覆盖 imperative 值）
  const selFileRef = useRef<{ path: string; ref: string; untracked: boolean } | null>(null);

  // ---- D8：视图与 branch（已提交部分）状态；未提交状态为两视图共享，不清空 ----
  const [view, setView] = useState<GitViewKind>('uncommitted');
  // Y6：异步回调（commit 完成）需读「完成时」的视图/base——ref 在赋值点同步，不用发起时闭包
  const viewRef = useRef<GitViewKind>('uncommitted');
  // base 输入 draft（编辑中）/applied（已生效）分离：失焦或 Enter 生效，输入过程零请求
  const [baseDraft, setBaseDraft] = useState('');
  const [baseApplied, setBaseApplied] = useState('');
  const baseAppliedRef = useRef('');
  // manual：用户首次手改置位；baseRef prop 后续变化仅在 !manual 时更新
  const [baseManual, setBaseManual] = useState(false);
  const [branchFiles, setBranchFiles] = useState<GitBranchDiffFileEntry[] | null>(null);
  const [branchListLoading, setBranchListLoading] = useState(false);
  const [branchListError, setBranchListError] = useState('');
  const [branchListStale, setBranchListStale] = useState(false);
  // 已提交条目选中身份 = baseRef + generation + path（tasks 6.5：响应写入按全身份匹配，
  // 防同路径已提交/未提交交错选择竞态）；与未提交选中相互独立、各自保留、不互相覆盖
  const [committedSel, setCommittedSel] = useState<{
    path: string;
    base: string;
    generation: number;
  } | null>(null);
  // branch 视图 diff 焦点：视图内最近点击的条目类型（评审 W2）——branch 视图按焦点渲染
  // 已提交或未提交块，uncommitted 视图恒渲染共享未提交选中。
  // Y2：ref 与 state 同步维护——matches 等异步消费按焦点判定响应写入
  const [branchFocus, setBranchFocusState] = useState<'committed' | 'uncommitted'>('committed');
  const branchFocusRef = useRef<'committed' | 'uncommitted'>('committed');
  const setBranchFocus = (f: 'committed' | 'uncommitted') => {
    branchFocusRef.current = f;
    setBranchFocusState(f);
  };
  const [committedDiff, setCommittedDiff] = useState<GitDiffResult | null>(null);
  const [committedDiffLoading, setCommittedDiffLoading] = useState(false);
  const [committedDiffError, setCommittedDiffError] = useState('');
  const [committedDiffStale, setCommittedDiffStale] = useState(false);
  // D6：flat/tree 显式切换（默认 flat）与目录折叠（均会话级，不持久化）
  const [listForm, setListForm] = useState<'flat' | 'tree'>('flat');
  const [collapsedDirs, setCollapsedDirs] = useState<Set<string>>(new Set());
  // D8 序号模型：组件级全局单调 requestSeq + latest 游标（纯客户端状态，MUST NOT 进请求/响应）；
  // generation 为已提交缓存失效代次（base 变更与提交成功均递增），请求捕获发起时值用于响应写入条件判定
  const generationRef = useRef(0);
  const requestSeqRef = useRef(0);
  const latestListSeqRef = useRef(0);
  const latestFileSeqRef = useRef(0);
  const committedSelRef = useRef(committedSel);
  committedSelRef.current = committedSel;
  const branchFilesRef = useRef(branchFiles);
  branchFilesRef.current = branchFiles;
  // applied 变化后待拉取标记：仅 branch 视图（或进入 branch 视图）时真正发起请求，
  // 避免 prop 同步转移在 uncommitted 视图空耗请求
  const branchNeedsLoadRef = useRef(false);

  const loadAnnotations = async () => {
    try {
      const r = await api.listAnnotations(taskID);
      setAnnotations(r.annotations);
      setCapability(r.submitCapability);
    } catch {
      /* 批注列表失败不阻断 git 面板 */
    }
  };

  const loadStatus = async () => {
    setRefreshing(true);
    try {
      const s = await api.gitStatus(taskID);
      setStatus(s);
      setLoadError('');
      // 保留刷新后仍存在的选中项；新出现的文件跟随「全选」语义
      setSelected((prev) =>
        prev === null ? null : new Set(s.files.filter((f) => prev.has(f.path)).map((f) => f.path)),
      );
      // 选中视图已消失时清掉 diff 视图（按三元组核对：路径 + 来源组）。
      // F11：清除前必须过同一离开事务（flush + 等待在途写），否则 debounce 内最新文本静默丢失；
      // 阻塞未解决时保留视图（status 照更新），由冲突横幅引导用户处理。
      const cur = selFileRef.current;
      if (cur) {
        const stillThere = listFileRows(s.files).some(
          (row) =>
            row.ref === cur.ref && row.untracked === cur.untracked && row.path === cur.path,
        );
        if (!stillThere) {
          const guardOk = leaveGuard.current ? await leaveGuard.current() : true;
          if (guardOk) {
            // X5：先同步清 ref 再清 state——消失检查不得因残留引用重复消费旧条目
            selFileRef.current = null;
            setSelFile(null);
            setDiff(null);
          }
        }
      }
    } catch (err) {
      setLoadError(err instanceof ApiError ? err.message : '加载 git 状态失败');
    } finally {
      setRefreshing(false);
    }
  };

  // 面板激活时加载一次；不做自动轮询，避免打断勾选与 diff 阅读
  useEffect(() => {
    if (active) {
      void loadStatus();
      void loadAnnotations();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active, taskID]);

  // 用户主动操作上报（idle-reminder-user-activity D4）：在区域容器捕获阶段集中监听，
  // 嵌套组件（ReviewPanel/DiffViewer）手势天然复用同一入口；自动加载/刷新请求与
  // 程序滚动（scroll 事件）不产生这些事件、不计为主动操作。wheel/touchmove 用 passive
  // 监听，MUST NOT 阻止默认滚动。
  const panelRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    const el = panelRef.current;
    if (!el) return;
    // DR4（design D6）：六类手势共用 leading+trailing 节流，2s 窗口。
    const WINDOW_MS = 2000;
    let lastSentAt = -Infinity; // 单调时钟（performance.now()）；发送即计入窗口（含失败）
    let pending: string | null = null; // 冷却期内首个手势的 taskID
    let timer: ReturnType<typeof setTimeout> | null = null;

    const send = (id: string) => {
      lastSentAt = performance.now();
      api.reportActivity(id); // fire-and-forget：失败不排队、不重放（D6 规则 6）
    };

    const clearTimer = () => {
      if (timer !== null) {
        clearTimeout(timer);
        timer = null;
      }
    };

    // 尽力补报冷却期积累的手势后清理定时器；无 pending 不发送（D6 规则 5）
    const flushPending = () => {
      clearTimer();
      if (pending === null) return;
      const id = pending;
      pending = null;
      send(id);
    };

    // 页面进入 hidden：定时器可能被浏览器暂停，立即尽力补报
    const onVisibilityChange = () => {
      if (document.visibilityState === 'hidden') flushPending();
    };

    const report = (event: Event) => {
      // 仅可信事件（isTrusted=true，真实用户输入）上报；程序派发的 click/input 等
      // isTrusted=false，MUST NOT 计为主动操作，也不改变节流状态（D6 规则 1）。
      if (!event.isTrusted) return;
      const now = performance.now();
      if (now - lastSentAt >= WINDOW_MS) {
        // leading：距上次发送已满 2s 的手势立即发送；滞留 pending 被本次发送覆盖
        clearTimer();
        pending = null;
        send(taskID);
        return;
      }
      // trailing：冷却期内仅首个手势置 pending，按原定时刻补报；后续手势不推迟（D6 规则 3）
      if (pending !== null) return;
      pending = taskID;
      timer = setTimeout(() => {
        timer = null;
        const id = pending;
        pending = null;
        if (id !== null) send(id);
      }, lastSentAt + WINDOW_MS - now);
    };

    const gestureTypes = ['click', 'keydown', 'input', 'touchstart'] as const;
    const passiveTypes = ['wheel', 'touchmove'] as const;
    for (const type of gestureTypes) el.addEventListener(type, report, { capture: true });
    for (const type of passiveTypes) {
      el.addEventListener(type, report, { capture: true, passive: true });
    }
    document.addEventListener('visibilitychange', onVisibilityChange);
    return () => {
      for (const type of gestureTypes) el.removeEventListener(type, report, { capture: true });
      for (const type of passiveTypes) {
        el.removeEventListener(type, report, { capture: true });
      }
      document.removeEventListener('visibilitychange', onVisibilityChange);
      flushPending(); // 任务切换/卸载：尽力补报 pending（归属手势发生时的 taskID，不串报）
    };
  }, [taskID]);

  const allPaths = useMemo(() => (status?.files ?? []).map((f) => f.path), [status]);
  const effective = useMemo(
    () => (selected === null ? new Set(allPaths) : selected),
    [selected, allPaths],
  );

  const toggle = (path: string) => {
    const next = new Set(effective);
    if (next.has(path)) next.delete(path);
    else next.add(path);
    setSelected(next);
  };

  const openDiff = async (
    path: string,
    ref: string,
    untracked: boolean,
    opts?: { focus?: boolean },
  ) => {
    // 编辑模式离开守卫：flush 并等待在途写；冲突阻塞未解决时拒绝切换（D5 前端写协议）
    if (leaveGuard.current && !(await leaveGuard.current())) return;
    // 焦点更新在守卫通过之后（评审 W7：拒绝切换不得改焦点）；刷新路径（focus:false）不改变焦点
    if (opts?.focus !== false && view === 'branch') setBranchFocus('uncommitted');
    // 乱序防护（I2）：快速切换文件时，晚到的旧响应不得覆盖最新选中文件的 diff 状态
    const reqID = ++diffReqSeq.current;
    // ref 与 setState 同步更新（同 X4 模式：loadStatus 的消失检查依赖 ref，渲染提交时机不定）
    const nextSel = { path, ref, untracked };
    selFileRef.current = nextSel;
    setSelFile(nextSel);
    setDiff(null);
    setDiffError('');
    setDiffLoading(true);
    try {
      const result = await api.gitDiff(taskID, ref, path, untracked);
      if (reqID !== diffReqSeq.current) return;
      setDiff(result);
    } catch (err) {
      if (reqID !== diffReqSeq.current) return;
      setDiffError(err instanceof ApiError ? err.message : '加载 diff 失败');
    } finally {
      if (reqID === diffReqSeq.current) setDiffLoading(false);
    }
  };

  const createAnnotation = async (input: AnnotationCreateInput) => {
    await api.createAnnotation(taskID, input);
    await loadAnnotations();
  };

  /** 退出编辑模式前刷新当前视图的原始 diff（F3：查看模式与批注快照不得停留在编辑前内容）。
   *  走 gitDiff 端点拿八字段原始侧内容，MUST NOT 用编辑 GET 的规范化文本替代。失败抛错由调用方决定。 */
  const refreshDiff = async () => {
    const cur = selFile;
    if (!cur) return;
    const reqID = ++diffReqSeq.current;
    const result = await api.gitDiff(taskID, cur.ref, cur.path, cur.untracked);
    if (reqID !== diffReqSeq.current) return;
    setDiff(result);
  };

  /** branch 列表拉取（D8 序号模型）：base 取当前 baseAppliedRef（Y6：调用方读「当下」意图）；
   *  分配新全局序号并更新 latestListSeq；响应写入条件 = generation 一致 且 seq 为最新列表序号
   *  ——不满足（含成功与失败）一律丢弃且不改变当前错误状态/重载标志；失败（满足条件）置
   *  stale + 手动重载入口，MUST NOT 自动重试。commit 触发的重载同样经过本函数的该条件。 */
  const loadBranchList = async () => {
    const base = baseAppliedRef.current;
    if (base === '') return; // 空值零请求
    const seq = ++requestSeqRef.current;
    latestListSeqRef.current = seq;
    const gen = generationRef.current;
    setBranchListLoading(true);
    try {
      const r = await api.gitBranchDiffFiles(taskID, base);
      if (gen !== generationRef.current || seq !== latestListSeqRef.current) return;
      // ref 与 setState 同步更新（X4：openBranchDiff 的 path 校验依赖 ref；渲染提交时机不定）
      branchFilesRef.current = r.files;
      setBranchFiles(r.files);
      setBranchListError('');
      setBranchListStale(false);
      // 列表重载后已选已提交路径消失：清空其选中与 diff 视图（与 base 变更清空语义一致，
      // 清齐 error/stale/loading——评审 Z1：在途文件响应会被选中检查丢弃，loading 须就地清掉），
      // MUST NOT 报错
      const cur = committedSelRef.current;
      if (cur && !r.files.some((f) => f.path === cur.path)) {
        committedSelRef.current = null;
        setCommittedSel(null);
        setCommittedDiff(null);
        setCommittedDiffError('');
        setCommittedDiffStale(false);
        setCommittedDiffLoading(false);
      }
    } catch (err) {
      if (gen !== generationRef.current || seq !== latestListSeqRef.current) return;
      setBranchListStale(true);
      setBranchListError(err instanceof ApiError ? err.message : '加载分支对比列表失败');
    } finally {
      if (gen === generationRef.current && seq === latestListSeqRef.current) {
        setBranchListLoading(false);
      }
    }
  };

  /** 已提交单文件 diff：捕获 (base, generation, seq, path) 全身份；写入条件 = generation 一致
   *  且 seq 为最新文件序号 且 当前选中仍为该已提交条目（base+generation+path 全匹配，
   *  tasks 6.5——文件请求与列表请求互不使对方失效）。 */
  const openBranchDiff = async (path: string, opts?: { focus?: boolean }) => {
    const base = baseApplied;
    if (base === '') return;
    // 捕获请求意图（X1）：leave guard 等待期间 base/generation 可能被并发变更
    const intentGeneration = generationRef.current;
    // X2：仅用户点击路径执行编辑离开守卫——刷新/重试（focus:false）不被无关会话阻塞
    if (opts?.focus !== false && leaveGuard.current && !(await leaveGuard.current())) return;
    // guard 等待后意图失效（base/generation 已变）或 path 已不在当前已提交列表 → 丢弃返回
    if (
      baseApplied !== base ||
      generationRef.current !== intentGeneration ||
      !branchFilesRef.current?.some((f) => f.path === path)
    ) {
      return;
    }
    // 焦点更新在守卫与意图校验通过之后（评审 W7）；刷新路径（focus:false）不改变焦点
    if (opts?.focus !== false) setBranchFocus('committed');
    const seq = ++requestSeqRef.current;
    latestFileSeqRef.current = seq;
    const gen = generationRef.current;
    // ref 与 setState 同步更新（响应写入条件依赖 ref；渲染提交可能晚于快速响应）
    const nextSel = { path, base, generation: gen };
    committedSelRef.current = nextSel;
    setCommittedSel(nextSel);
    setCommittedDiff(null);
    setCommittedDiffError('');
    setCommittedDiffStale(false);
    setCommittedDiffLoading(true);
    const matches = () =>
      // Y2：当前焦点必须是已提交块——焦点切至未提交后，旧已提交响应一律丢弃
      branchFocusRef.current === 'committed' &&
      gen === generationRef.current &&
      seq === latestFileSeqRef.current &&
      committedSelRef.current?.path === path &&
      committedSelRef.current?.base === base &&
      committedSelRef.current?.generation === gen;
    try {
      const r = await api.gitBranchDiffFile(taskID, base, path);
      if (!matches()) return;
      setCommittedDiff(r);
    } catch (err) {
      if (!matches()) return;
      setCommittedDiffStale(true);
      setCommittedDiffError(err instanceof ApiError ? err.message : '加载 diff 失败');
    } finally {
      if (matches()) setCommittedDiffLoading(false);
    }
  };

  /** 已提交缓存失效转移（Y3/X1 共用）：generation 递增（在途旧响应失效丢弃）、清空已提交
   *  列表/选中/diff/error/stale、置待拉取标记。base 变更与提交成功均触发。 */
  const invalidateCommittedCache = () => {
    generationRef.current += 1;
    branchNeedsLoadRef.current = true;
    // ref 与 setState 同步更新（X4，同 loadBranchList）
    branchFilesRef.current = null;
    setBranchFiles(null);
    setBranchListError('');
    setBranchListStale(false);
    setBranchListLoading(false);
    committedSelRef.current = null;
    setCommittedSel(null);
    setCommittedDiff(null);
    setCommittedDiffError('');
    setCommittedDiffStale(false);
    setCommittedDiffLoading(false);
  };

  /** applied base 变化的同一状态转移（D8）：失效已提交缓存；
   *  未提交状态（status/勾选/选中/diff/编辑会话）不受影响（tasks 6.4）。 */
  const applyBranchBase = (next: string) => {
    invalidateCommittedCache();
    // ref 与 setState 同步更新（Y6：异步回调读完成时 base）
    baseAppliedRef.current = next;
    setBaseApplied(next);
  };

  /** base 输入生效（失焦或 Enter；输入过程零请求）：trim 后空值回落任务 base_ref 默认值。 */
  const applyBaseInput = () => {
    const trimmed = baseDraft.trim();
    const next = trimmed !== '' ? trimmed : baseRef.trim();
    setBaseDraft(next);
    if (next === baseApplied) return;
    applyBranchBase(next);
  };

  // baseRef prop（任务 base_ref）默认值同步：manual 首次手改置位后 prop 不再覆盖（D8）
  useEffect(() => {
    if (baseManual) return;
    setBaseDraft(baseRef);
    if (baseRef !== baseApplied) applyBranchBase(baseRef);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [baseRef]);

  // branch 列表生命周期：仅 branch 视图消费待拉取标记（空值零请求）；重载/重试走显式调用
  useEffect(() => {
    if (view !== 'branch' || !branchNeedsLoadRef.current || baseApplied === '') return;
    branchNeedsLoadRef.current = false;
    void loadBranchList();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [view, baseApplied]);

  /** 视图切换（D8）：视为离开当前 diff 视图——两个方向都执行编辑离开守卫，拒绝则停留；
   *  两视图选中各自保留，切换本身不清空任何列表/选中。 */
  const switchView = async (next: GitViewKind) => {
    if (next === view) return;
    if (leaveGuard.current) {
      const ok = await leaveGuard.current();
      if (!ok) return;
    }
    // ref 与 setState 同步更新（Y6：异步回调读完成时视图）
    viewRef.current = next;
    setView(next);
  };

  const toggleDir = (key: string) => {
    setCollapsedDirs((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  /** 工具栏刷新按视图分发（tasks 6.5 动作表）：branch = 重拉已提交列表 + 选中已提交条目 diff
   *  + 既有 loadStatus（未提交状态为共享状态）；uncommitted 走既有 refreshAll 语义。 */
  const refreshAll = async () => {
    if (view === 'branch') {
      const jobs: Promise<void>[] = [loadBranchList(), loadStatus()];
      if (committedSelRef.current) {
        // 刷新是数据操作：不改变 branch 视图焦点（评审 W6/W7）
        jobs.push(openBranchDiff(committedSelRef.current.path, { focus: false }));
      }
      await Promise.all(jobs);
      return;
    }
    await loadStatus();
    try {
      await refreshDiff();
    } catch (err) {
      setDiffError(err instanceof ApiError ? err.message : '刷新 diff 失败');
    }
  };

  const locateAnnotations = (ids: string[]) => {
    setHighlightIDs(new Set(ids));
  };

  const commit = async () => {
    if (committing || !message.trim() || effective.size === 0) return;
    setCommitting(true);
    setOpError('');
    setOpResult('');
    try {
      // 选中全部等价于空 paths（提交全部改动）
      const paths = effective.size === allPaths.length ? [] : [...effective];
      await api.gitCommit(taskID, message.trim(), paths);
      setMessage('');
      setOpResult('提交完成');
      // Y3/Y6：新提交改变 HEAD 对比基线——已提交列表缓存失效；重载决策读「完成时」的
      // 视图与 base（ref），不用发起时闭包。重载经 loadBranchList 的 generation+latestListSeq
      // 写入条件，不绕过。branch 视图下无选中，无需刷新已提交 diff。
      invalidateCommittedCache();
      if (viewRef.current === 'branch') {
        branchNeedsLoadRef.current = false;
        void loadBranchList();
      }
      await loadStatus();
    } catch (err) {
      setOpError(err instanceof ApiError ? err.message : '提交失败');
    } finally {
      setCommitting(false);
    }
  };

  const push = async () => {
    if (pushing) return;
    setPushing(true);
    setOpError('');
    setOpResult('');
    try {
      await api.gitPush(taskID);
      setOpResult('推送完成');
      // tasks 6.5 动作表：push 成功两视图均刷新共享的未提交状态；
      // 已提交列表与选中保持不变（commit/push 不改变 merge-base 差异）
      await loadStatus();
    } catch (err) {
      setOpError(err instanceof ApiError ? err.message : '推送失败');
    } finally {
      setPushing(false);
    }
  };

  const rows = useMemo(
    () => listFileRows(status?.files ?? [], view === 'branch' ? (branchFiles ?? []) : undefined),
    [view, branchFiles, status],
  );
  const treeItems = useMemo(() => buildFileTree(rows), [rows]);

  const rowKeyOf = (row: FileRow) => `${row.path}\0${row.suffix}`;

  const renderRow = (row: FileRow, depth: number, showDir: boolean) => {
    // 高亮按视图 + 焦点类型（Y2）：同路径两类条目不得同时 active
    const active = row.committed
      ? view === 'branch' &&
        branchFocus === 'committed' &&
        committedSel?.path === row.path
      : (view === 'uncommitted' || branchFocus === 'uncommitted') &&
        selFile?.path === row.path &&
        selFile.ref === row.ref &&
        selFile.untracked === row.untracked;
    // tasks 6.1 裁决 3：已提交条目无暂存状态，不套三色——中性色 + 「已提交」badge，
    // 非颜色提示经 title/aria-label 保留
    const statusText = row.status ? STATUS_LABELS[row.status] : row.committed ? '已提交' : '';
    // D5：颜色非唯一状态信息——title/aria-label 附状态文本
    const pathLabel = statusText ? `${row.path}（${statusText}）` : row.path;
    // flat：文件名（主视觉）+ 目录路径（次要层级）两段式，目录段含末尾 '/'（如 src/），
    // 根目录文件无目录段；tree：仅文件名——目录层级已由树节点表达（视觉 review 裁决），
    // 完整路径仍经 title/aria-label 保留（D5 非视觉信息通道不变）
    const slash = row.path.lastIndexOf('/');
    const name = slash === -1 ? row.path : row.path.slice(slash + 1);
    const dir = showDir && slash !== -1 ? row.path.slice(0, slash + 1) : '';
    const pathClass = row.status
      ? `git-file-path mono git-file-${row.status}`
      : row.committed
        ? 'git-file-path mono git-file-committed'
        : 'git-file-path mono';
    return (
      <div
        key={rowKeyOf(row)}
        className={active ? 'git-file git-file-active' : 'git-file'}
        style={{ paddingLeft: 12 + depth * 14 }}
      >
        {/* commit 勾选作用于未提交条目（两视图一致，tasks 6.4）；已提交条目只读无勾选 */}
        {!row.committed && (
          <input
            type="checkbox"
            checked={effective.has(row.path)}
            onChange={() => toggle(row.path)}
            title="纳入本次提交"
          />
        )}
        <span
          className={pathClass}
          title={pathLabel}
          aria-label={pathLabel}
          onClick={() => {
            // 焦点更新发生在打开函数的 leave guard 通过之后（评审 W7/W6）：
            // 点击路径设置焦点，guard 拒绝则焦点不变；刷新/重试路径不携带焦点意图
            if (row.committed) void openBranchDiff(row.path);
            else void openDiff(row.path, row.ref, row.untracked);
          }}
        >
          <span className="git-file-name">{name}</span>
          {dir !== '' && <span className="git-file-dir">{dir}</span>}
        </span>
        {row.committed && (
          <span className="git-file-badge" aria-hidden>
            已提交
          </span>
        )}
        {row.isBinary ? (
          <span className="git-stat">bin</span>
        ) : (
          <span className="git-stat">
            {row.additions > 0 && <span className="stat-add">+{row.additions}</span>}
            {row.deletions > 0 && <span className="stat-del">−{row.deletions}</span>}
          </span>
        )}
      </div>
    );
  };

  const renderTreeItems = (items: FileTreeItem[], depth: number): ReactNode[] => {
    const out: ReactNode[] = [];
    for (const item of items) {
      if (item.kind === 'dir') {
        const collapsed = collapsedDirs.has(item.key);
        out.push(
          <div
            key={item.key}
            className="git-tree-dir"
            style={{ paddingLeft: 12 + depth * 14 }}
            onClick={() => toggleDir(item.key)}
            title={collapsed ? `展开 ${item.name}` : `折叠 ${item.name}`}
            aria-label={`${collapsed ? '展开' : '折叠'} 目录 ${item.name}`}
            aria-expanded={!collapsed}
          >
            <span className="git-tree-toggle" aria-hidden>
              {collapsed ? '▸' : '▾'}
            </span>
            {item.name}
          </div>,
        );
        if (!collapsed) out.push(...renderTreeItems(item.children, depth + 1));
      } else {
        // tree 叶子仅展示文件名（目录层级由树节点表达）；完整路径经 renderRow 的 title/aria-label 保留
        out.push(renderRow(item.row, depth, false));
      }
    }
    return out;
  };

  /** F10：editIO memo——按真实依赖（taskID + 当前视图路径）稳定引用，
   *  避免 GitPanel 每次 render 产生新对象导致 DiffViewer 资格预取反复重启。 */
  const editIO = useMemo(
    () =>
      selFile
        ? {
            read: () => api.gitFileRead(taskID, selFile.path),
            write: (input: Parameters<typeof api.gitFileWrite>[1]) =>
              api.gitFileWrite(taskID, input),
          }
        : undefined,
    [taskID, selFile],
  );

  const refreshDisabled = branchListLoading || refreshing;

  /** 未提交条目 diff 块（完整能力，uncommitted 视图与 branch 视图未提交焦点共用）。 */
  const renderUncommittedDiff = (sel: { path: string; ref: string; untracked: boolean }) => (
    <>
      <div className="git-diff-header mono">
        {sel.path}
        {sel.ref && <span className="header-meta">（{sel.ref}）</span>}
      </div>
      {diffLoading && (
        <div className="empty">
          <span className="spinner spinner-inline" aria-hidden />
          加载 diff…
        </div>
      )}
      {diffError && <pre className="git-error mono">{diffError}</pre>}
      {!diffLoading && !diffError && diff && (
        <Suspense fallback={<div className="empty">加载 diff 视图…</div>}>
          <DiffViewerLazy
            // 三元组作为 key：切换目标文件即重建（编辑会话/批注手势随三元组隔离）
            key={`uncommitted|${sel.path}|${sel.ref}|${sel.untracked ? 'u' : ''}`}
            diff={diff}
            path={sel.path}
            sourceRef={sel.ref}
            untracked={sel.untracked}
            annotations={annotations}
            agentBusy={agentBusy}
            onCreateAnnotation={createAnnotation}
            onLocateAnnotations={locateAnnotations}
            onRegisterLeaveGuard={(fn) => {
              leaveGuard.current = fn;
            }}
            onRefreshDiff={refreshDiff}
            editIO={editIO}
            editModePreferred={editModePreferred}
            onEditModeChange={setEditModePreferred}
            modeOverride={modeOverride}
            onModeChange={setModeOverride}
            wrapOverride={wrapOverride}
            onWrapChange={setWrapOverride}
          />
        </Suspense>
      )}
    </>
  );

  /** 已提交条目 diff 块（只读门控：不传 annotations/批注回调/editIO/编辑资格刷新/离开守卫）。 */
  const renderCommittedDiff = (sel: { path: string; base: string; generation: number }) => (
    <>
      <div className="git-diff-header mono">
        {sel.path}
        {sel.base && <span className="header-meta">（{sel.base}）</span>}
      </div>
      {committedDiffLoading && (
        <div className="empty">
          <span className="spinner spinner-inline" aria-hidden />
          加载 diff…
        </div>
      )}
      {committedDiffError && (
        <div className="git-diff-retry">
          <pre className="git-error mono">{committedDiffError}</pre>
          {committedDiffStale && (
            <button
              type="button"
              className="btn btn-small"
              onClick={() => void openBranchDiff(sel.path, { focus: false })}
            >
              重试
            </button>
          )}
        </div>
      )}
      {!committedDiffLoading && !committedDiffError && committedDiff && (
        <Suspense fallback={<div className="empty">加载 diff 视图…</div>}>
          <DiffViewerLazy
            key={`branch|${sel.base}|${sel.generation}|${sel.path}`}
            diff={committedDiff}
            path={sel.path}
            modeOverride={modeOverride}
            onModeChange={setModeOverride}
            wrapOverride={wrapOverride}
            onWrapChange={setWrapOverride}
          />
        </Suspense>
      )}
    </>
  );

  return (
    <div className="git-panel" ref={panelRef}>
      <div
        className={sideCollapsed ? 'git-side git-side-collapsed' : 'git-side'}
        // D6：宽度经 CSS 变量传递（桌面规则 width: var(--git-side-w, 340px)），
        // 内联固定值 MUST NOT 直接写 width（窄屏 width:100% 规则保持优先）
        style={{ '--git-side-w': `${gitSide.value}px` } as CSSProperties}
      >
        {sideCollapsed ? (
          <button
            type="button"
            className="btn btn-ghost git-side-expand"
            onClick={() => setGitSideCollapsed(false)}
            aria-label="展开文件面板"
            title="展开文件面板"
          >
            <SidebarExpandIcon />
          </button>
        ) : (
          <>
            <div className="git-toolbar">
              <span className="mono git-branch" title="当前分支">
                <BranchIcon /> {status?.branch ?? '…'}
              </span>
              <span className="header-spacer" />
              <button
                className="btn btn-small btn-ghost"
                disabled={refreshDisabled}
                onClick={() => void refreshAll()}
              >
                {refreshDisabled ? '刷新中…' : '刷新'}
              </button>
              <button className="btn btn-small" disabled={pushing} onClick={() => void push()}>
                {pushing ? '推送中…' : '推送'}
              </button>
              {/* D6：收起入口仅 >1024px 桌面显示（堆叠态强制完整显示） */}
              {!narrow && (
                <button
                  type="button"
                  className="btn btn-small btn-ghost git-side-collapse"
                  onClick={() => setGitSideCollapsed(true)}
                  aria-label="收起文件面板"
                  title="收起文件面板"
                >
                  <SidebarCollapseIcon />
                </button>
              )}
            </div>

            <div className="git-view-tabs">
              <button
                type="button"
                className={view === 'uncommitted' ? 'git-view-tab git-view-tab-active' : 'git-view-tab'}
                onClick={() => void switchView('uncommitted')}
              >
                未提交变更
              </button>
              <button
                type="button"
                className={view === 'branch' ? 'git-view-tab git-view-tab-active' : 'git-view-tab'}
                onClick={() => void switchView('branch')}
              >
                分支对比
              </button>
              <span className="header-spacer" />
              {/* D6：flat/tree 显式切换，默认 flat，会话内保留不持久化 */}
              <button
                type="button"
                className="btn btn-small btn-ghost"
                onClick={() => setListForm((f) => (f === 'flat' ? 'tree' : 'flat'))}
              >
                {listForm === 'flat' ? '目录树' : '列表'}
              </button>
            </div>

            {view === 'branch' && (
              <div className="git-base-ref">
                <span>基础 ref</span>
                <input
                  className="input"
                  value={baseDraft}
                  placeholder={baseRef || '请输入基础 ref'}
                  onChange={(e) => {
                    setBaseManual(true);
                    setBaseDraft(e.target.value);
                  }}
                  onBlur={applyBaseInput}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') applyBaseInput();
                  }}
                />
              </div>
            )}

        {loadError && <div className="error-line">{loadError}</div>}
        {opResult && <div className="git-result">{opResult}</div>}
        {opError && <pre className="git-error mono">{opError}</pre>}

        <div className="git-files">
          <div className="git-files-inner">
            {view === 'uncommitted' ? (
              <>
                {status && status.files.length === 0 && (
                  <div className="git-clean">工作区干净，没有改动。</div>
                )}
                {listForm === 'tree'
                  ? renderTreeItems(treeItems, 0)
                  : rows.map((row) => renderRow(row, 0, true))}
              </>
            ) : (
              <>
                {/* tasks 6.3：已提交部分依赖基础 ref（空值提示），未提交条目照常展示 */}
                {baseApplied === '' && <div className="git-files-hint">请输入基础 ref</div>}
                {branchListStale && (
                  <div className="git-files-stale">
                    <span>{branchListError || '加载分支对比列表失败'}</span>
                    <button type="button" className="btn btn-small" onClick={() => void loadBranchList()}>
                      重载
                    </button>
                  </div>
                )}
                {branchListLoading && !branchFiles && (
                  <div className="git-clean">
                    <span className="spinner spinner-inline" aria-hidden />
                    加载分支对比…
                  </div>
                )}
                {branchFiles && branchFiles.length === 0 && status?.files.length === 0 && (
                  <div className="git-clean">没有已提交差异。</div>
                )}
                {listForm === 'tree'
                  ? renderTreeItems(treeItems, 0)
                  : rows.map((row) => renderRow(row, 0, true))}
              </>
            )}
          </div>
        </div>

        {/* 批注管理面板两视图均可用：展示任务全部活动批注（Y4，不做 status 过滤——
            文件消失后的 stale 批注仍可见可提交）；已提交条目的 DiffViewer 不接批注（门控不变） */}
        <ReviewPanel
          taskID={taskID}
          annotations={annotations}
          capability={capability}
          onChanged={loadAnnotations}
          highlightIDs={highlightIDs}
        />

        <div className="git-commit-box">
            <div className="git-commit-meta">
              <span>
                已选 {effective.size} / {allPaths.length} 个文件
              </span>
              <button className="btn btn-small btn-ghost" onClick={() => setSelected(null)}>
                全选
              </button>
              <button
                className="btn btn-small btn-ghost"
                onClick={() => setSelected(new Set())}
              >
                清空
              </button>
            </div>
            <textarea
              className="input git-commit-msg mono"
              placeholder="提交信息"
              rows={3}
              value={message}
              onChange={(e) => setMessage(e.target.value)}
            />
            <button
              className="btn btn-primary"
              disabled={committing || !message.trim() || effective.size === 0}
              onClick={() => void commit()}
            >
              {committing ? '提交中…' : `提交（${effective.size}）`}
            </button>
        </div>
          </>
        )}
      </div>

      {/* D6：文件面板拖宽把手（.git-side 右缘 6px 热区）；收起态与 ≤1024px 堆叠态不渲染 */}
      {!narrow && !sideCollapsed && (
        <ResizeHandle
          mode="px"
          value={gitSide.value}
          min={240}
          max={600}
          onDrag={gitSide.setSize}
          onCommit={gitSide.commitSize}
        />
      )}

      <div className="git-diff">
        {/* W5：未提交 diff 块恒挂载（hidden 切换可见性）——编辑会话/CM 实例跨视图与 branch
            焦点保活，不随焦点切换卸载；已提交块只读、仅 branch 视图挂载。隐藏块仍接收
            真实数据（与可见性解耦），可见块由视图与 branchFocus 决定（评审 W2）；
            两块 props 差异即 tasks 6.4 的按条目类型门控。 */}
        {selFile && (
          <div hidden={view === 'branch' && branchFocus !== 'uncommitted'}>
            {renderUncommittedDiff(selFile)}
          </div>
        )}
        {view === 'branch' && committedSel && (
          <div hidden={branchFocus !== 'committed'}>
            {renderCommittedDiff(committedSel)}
          </div>
        )}
        {(view === 'uncommitted'
          ? !selFile
          : branchFocus === 'committed'
            ? !committedSel
            : !selFile) && <div className="empty">点击左侧文件查看 diff。</div>}
      </div>
    </div>
  );
}

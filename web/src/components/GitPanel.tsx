import { lazy, Suspense, useEffect, useMemo, useRef, useState } from 'react';
import { api, ApiError } from '../api';
import type {
  Annotation,
  AnnotationCreateInput,
  GitDiffResult,
  GitFileEntry,
  GitStatus,
  SubmitCapability,
} from '../types';
import type { DiffViewMode } from './diff/DiffViewer';
import { BranchIcon } from '../icons';
import { ReviewPanel } from './ReviewPanel';

// 编辑器代码（CodeMirror 及语言包）整 chunk 懒加载，与主 bundle 分离（design D8）。
const DiffViewerLazy = lazy(() => import('./diff/DiffViewer'));

interface FileGroup {
  key: string;
  title: string;
  files: GitFileEntry[];
  /** 该组文件请求 diff 时使用的 ref（暂存组用 HEAD 才能看到索引内容）。 */
  ref: string;
  /** untracked 组 ref 为空且 untracked=1：旧侧不存在，渲染为全部新增视图。 */
  untracked: boolean;
}

function groupFiles(files: GitFileEntry[]): FileGroup[] {
  const groups: FileGroup[] = [
    { key: 'staged', title: '已暂存', ref: 'HEAD', untracked: false, files: files.filter((f) => f.staged) },
    {
      key: 'unstaged',
      title: '未暂存',
      ref: '',
      untracked: false,
      files: files.filter((f) => f.unstaged && !f.untracked),
    },
    { key: 'untracked', title: '未跟踪', ref: '', untracked: true, files: files.filter((f) => f.untracked) },
  ];
  return groups.filter((g) => g.files.length > 0);
}

/** 任务工作台 git 面板（design.md 2.6）：status 分组 + CodeMirror merge diff 渲染 + commit/push
 *  + diff review 批注/提交（diff-review-workbench tasks 5.4）。 */
export function GitPanel({
  taskID,
  active,
  agentBusy = false,
}: {
  taskID: string;
  active: boolean;
  /** agent 会话 busy/retry（design D6 编辑警告横幅）。 */
  agentBusy?: boolean;
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
  // 换行开关（design D6）：默认关（横向滚动）；生命周期与 modeOverride 一致（跨文件/视口保留，卸载丢弃）。
  const [wrapOverride, setWrapOverride] = useState(false);
  // openDiff 请求序号：仅最新请求可写 diff/diffError/diffLoading（I2 乱序防护）
  const diffReqSeq = useRef(0);
  // diff review：批注/提交能力/列表定位高亮
  const [annotations, setAnnotations] = useState<Annotation[]>([]);
  const [capability, setCapability] = useState<SubmitCapability>({ state: 'unknown', reason: '' });
  const [highlightIDs, setHighlightIDs] = useState<Set<string>>(new Set());
  // 编辑模式离开守卫（DiffViewer 注册）：切换文件/视图消失前 flush 并等待在途写；阻塞未解决 → 拒绝
  const leaveGuard = useRef<(() => Promise<boolean>) | null>(null);
  // loadStatus 是 async，setState updater 内不能 await——用 ref 拿当前 selFile
  const selFileRef = useRef(selFile);
  selFileRef.current = selFile;

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
        const stillThere = groupFiles(s.files).some(
          (g) =>
            g.ref === cur.ref &&
            g.untracked === cur.untracked &&
            g.files.some((f) => f.path === cur.path),
        );
        if (!stillThere) {
          const guardOk = leaveGuard.current ? await leaveGuard.current() : true;
          if (guardOk) {
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

  const openDiff = async (path: string, ref: string, untracked: boolean) => {
    // 编辑模式离开守卫：flush 并等待在途写；冲突阻塞未解决时拒绝切换（D5 前端写协议）
    if (leaveGuard.current && !(await leaveGuard.current())) return;
    // 乱序防护（I2）：快速切换文件时，晚到的旧响应不得覆盖最新选中文件的 diff 状态
    const reqID = ++diffReqSeq.current;
    setSelFile({ path, ref, untracked });
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

  /** 工具栏刷新：status 与当前 diff 一起刷（否则查看模式停留在旧内容，批注立即漂移）。 */
  const refreshAll = async () => {
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
      await loadStatus();
    } catch (err) {
      setOpError(err instanceof ApiError ? err.message : '推送失败');
    } finally {
      setPushing(false);
    }
  };

  const groups = useMemo(() => groupFiles(status?.files ?? []), [status]);

  return (
    <div className="git-panel">
      <div className="git-side">
        <div className="git-toolbar">
          <span className="mono git-branch" title="当前分支">
            <BranchIcon /> {status?.branch ?? '…'}
          </span>
          <span className="header-spacer" />
          <button
            className="btn btn-small btn-ghost"
            disabled={refreshing}
            onClick={() => void refreshAll()}
          >
            {refreshing ? '刷新中…' : '刷新'}
          </button>
          <button className="btn btn-small" disabled={pushing} onClick={() => void push()}>
            {pushing ? '推送中…' : '推送'}
          </button>
        </div>

        {loadError && <div className="error-line">{loadError}</div>}
        {opResult && <div className="git-result">{opResult}</div>}
        {opError && <pre className="git-error mono">{opError}</pre>}

        <div className="git-files">
          {status && status.files.length === 0 && (
            <div className="git-clean">工作区干净，没有改动。</div>
          )}
          {groups.map((g) => (
            <div key={g.key} className="git-group">
              <div className="git-group-title">
                {g.title}
                <span className="git-group-count">{g.files.length}</span>
              </div>
              {g.files.map((f) => (
                <div
                  key={`${g.key}:${f.path}`}
                  className={`git-file ${
                    selFile?.path === f.path &&
                    selFile.ref === g.ref &&
                    selFile.untracked === g.untracked
                      ? 'git-file-active'
                      : ''
                  }`}
                >
                  <input
                    type="checkbox"
                    checked={effective.has(f.path)}
                    onChange={() => toggle(f.path)}
                    title="纳入本次提交"
                  />
                  <span
                    className="git-file-path mono"
                    title={f.path}
                    onClick={() => void openDiff(f.path, g.ref, g.untracked)}
                  >
                    {f.path}
                  </span>
                  {f.isBinary ? (
                    <span className="git-stat">bin</span>
                  ) : (
                    <span className="git-stat">
                      {f.additions > 0 && <span className="stat-add">+{f.additions}</span>}
                      {f.deletions > 0 && <span className="stat-del">−{f.deletions}</span>}
                    </span>
                  )}
                </div>
              ))}
            </div>
          ))}
        </div>

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
      </div>

      <div className="git-diff">
        {!selFile && <div className="empty">点击左侧文件查看 diff。</div>}
        {selFile && (
          <>
            <div className="git-diff-header mono">
              {selFile.path}
              {selFile.ref && <span className="header-meta">（{selFile.ref}）</span>}
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
                  // 三元组作为 key：切换视图即重建（编辑会话/批注手势随视图隔离）
                  key={`${selFile.path}|${selFile.ref}|${selFile.untracked ? 'u' : ''}`}
                  diff={diff}
                  path={selFile.path}
                  sourceRef={selFile.ref}
                  untracked={selFile.untracked}
                  annotations={annotations}
                  agentBusy={agentBusy}
                  onCreateAnnotation={createAnnotation}
                  onLocateAnnotations={locateAnnotations}
                  onRegisterLeaveGuard={(fn) => {
                    leaveGuard.current = fn;
                  }}
                  onRefreshDiff={refreshDiff}
                  editIO={{
                    read: () => api.gitFileRead(taskID, selFile.path),
                    write: (input) => api.gitFileWrite(taskID, input),
                  }}
                  modeOverride={modeOverride}
                  onModeChange={setModeOverride}
                  wrapOverride={wrapOverride}
                  onWrapChange={setWrapOverride}
                />
              </Suspense>
            )}
          </>
        )}
      </div>
    </div>
  );
}

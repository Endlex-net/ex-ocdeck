import { useEffect, useMemo, useState } from 'react';
import { html as renderDiffHtml } from 'diff2html';
import 'diff2html/bundles/css/diff2html.min.css';
import { api, ApiError } from '../api';
import type { GitDiffResult, GitFileEntry, GitStatus } from '../types';

interface FileGroup {
  key: string;
  title: string;
  files: GitFileEntry[];
  /** 该组文件请求 diff 时使用的 ref（暂存组用 HEAD 才能看到索引内容）。 */
  ref: string;
  /** untracked 组用 `git diff --no-index` 合成 new-file diff（design.md D1）。 */
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

/** 任务工作台 git 面板（design.md 2.6）：status 分组 + diff2html 渲染 + commit/push。 */
export function GitPanel({ taskID, active }: { taskID: string; active: boolean }) {
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
  const [selFile, setSelFile] = useState<{ path: string; ref: string } | null>(null);
  const [diff, setDiff] = useState<GitDiffResult | null>(null);
  const [diffLoading, setDiffLoading] = useState(false);
  const [diffError, setDiffError] = useState('');

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
      // 选中文件已消失时清掉 diff 视图
      setSelFile((prev) => (prev && !s.files.some((f) => f.path === prev.path) ? null : prev));
    } catch (err) {
      setLoadError(err instanceof ApiError ? err.message : '加载 git 状态失败');
    } finally {
      setRefreshing(false);
    }
  };

  // 面板激活时加载一次；不做自动轮询，避免打断勾选与 diff 阅读
  useEffect(() => {
    if (active) void loadStatus();
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
    setSelFile({ path, ref });
    setDiff(null);
    setDiffError('');
    setDiffLoading(true);
    try {
      setDiff(await api.gitDiff(taskID, ref, path, untracked));
    } catch (err) {
      setDiffError(err instanceof ApiError ? err.message : '加载 diff 失败');
    } finally {
      setDiffLoading(false);
    }
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

  // diff2html 输出对代码内容做转义，可安全渲染；禁止直接注入原始 diff 文本
  const diffHtml = useMemo(() => {
    if (!diff || !diff.diff) return '';
    return renderDiffHtml(diff.diff, {
      drawFileList: false,
      matching: 'lines',
      outputFormat: 'line-by-line',
    });
  }, [diff]);

  return (
    <div className="git-panel">
      <div className="git-side">
        <div className="git-toolbar">
          <span className="mono git-branch" title="当前分支">
            ⎇ {status?.branch ?? '…'}
          </span>
          <span className="header-spacer" />
          <button
            className="btn btn-small btn-ghost"
            disabled={refreshing}
            onClick={() => void loadStatus()}
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
                  className={`git-file ${selFile?.path === f.path ? 'git-file-active' : ''}`}
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
            {diff?.truncated && (
              <div className="alert-bar alert-notice">diff 过大，已被服务端截断。</div>
            )}
            {diffLoading && (
              <div className="empty">
                <span className="spinner spinner-inline" aria-hidden />
                加载 diff…
              </div>
            )}
            {diffError && <pre className="git-error mono">{diffError}</pre>}
            {!diffLoading && !diffError && diff && !diff.diff && (
              <div className="empty">该文件暂无可展示的 diff。</div>
            )}
            {diffHtml && (
              // diff2html 渲染产物（内容已转义）
              <div className="diff-view" dangerouslySetInnerHTML={{ __html: diffHtml }} />
            )}
          </>
        )}
      </div>
    </div>
  );
}

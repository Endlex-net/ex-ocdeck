import { useEffect, useMemo, useRef, useState } from 'react';
import { api, ApiError } from '../api';
import type {
  Annotation,
  Submission,
  SubmissionsListResponse,
  SubmitCapability,
} from '../types';
import { sortAnnotations } from './diff/review-utils';

/* ============================ 批注列表面板 + 提交预览 + 提交分区（diff-review-workbench tasks 5.4） ============================
 * 低存在感：整体可折叠，嵌在 git 侧栏文件列表与提交框之间。 */

function formatTime(ts: number | null): string {
  if (!ts) return '';
  const d = new Date(ts * 1000);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getMonth() + 1}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function annLabel(a: { path: string; side: string; ref: string; untracked: boolean; startLine: number; endLine: number }): string {
  const src = a.untracked ? 'untracked' : a.ref || 'index';
  return `${a.path} · ${a.side === 'old' ? '旧侧' : '新侧'} L${a.startLine}${
    a.endLine > a.startLine ? `-${a.endLine}` : ''
  } · ${src}`;
}

/** 提交只读详情（spec「提交清空与历史」：历史展示批注快照；失败/投递未知展示可复制 payload，
 *  供用户人工判断后手动重发——不提供自动重发入口）。 */
function SubmissionDetail({
  sub,
  showPayload,
  onCopy,
  copied,
}: {
  sub: Submission;
  showPayload: boolean;
  onCopy: (sub: Submission) => void;
  copied: boolean;
}) {
  return (
    <div className="subs-detail">
      {sub.items.map((it) => (
        <div key={it.annotationId} className="subs-detail-item">
          <div className="ann-item-head mono">
            <span className="ann-item-loc" title={annLabel(it)}>
              {annLabel(it)}
            </span>
          </div>
          <div className="ann-comment">{it.comment}</div>
          <pre className="subs-snapshot mono">{it.snapshot}</pre>
        </div>
      ))}
      {sub.items.length === 0 && <div className="ann-empty">无批注条目。</div>}
      {showPayload && (
        <>
          <div className="subs-detail-payload-title">原文 payload（可复制后手动发给 agent）</div>
          <pre className="subs-payload mono">{sub.payload}</pre>
          <div className="ann-item-actions">
            <button
              type="button"
              className="btn btn-small btn-ghost"
              onClick={() => onCopy(sub)}
            >
              {copied ? '已复制' : '复制 payload'}
            </button>
          </div>
        </>
      )}
    </div>
  );
}

interface ReviewPanelProps {
  taskID: string;
  annotations: Annotation[];
  capability: SubmitCapability;
  /** 列表数据变化后回调（创建/编辑/删除/提交后刷新批注与能力）。 */
  onChanged: () => Promise<void> | void;
  /** 行内标记点击定位：高亮条目集合。 */
  highlightIDs: Set<string>;
}

export function ReviewPanel({
  taskID,
  annotations,
  capability,
  onChanged,
  highlightIDs,
}: ReviewPanelProps) {
  const [collapsed, setCollapsed] = useState(false);
  const [subs, setSubs] = useState<SubmissionsListResponse | null>(null);
  const [opError, setOpError] = useState('');

  // 行内编辑评论
  const [editingID, setEditingID] = useState<string | null>(null);
  const [draft, setDraft] = useState('');
  const [editError, setEditError] = useState('');

  // 预览弹层
  const [previewOpen, setPreviewOpen] = useState(false);
  const [excluded, setExcluded] = useState<Set<string>>(new Set());
  const [note, setNote] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [previewError, setPreviewError] = useState('');

  // 历史/失败提交的只读详情展开（F5）
  const [expandedSubs, setExpandedSubs] = useState<Set<string>>(new Set());
  const [copiedID, setCopiedID] = useState('');

  const toggleSubDetail = (id: string) =>
    setExpandedSubs((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  const copyPayload = async (s: Submission) => {
    try {
      await navigator.clipboard.writeText(s.payload);
      setCopiedID(s.id);
      setTimeout(() => setCopiedID((cur) => (cur === s.id ? '' : cur)), 2000);
    } catch {
      setOpError('复制失败，请手动选择文本复制');
    }
  };

  const sorted = useMemo(() => sortAnnotations(annotations), [annotations]);
  const canSubmit = capability.state === 'supported' && sorted.length > 0;

  const loadSubs = async () => {
    try {
      setSubs(await api.listSubmissions(taskID));
    } catch {
      /* 分区列表失败不阻断批注列表 */
    }
  };

  useEffect(() => {
    void loadSubs();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [taskID]);

  // 队列非空时轮询分区与批注（投递成功后服务端按 id+revision 清理批注）
  const queueLen = (subs?.queue.length ?? 0) > 0;
  useEffect(() => {
    if (!queueLen) return;
    const t = setInterval(() => {
      void loadSubs();
      void onChanged();
    }, 2500);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [queueLen, taskID]);

  // 定位高亮：展开面板并滚动到首个匹配条目
  const listRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (highlightIDs.size === 0) return;
    setCollapsed(false);
  }, [highlightIDs]);
  useEffect(() => {
    if (highlightIDs.size === 0 || collapsed || !listRef.current) return;
    const el = listRef.current.querySelector('.ann-item-highlight');
    el?.scrollIntoView?.({ block: 'nearest' });
  }, [highlightIDs, collapsed]);

  const saveComment = async (a: Annotation) => {
    try {
      await api.updateAnnotationComment(taskID, a.id, draft);
      setEditingID(null);
      setEditError('');
      await onChanged();
    } catch (err) {
      setEditError(err instanceof ApiError ? err.message : '更新评论失败');
    }
  };

  const removeAnnotation = async (a: Annotation) => {
    setOpError('');
    try {
      await api.deleteAnnotation(taskID, a.id);
      await onChanged();
    } catch (err) {
      setOpError(err instanceof ApiError ? err.message : '删除批注失败');
    }
  };

  const openPreview = () => {
    setExcluded(new Set());
    setNote('');
    setPreviewError('');
    setPreviewOpen(true);
  };

  const confirmSubmit = async () => {
    const items = sorted.filter((a) => !excluded.has(a.id));
    if (items.length === 0 || submitting) return;
    setSubmitting(true);
    setPreviewError('');
    try {
      await api.createSubmission(
        taskID,
        items.map((a) => ({ id: a.id, revision: a.revision })),
        note,
      );
      setPreviewOpen(false);
      await onChanged();
      await loadSubs();
    } catch (err) {
      if (err instanceof ApiError && (err.status === 409 || err.code === 'conflict')) {
        // 409：保留弹层，刷新批注后要求重新确认（D2 复核失败语义）
        setPreviewError('批注在确认前已变化，列表已刷新，请重新确认。');
        await onChanged();
      } else {
        setPreviewError(err instanceof ApiError ? err.message : '提交失败');
      }
    } finally {
      setSubmitting(false);
    }
  };

  const cancelSub = async (s: Submission) => {
    setOpError('');
    try {
      await api.cancelSubmission(taskID, s.id);
      await loadSubs();
    } catch (err) {
      setOpError(err instanceof ApiError ? err.message : '撤回失败');
    }
  };

  const deleteSub = async (s: Submission) => {
    setOpError('');
    try {
      await api.deleteSubmission(taskID, s.id);
      await loadSubs();
    } catch (err) {
      setOpError(err instanceof ApiError ? err.message : '删除失败');
    }
  };

  const previewItems = sorted.filter((a) => !excluded.has(a.id));

  return (
    <div className="review-panel">
      <div className="review-header" onClick={() => setCollapsed((c) => !c)}>
        <span className="review-title">批注（{sorted.length}）</span>
        <span className="header-spacer" />
        <button
          type="button"
          className="btn btn-small"
          disabled={!canSubmit}
          title={
            capability.state !== 'supported'
              ? capability.reason || '当前 agent 会话不支持提交批注'
              : sorted.length === 0
                ? '暂无批注'
                : '预览并提交给当前任务的 AI 会话'
          }
          onClick={(e) => {
            e.stopPropagation();
            openPreview();
          }}
        >
          提交给 AI
        </button>
        <span className="review-collapse">{collapsed ? '展开' : '收起'}</span>
      </div>

      {!collapsed && (
        <>
          {capability.state !== 'supported' && (
            <div className="review-capability-note">
              暂不可提交：{capability.reason || '当前 agent 会话不支持消息发送'}
            </div>
          )}
          {opError && <div className="error-line">{opError}</div>}

          <div className="ann-list" ref={listRef}>
            {sorted.length === 0 && <div className="ann-empty">暂无批注。在 diff 中框选或点击行号添加。</div>}
            {sorted.map((a) => (
              <div
                key={a.id}
                data-ann-id={a.id}
                className={`ann-item${highlightIDs.has(a.id) ? ' ann-item-highlight' : ''}`}
              >
                <div className="ann-item-head mono">
                  <span className="ann-item-loc" title={annLabel(a)}>
                    {annLabel(a)}
                  </span>
                  {a.stale && <span className="ann-stale-badge">已漂移</span>}
                </div>
                {editingID === a.id ? (
                  <div className="ann-edit">
                    <textarea
                      className="input ann-edit-input"
                      rows={2}
                      value={draft}
                      onChange={(e) => setDraft(e.target.value)}
                    />
                    {editError && <div className="error-line">{editError}</div>}
                    <div className="ann-item-actions">
                      <button
                        type="button"
                        className="btn btn-small"
                        onClick={() => void saveComment(a)}
                      >
                        保存
                      </button>
                      <button
                        type="button"
                        className="btn btn-small btn-ghost"
                        onClick={() => {
                          setEditingID(null);
                          setEditError('');
                        }}
                      >
                        取消
                      </button>
                    </div>
                  </div>
                ) : (
                  <div className="ann-comment">{a.comment}</div>
                )}
                {editingID !== a.id && (
                  <div className="ann-item-actions">
                    <button
                      type="button"
                      className="btn btn-small btn-ghost"
                      onClick={() => {
                        setEditingID(a.id);
                        setDraft(a.comment);
                        setEditError('');
                      }}
                    >
                      编辑评论
                    </button>
                    <button
                      type="button"
                      className="btn btn-small btn-ghost ann-danger"
                      onClick={() => void removeAnnotation(a)}
                    >
                      删除
                    </button>
                  </div>
                )}
              </div>
            ))}
          </div>

          {subs && (subs.queue.length > 0 || subs.history.length > 0 || subs.failures.length > 0) && (
            <div className="subs">
              {subs.queue.length > 0 && (
                <div className="subs-section">
                  <div className="subs-title">队列（{subs.queue.length}）</div>
                  {subs.queue.map((s) => (
                    <div key={s.id} className="subs-item">
                      <div className="subs-item-head">
                        <span>{s.status === 'queued' ? '排队中' : '发送中'} · {s.items.length} 条批注</span>
                        <span className="subs-time">{formatTime(s.createdAt)}</span>
                      </div>
                      {s.note && <div className="subs-note">{s.note}</div>}
                      {s.status === 'queued' && (
                        <div className="ann-item-actions">
                          <button
                            type="button"
                            className="btn btn-small btn-ghost"
                            onClick={() => void cancelSub(s)}
                          >
                            撤回
                          </button>
                        </div>
                      )}
                    </div>
                  ))}
                </div>
              )}
              {subs.failures.length > 0 && (
                <div className="subs-section">
                  <div className="subs-title">失败（{subs.failures.length}）</div>
                  {subs.failures.map((s) => (
                    <div key={s.id} className="subs-item">
                      <div className="subs-item-head">
                        <span>
                          {s.status === 'delivery_unknown' ? '投递未知' : '失败'} · {s.items.length} 条批注
                        </span>
                        <span className="subs-time">{formatTime(s.createdAt)}</span>
                      </div>
                      {s.error && <div className="subs-error">{s.error}</div>}
                      <div className="ann-item-actions">
                        <button
                          type="button"
                          className="btn btn-small btn-ghost"
                          onClick={() => toggleSubDetail(s.id)}
                        >
                          {expandedSubs.has(s.id) ? '收起详情' : '详情'}
                        </button>
                        <button
                          type="button"
                          className="btn btn-small btn-ghost ann-danger"
                          onClick={() => void deleteSub(s)}
                        >
                          删除
                        </button>
                      </div>
                      {expandedSubs.has(s.id) && (
                        <SubmissionDetail
                          sub={s}
                          showPayload
                          onCopy={(x) => void copyPayload(x)}
                          copied={copiedID === s.id}
                        />
                      )}
                    </div>
                  ))}
                </div>
              )}
              {subs.history.length > 0 && (
                <div className="subs-section">
                  <div className="subs-title">历史（{subs.history.length}）</div>
                  {subs.history.map((s) => (
                    <div key={s.id} className="subs-item">
                      <div className="subs-item-head">
                        <span>
                          已发送 · {s.items.length} 条批注
                          {s.truncated && <span className="ann-stale-badge">diff 已截断</span>}
                        </span>
                        <span className="subs-time">{formatTime(s.sentAt)}</span>
                      </div>
                      {s.note && <div className="subs-note">{s.note}</div>}
                      <div className="ann-item-actions">
                        <button
                          type="button"
                          className="btn btn-small btn-ghost"
                          onClick={() => toggleSubDetail(s.id)}
                        >
                          {expandedSubs.has(s.id) ? '收起详情' : '详情'}
                        </button>
                        <button
                          type="button"
                          className="btn btn-small btn-ghost ann-danger"
                          onClick={() => void deleteSub(s)}
                        >
                          删除
                        </button>
                      </div>
                      {expandedSubs.has(s.id) && (
                        <SubmissionDetail
                          sub={s}
                          showPayload={false}
                          onCopy={(x) => void copyPayload(x)}
                          copied={copiedID === s.id}
                        />
                      )}
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}
        </>
      )}

      {previewOpen && (
        <div className="od-modal-mask" onClick={() => !submitting && setPreviewOpen(false)}>
          <div className="od-modal ann-preview" onClick={(e) => e.stopPropagation()}>
            <h2>提交给 AI</h2>
            <div className="od-modal-body">
              <div className="ann-preview-hint">
                将把以下 {previewItems.length} 条批注与相关 diff 发送给当前任务的 AI 会话。
              </div>
              <div className="ann-preview-list">
                {previewItems.length === 0 && <div className="ann-empty">没有可提交的条目。</div>}
                {previewItems.map((a) => (
                  <div key={a.id} className="ann-preview-item">
                    <div className="ann-item-head mono">
                      <span className="ann-item-loc" title={annLabel(a)}>
                        {annLabel(a)}
                      </span>
                      {a.stale && <span className="ann-stale-badge">已漂移</span>}
                    </div>
                    <div className="ann-comment">{a.comment}</div>
                    <div className="ann-item-actions">
                      <button
                        type="button"
                        className="btn btn-small btn-ghost"
                        onClick={() => setExcluded((prev) => new Set(prev).add(a.id))}
                      >
                        本次不提交
                      </button>
                    </div>
                  </div>
                ))}
                {sorted
                  .filter((a) => excluded.has(a.id))
                  .map((a) => (
                    <div key={a.id} className="ann-preview-item ann-preview-excluded">
                      <div className="ann-item-head mono">
                        <span className="ann-item-loc">{annLabel(a)}</span>
                      </div>
                      <div className="ann-item-actions">
                        <span className="ann-excluded-note">本次不提交（保留在活动批注中）</span>
                        <button
                          type="button"
                          className="btn btn-small btn-ghost"
                          onClick={() =>
                            setExcluded((prev) => {
                              const next = new Set(prev);
                              next.delete(a.id);
                              return next;
                            })
                          }
                        >
                          恢复
                        </button>
                      </div>
                    </div>
                  ))}
              </div>
              <textarea
                className="input ann-note-input"
                rows={3}
                placeholder="补充说明（可选）"
                value={note}
                onChange={(e) => setNote(e.target.value)}
              />
              {previewError && <div className="error-line">{previewError}</div>}
            </div>
            <div className="od-modal-actions">
              <button
                type="button"
                className="btn btn-ghost"
                disabled={submitting}
                onClick={() => setPreviewOpen(false)}
              >
                取消
              </button>
              <button
                type="button"
                className="btn btn-primary"
                disabled={submitting || previewItems.length === 0}
                onClick={() => void confirmSubmit()}
              >
                {submitting ? '提交中…' : `确认提交（${previewItems.length}）`}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

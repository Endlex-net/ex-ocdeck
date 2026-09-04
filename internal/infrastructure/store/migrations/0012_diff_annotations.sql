-- migrations/0012_diff_annotations.sql
-- diff-review-workbench D3：批注 + 提交队列 + 提交快照三表。
-- 时间戳沿用秒级 Unix（与 queries.go nowUnix 一致）；revision 为每次实变严格 +1 的整数，
-- 版本比对唯一依据（秒级 updated_at 同秒实变不推进，见 Context）。

-- 活动批注：每次提交后按 id+revision 在 sent 清理事务内删除（revision 已 +1 即被编辑，保留）。
CREATE TABLE IF NOT EXISTS diff_annotations (
    id                  TEXT PRIMARY KEY,          -- UUID
    task_id             TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    path                TEXT NOT NULL,
    side                TEXT NOT NULL,             -- 'old' | 'new'
    ref                 TEXT NOT NULL DEFAULT '',  -- 创建时 diff 来源 ref（空=index/untracked）
    untracked           INTEGER NOT NULL DEFAULT 0,
    start_line          INTEGER NOT NULL,          -- 1-based 闭区间
    end_line            INTEGER NOT NULL,
    snapshot_start_line INTEGER NOT NULL,          -- 快照窗口首行行号（含上下文）
    snapshot            TEXT NOT NULL,             -- 完整窗口文本（含行尾字符，见 D4）
    snapshot_line_count INTEGER NOT NULL,          -- 窗口行数
    comment             TEXT NOT NULL,
    revision            INTEGER NOT NULL DEFAULT 1, -- 每次实变严格 +1（版本比对唯一依据）
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_diff_annotations_task ON diff_annotations(task_id);

-- 提交队列：seq 为表级 AUTOINCREMENT 入队序（FIFO 唯一依据，不复用、单调）；
-- id 为 TEXT UNIQUE 的 UUID；message_id = msg_<submission UUID 去连字符>（见 D1）。
-- status ∈ queued|sending|sent|failed|delivery_unknown（撤回=DELETE 行，不保留 cancelled 记录）。
-- sent_at 仅 sent 事务本地提交时间，其余状态为 NULL。
CREATE TABLE IF NOT EXISTS diff_review_submissions (
    seq               INTEGER PRIMARY KEY AUTOINCREMENT,  -- 全局入队序（FIFO 唯一依据，不复用）
    id                TEXT NOT NULL UNIQUE,        -- UUID
    task_id           TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    status            TEXT NOT NULL,               -- queued|sending|sent|failed|delivery_unknown
    target_session_id TEXT NOT NULL,
    message_id        TEXT NOT NULL UNIQUE,        -- msg_<submission UUID 去连字符>，见 D1
    note              TEXT NOT NULL DEFAULT '',
    payload           TEXT NOT NULL,
    truncated         INTEGER NOT NULL DEFAULT 0,
    error             TEXT NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,            -- 排队时间
    sent_at           INTEGER                      -- sent 事务本地提交时间
);
CREATE INDEX IF NOT EXISTS idx_diff_review_submissions_task ON diff_review_submissions(task_id);

-- 不可变提交快照：排队期间用户继续编辑/删除活动批注不影响已提交内容。
-- annotation_revision 为快照时批注 revision（sent 清理比对用）；PRIMARY KEY 防止同提交重复条目。
CREATE TABLE IF NOT EXISTS diff_review_submission_items (
    submission_id      TEXT NOT NULL REFERENCES diff_review_submissions(id) ON DELETE CASCADE,
    annotation_id      TEXT NOT NULL,
    annotation_revision INTEGER NOT NULL,          -- 快照时批注 revision（sent 清理比对用）
    path               TEXT NOT NULL,
    side               TEXT NOT NULL,
    ref                TEXT NOT NULL DEFAULT '',
    untracked          INTEGER NOT NULL DEFAULT 0,
    start_line         INTEGER NOT NULL,
    end_line           INTEGER NOT NULL,
    snapshot_start_line INTEGER NOT NULL,
    snapshot           TEXT NOT NULL,
    comment            TEXT NOT NULL,
    PRIMARY KEY (submission_id, annotation_id)
);
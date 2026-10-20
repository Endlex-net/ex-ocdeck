-- migrations/0019_provider_runtime.sql
-- v2 runtime 实例记录（opencode-v2-provider design D3）：A+ 全局单 server 的
-- 跨进程恢复来源。provider_id/instance_id 复合主键（单实例模型下 (opencode-v2,
-- default) 一行）；port 固定不轮换（首次分配后即持久事实）；pid 允许 NULL
--（「启动中」态——先于 server 启动写入，步骤⑤才落实际 pid）；last_error 记
-- 启动失败诊断（D3 恢复矩阵「启动失败」分支）。
CREATE TABLE IF NOT EXISTS provider_runtime (
    provider_id  TEXT NOT NULL,
    instance_id  TEXT NOT NULL,
    port         INTEGER NOT NULL,
    pid          INTEGER,
    last_error   TEXT,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (provider_id, instance_id)
);

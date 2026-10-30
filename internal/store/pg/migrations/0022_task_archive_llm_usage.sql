-- 0022_task_archive_llm_usage: 任务归档，与按天累计的模型用量
--
-- 归档只是把已结束的任务从后台的任务列表里收起来，数据原样保留，可以取消归档。
-- 模型用量由每次模型调用累加，按本地日期一天一行；只记数字，不记用途、模型名和内容。

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS tasks_created_idx ON tasks (created_at DESC);

CREATE TABLE IF NOT EXISTS llm_usage (
    day               DATE   PRIMARY KEY,
    calls             BIGINT NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    cached_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    cost_micros       BIGINT NOT NULL DEFAULT 0
);

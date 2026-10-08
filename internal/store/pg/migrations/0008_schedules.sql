-- 0008_schedules: 定时任务
--
-- 到点就按预设的标题与目标创建一个新任务。见 internal/schedule。

CREATE TABLE IF NOT EXISTS schedules (
    id            TEXT PRIMARY KEY,
    title         TEXT        NOT NULL,
    goal          TEXT        NOT NULL DEFAULT '',

    every_ns      BIGINT      NOT NULL,
    next_run_at   TIMESTAMPTZ NOT NULL,
    enabled       BOOLEAN     NOT NULL DEFAULT true,

    -- 它创建的任务沿用这个信任等级。用户亲手建的是 user。
    origin_trust  SMALLINT    NOT NULL,
    domain        TEXT        NOT NULL,

    -- 最近一次触发创建的任务。不设外键：任务被清理后这条定时任务照常工作。
    last_task_id  TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL,

    -- 一分钟。更密的触发基本都是配置写错了，而每次触发都要花模型的钱。
    CONSTRAINT schedules_every_min CHECK (every_ns >= 60000000000),
    CONSTRAINT schedules_domain_valid CHECK (domain IN ('personal', 'work', 'mixed'))
);

CREATE INDEX IF NOT EXISTS schedules_due_idx ON schedules (next_run_at) WHERE enabled;

-- 0019_op_logs: 操作日志，在管理后台里按类别查看
--
-- 运行时的日志和任务审计都记在这里。消息和各项内容（message、attrs）是加密的。
-- 只留一段时间，过期的由程序定期删掉。
CREATE TABLE IF NOT EXISTS op_logs (
    id        TEXT PRIMARY KEY,
    at        TIMESTAMPTZ NOT NULL,
    level     SMALLINT    NOT NULL,
    category  TEXT        NOT NULL,
    message   TEXT        NOT NULL,
    attrs     TEXT        NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS op_logs_at_idx ON op_logs (at DESC, id DESC);
CREATE INDEX IF NOT EXISTS op_logs_category_idx ON op_logs (category, at DESC, id DESC);

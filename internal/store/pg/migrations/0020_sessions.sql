-- 0020_sessions: 用户与助手的会话，一天一个
--
-- 话记在会话里，事记在任务里。见 internal/session。
-- 消息正文和摘要是加密的。日子写成 2026-10-21 这样的文字：一天从凌晨四点算起，由程序换算。

CREATE TABLE IF NOT EXISTS session_messages (
    id       TEXT PRIMARY KEY,
    day      TEXT        NOT NULL,
    at       TIMESTAMPTZ NOT NULL,
    role     TEXT        NOT NULL,
    channel  TEXT        NOT NULL DEFAULT '',
    -- 助手推送任务进展时记下是哪个任务；用户说的话为空。
    task_id  TEXT        NOT NULL DEFAULT '',
    text     TEXT        NOT NULL,
    CONSTRAINT session_messages_role_valid CHECK (role IN ('user', 'assistant')),
    CONSTRAINT session_messages_day_valid CHECK (day ~ '^\d{4}-\d{2}-\d{2}$')
);

CREATE INDEX IF NOT EXISTS session_messages_day_idx ON session_messages (day, at, id);

CREATE TABLE IF NOT EXISTS session_summaries (
    day         TEXT PRIMARY KEY,
    summary     TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    CONSTRAINT session_summaries_day_valid CHECK (day ~ '^\d{4}-\d{2}-\d{2}$')
);

-- 0004_events: 事件与订阅
--
-- 见 docs/架构方案.md §3.5。任务用订阅表达式声明它在等什么，
-- 事件到来时匹配订阅并唤醒任务，而不是每分钟把所有数据捞一遍。

CREATE TABLE IF NOT EXISTS events (
    id           TEXT PRIMARY KEY,
    topic        TEXT        NOT NULL,
    payload      JSONB,

    -- 事件内容的信任等级。事件能唤醒任务，但唤醒后的内容依然按这个等级对待。
    trust        SMALLINT    NOT NULL,
    source       TEXT        NOT NULL DEFAULT '',

    occurred_at  TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS events_topic_idx ON events (topic, occurred_at DESC);


CREATE TABLE IF NOT EXISTS event_subscriptions (
    id            TEXT PRIMARY KEY,
    task_id       TEXT        NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,

    topic_prefix  TEXT        NOT NULL DEFAULT '',
    expr          TEXT        NOT NULL,
    once          BOOLEAN     NOT NULL DEFAULT true,

    -- 兜底过期时间：没有它，被取消的任务会永远留着订阅。
    expires_at    TIMESTAMPTZ,
    active        BOOLEAN     NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL,

    CONSTRAINT event_subscriptions_expr_not_empty CHECK (expr <> '')
);

-- 每个事件都要扫一遍有效订阅。
CREATE INDEX IF NOT EXISTS event_subscriptions_active_idx
    ON event_subscriptions (expires_at)
    WHERE active;

CREATE INDEX IF NOT EXISTS event_subscriptions_task_idx
    ON event_subscriptions (task_id)
    WHERE active;

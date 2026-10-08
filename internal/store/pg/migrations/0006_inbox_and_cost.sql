-- 0006_inbox_and_cost: 任务收件箱、不足一分的开销零头

-- 任务不在等待时收到的消息先挂在这里，任务下一次推进时取走。
-- 取走后置回 NULL，事件本身保留。
ALTER TABLE events ADD COLUMN IF NOT EXISTS inbox_task_id TEXT REFERENCES tasks (id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS events_inbox_idx
    ON events (inbox_task_id, occurred_at)
    WHERE inbox_task_id IS NOT NULL;

-- 单次模型调用常常不到一分钱。零头单位为百万分之一元，满一分进位到 usage_cost_cents。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS usage_cost_micros BIGINT NOT NULL DEFAULT 0;

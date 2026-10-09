-- 0011_channels: 消息渠道的运行状态
--
-- 渠道本身（接了哪些、主人是谁）在配置文件里，由用户亲手写；这里只存运行中产生的状态。
-- 见 internal/channel。

-- 每个会话最近一次来信。有的渠道回复时要带上来信的编号和一个递增的序号。
CREATE TABLE IF NOT EXISTS channel_chats (
    channel      TEXT        NOT NULL,
    chat         TEXT        NOT NULL,
    last_msg_id  TEXT        NOT NULL DEFAULT '',
    last_msg_at  TIMESTAMPTZ NOT NULL,
    -- 对最近那条来信已经回应了几次。
    seq          INT         NOT NULL DEFAULT 0,
    PRIMARY KEY (channel, chat)
);

-- 每个任务上一次推给用户的进展的指纹。推过的不再推，进程重启也不会重复打扰。
CREATE TABLE IF NOT EXISTS channel_notices (
    task_id      TEXT PRIMARY KEY,
    fingerprint  TEXT        NOT NULL,
    notified_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 按"来自哪个会话"找任务，以及找出最近有变化的任务，都靠标签。
CREATE INDEX IF NOT EXISTS tasks_tags ON tasks USING GIN (tags);

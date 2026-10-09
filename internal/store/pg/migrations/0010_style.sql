-- 0010_style: 用户的表达风格
--
-- 样本是用户亲手写的话，概述是从样本里提炼出来的规律。见 internal/style。

CREATE TABLE IF NOT EXISTS style_samples (
    id          TEXT PRIMARY KEY,

    -- 场景：chat（对助手说话）/ mail（给别人写邮件）。说话方式因对象而异，分开攒。
    scene       TEXT        NOT NULL,
    text        TEXT        NOT NULL,

    -- 场景 + 原文的摘要。同一个场景里一模一样的话只留一条。
    digest      TEXT        NOT NULL UNIQUE,

    -- 来源，如 task:任务ID、mail:邮件ID。
    ref         TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS style_samples_recent ON style_samples (scene, created_at DESC);

CREATE TABLE IF NOT EXISTS style_profiles (
    scene        TEXT PRIMARY KEY,
    description  TEXT        NOT NULL,

    -- 提炼时见过的最新一条样本的时间。之后攒够一批新样本就重新提炼。
    up_to        TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL
);

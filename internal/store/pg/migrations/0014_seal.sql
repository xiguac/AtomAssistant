-- 0014_seal: 内容列落库加密的配套改动
--
-- 加密本身不改表结构：密文仍然存在原来的列里（JSONB 列里存的是一个 JSON 字符串）。
-- 这里只处理加密之后数据库没法再从内容里取的东西，以及两张新表。

-- 唤醒时间原来是从 wake 这个 JSON 里现取的，wake 加密后取不到了，单独存一列。
-- 时间是调度要用的，不算内容。迁移在补加密之前执行，此时 wake 还是明文。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS wake_at TIMESTAMPTZ;
UPDATE tasks SET wake_at = (wake ->> 'at')::timestamptz
 WHERE wake_at IS NULL AND jsonb_typeof(wake) = 'object' AND wake ->> 'at' IS NOT NULL;

DROP INDEX IF EXISTS tasks_wake_at_idx;
CREATE INDEX IF NOT EXISTS tasks_wake_at_idx
    ON tasks (wake_at)
    WHERE state = 'waiting' AND wake_at IS NOT NULL;

-- 库里的内容是用哪把密钥加密的。只有一行；没有这一行表示内容还没有加密过。
CREATE TABLE IF NOT EXISTS seal_state (
    id         SMALLINT PRIMARY KEY CHECK (id = 1),
    key_id     TEXT        NOT NULL,
    sealed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 后台自己要记的设置：登录方式、动态验证码的种子、登记过的通行密钥。值是加密的。
CREATE TABLE IF NOT EXISTS settings (
    key         TEXT PRIMARY KEY,
    value       TEXT        NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

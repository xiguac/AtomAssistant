-- 0015_task_images: 用户发给任务的图片
--
-- 图片原样存着，任务每一步决策时随上下文一起交给模型看。内容（data）是加密的，
-- 存的是 Base64 文字，和别的内容列用同一套办法加解密。
CREATE TABLE IF NOT EXISTS task_images (
    id          TEXT PRIMARY KEY,
    task_id     TEXT        NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    mime        TEXT        NOT NULL,
    data        TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS task_images_task_idx ON task_images (task_id, created_at);

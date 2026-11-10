-- 0027_matters: 事项清单
--
-- 用户交代过的事，一件一行。任务是一次执行，事项是用户说的"一件事"：同一件事可以先后有好几个任务。
-- 任务靠标签 matter:<id> 挂到事项上，不改 tasks 表。见 internal/matter。
-- 标题、进展、会话标识是加密的；进展是一段 JSON 文字。阶段（低优先、归档）不存：按 last_active_at 算。

CREATE TABLE IF NOT EXISTS matters (
    id              TEXT PRIMARY KEY,
    -- 挂在哪件长期的事下面，没有为空。
    parent_id       TEXT        NOT NULL DEFAULT '',
    kind            TEXT        NOT NULL,
    title           TEXT        NOT NULL,
    notes           TEXT        NOT NULL DEFAULT '',
    -- 用户最近在哪个会话里说到它，要问用户话时发到这里。
    channel         TEXT        NOT NULL DEFAULT '',
    chat            TEXT        NOT NULL DEFAULT '',
    last_active_at  TIMESTAMPTZ NOT NULL,
    -- 已经问过用户这件事还要不要留着、还没等到回答时非空。
    asked_at        TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL,
    CONSTRAINT matters_kind_valid CHECK (kind IN ('once', 'ongoing'))
);

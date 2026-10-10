-- 0012_team: 多模型协作
--
-- 主 AI 派给协作模型的每一份活，以及用户对它的评价。成绩是从这张表里算出来的，
-- 不另存一份——评价改了，成绩自然跟着变。见 internal/team。

CREATE TABLE IF NOT EXISTS team_work (
    -- 任务ID:步骤序号:第几份。同一步重跑时不变，所以不会重复计数。
    id          TEXT PRIMARY KEY,
    task_id     TEXT        NOT NULL,
    step_seq    INT         NOT NULL,
    idx         INT         NOT NULL,

    -- 协作模型的名字（用户在配置里起的）和这份活的类别。
    worker      TEXT        NOT NULL,
    category    TEXT        NOT NULL,
    title       TEXT        NOT NULL DEFAULT '',

    -- 为正表示做得好，为负表示不行，绝对值是分量；0 表示还没有人评价。
    rating      DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- user（用户明确表态）/ inferred（从用户的话里推断）/ 空（没评价）。
    rated_by    TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS team_work_task ON team_work (task_id, step_seq, idx);

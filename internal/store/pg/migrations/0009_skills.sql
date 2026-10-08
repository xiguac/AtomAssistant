-- 0009_skills: 已安装的 Skill
--
-- Skill 是声明式流程，不含可执行代码。见 internal/skill。

CREATE TABLE IF NOT EXISTS skills (
    id              TEXT PRIMARY KEY,
    name            TEXT        NOT NULL,
    version         TEXT        NOT NULL DEFAULT '',
    goal            TEXT        NOT NULL,
    author          TEXT        NOT NULL DEFAULT '',

    -- 运行时可用的工具 = 这张名单 ∩ 用户已授权的工具。
    required_tools  TEXT[]      NOT NULL DEFAULT '{}',
    steps           TEXT[]      NOT NULL,

    -- 清单原文与安装前的扫描报告，留作事后追溯。
    raw             TEXT        NOT NULL,
    scan_report     TEXT        NOT NULL DEFAULT '',

    installed_at    TIMESTAMPTZ NOT NULL,

    CONSTRAINT skills_has_steps CHECK (cardinality(steps) > 0)
);

-- 助手自己找来并安装的能力：公开 Skill 库里的 SKILL.md，以及公开登记的远程 MCP server。

-- SKILL.md 没有步骤列表，原来"至少一个步骤"的约束只对声明式清单成立，改由代码校验。
ALTER TABLE skills DROP CONSTRAINT IF EXISTS skills_has_steps;
ALTER TABLE skills ADD COLUMN IF NOT EXISTS format TEXT NOT NULL DEFAULT 'manifest';
ALTER TABLE skills ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT '';

-- 助手安装的 MCP server 单独存放，不写进配置文件：
-- 配置文件里的 server 是用户亲手批准的，授权边界由他定；这里的一律按固定的保守边界对待。
CREATE TABLE IF NOT EXISTS mcp_installs (
    name          TEXT PRIMARY KEY,
    url           TEXT        NOT NULL,
    -- 它在公开登记处的名字与说明，仅作展示与追溯。
    registry_name TEXT        NOT NULL,
    description   TEXT        NOT NULL DEFAULT '',
    -- 安装时登记处给出的条目原文，以及安装前的扫描报告。
    payload       TEXT        NOT NULL,
    scan_report   TEXT        NOT NULL DEFAULT '',
    installed_at  TIMESTAMPTZ NOT NULL
);

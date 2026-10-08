-- 0002_memory: 长期记忆、版本链、待裁决候选
--
-- 设计要点见 docs/架构方案.md §4.5：
-- memory 的 ID 稳定，值随 revision 演进；旧值不被物理删除。

CREATE TABLE IF NOT EXISTS memories (
    id               TEXT PRIMARY KEY,

    kind             TEXT        NOT NULL,
    subject          TEXT        NOT NULL,
    domain           TEXT        NOT NULL DEFAULT '',
    attribute        TEXT        NOT NULL DEFAULT '',
    -- 偏好的生效条件。条件参与身份：{场景:通勤} 和 {场景:接待客户} 是两条并存的记忆。
    condition        JSONB       NOT NULL DEFAULT '{}'::jsonb,

    -- identity_key 由 kind/subject/domain/attribute(/condition) 派生，
    -- 同键上的新值构成版本演进而不是新增一条。
    identity_key     TEXT        NOT NULL UNIQUE,

    status           TEXT        NOT NULL,
    temporality      TEXT        NOT NULL,
    valid_from       TIMESTAMPTZ NOT NULL,
    valid_until      TIMESTAMPTZ,

    confidence       DOUBLE PRECISION NOT NULL,
    hit_count        INTEGER     NOT NULL DEFAULT 0,
    last_hit_at      TIMESTAMPTZ,

    -- 安全相关属性（过敏、禁忌、疾病、财务红线）。走旁路强制注入，永不归档。
    safety_critical  BOOLEAN     NOT NULL DEFAULT FALSE,
    tags             TEXT[]      NOT NULL DEFAULT '{}',

    current_revision_id TEXT     NOT NULL,

    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,

    CONSTRAINT memories_kind_valid CHECK (
        kind IN ('preference', 'entity', 'fact', 'playbook', 'episode')
    ),
    CONSTRAINT memories_status_valid CHECK (
        status IN ('active', 'disputed', 'expired', 'archived')
    ),
    CONSTRAINT memories_temporality_valid CHECK (
        temporality IN ('permanent', 'durable', 'limited', 'one_shot')
    ),
    -- 限时记忆没有过期时间就等于永久记忆，三个月后它就是一条错误信息。
    -- 这条和应用层的校验重复，是刻意的：应用层有 bug 也写不进来。
    CONSTRAINT memories_limited_needs_until CHECK (
        temporality <> 'limited' OR valid_until IS NOT NULL
    ),
    CONSTRAINT memories_confidence_range CHECK (confidence >= 0 AND confidence <= 1)
);

CREATE INDEX IF NOT EXISTS memories_subject_idx ON memories (subject, status);
CREATE INDEX IF NOT EXISTS memories_domain_idx ON memories (subject, domain);
CREATE INDEX IF NOT EXISTS memories_tags_idx ON memories USING GIN (tags);

-- 安全属性要被单独、快速地全量捞出来做强制注入。
CREATE INDEX IF NOT EXISTS memories_safety_idx
    ON memories (subject)
    WHERE safety_critical;

-- 维护任务扫描到期记忆。
CREATE INDEX IF NOT EXISTS memories_valid_until_idx
    ON memories (valid_until)
    WHERE valid_until IS NOT NULL;


CREATE TABLE IF NOT EXISTS memory_revisions (
    id               TEXT PRIMARY KEY,
    memory_id        TEXT        NOT NULL REFERENCES memories (id) ON DELETE CASCADE,

    value            TEXT        NOT NULL,

    source_kind      TEXT        NOT NULL,
    source_ref       TEXT        NOT NULL DEFAULT '',
    source_trust     SMALLINT    NOT NULL,

    evidence         TEXT[]      NOT NULL DEFAULT '{}',
    confidence       DOUBLE PRECISION NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL,

    -- 非空表示已被取代。旧值永不物理删除：
    -- 用户可以推翻助手的记忆，但推翻过程本身必须可回溯。
    superseded_by    TEXT,
    override_reason  TEXT        NOT NULL DEFAULT '',

    CONSTRAINT memory_revisions_source_valid CHECK (
        source_kind IN ('user', 'task', 'connector', 'observed')
    )
);

CREATE INDEX IF NOT EXISTS memory_revisions_memory_idx
    ON memory_revisions (memory_id, created_at);


-- 待用户裁决的候选：冲突未决、或来源不可信需要确认。
CREATE TABLE IF NOT EXISTS memory_pending (
    id           TEXT PRIMARY KEY,
    -- 非空表示与某条既有记忆冲突。
    memory_id    TEXT REFERENCES memories (id) ON DELETE CASCADE,
    subject      TEXT        NOT NULL,
    candidate    JSONB       NOT NULL,
    arbitration  JSONB       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    resolved_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS memory_pending_open_idx
    ON memory_pending (subject, created_at)
    WHERE resolved_at IS NULL;

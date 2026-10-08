-- 0001_init: 任务控制块、步骤日志、审计
--
-- 设计要点见 docs/架构方案.md §3.3：
-- 任务的真身是这张表里的一行，不是内存里的 goroutine。

CREATE TABLE IF NOT EXISTS tasks (
    id                 TEXT PRIMARY KEY,
    parent_id          TEXT REFERENCES tasks (id) ON DELETE SET NULL,

    title              TEXT        NOT NULL,
    goal               TEXT        NOT NULL DEFAULT '',
    state              TEXT        NOT NULL,
    reason             TEXT        NOT NULL DEFAULT '',

    -- 域决定默认可见性，而不是反过来。personal 一律 private，见 docs/协作与联邦.md §4.1。
    domain             TEXT        NOT NULL,
    visibility         TEXT        NOT NULL,
    visibility_target  TEXT        NOT NULL DEFAULT '',

    -- 创建这个任务的来源信任等级。untrusted 来源的不可逆操作不适用任何自动放行。
    origin_trust       SMALLINT    NOT NULL,

    priority           INTEGER     NOT NULL DEFAULT 0,
    tags               TEXT[]      NOT NULL DEFAULT '{}',

    budget_max_tokens      BIGINT  NOT NULL DEFAULT 0,
    budget_max_cost_cents  BIGINT  NOT NULL DEFAULT 0,
    budget_max_wall_ns     BIGINT  NOT NULL DEFAULT 0,
    usage_tokens           BIGINT  NOT NULL DEFAULT 0,
    usage_cost_cents       BIGINT  NOT NULL DEFAULT 0,
    usage_wall_ns          BIGINT  NOT NULL DEFAULT 0,

    deadline           TIMESTAMPTZ,
    wake               JSONB,

    -- next_seq 只是缓存值。真正的序号来源是 task_steps，见 kernel.nextSeq。
    next_seq           INTEGER     NOT NULL DEFAULT 1,

    lease_owner        TEXT        NOT NULL DEFAULT '',
    lease_expires_at   TIMESTAMPTZ,

    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL,

    CONSTRAINT tasks_state_valid CHECK (
        state IN ('new', 'ready', 'running', 'waiting', 'suspended', 'done', 'failed', 'cancelled')
    ),
    CONSTRAINT tasks_domain_valid CHECK (domain IN ('personal', 'work', 'mixed')),
    CONSTRAINT tasks_visibility_valid CHECK (visibility IN ('private', 'circle', 'user')),

    -- 域约束下沉到数据库：非工作域的任务不可能被设置成可共享，
    -- 即使应用层有 bug 也写不进去。
    CONSTRAINT tasks_personal_stays_private CHECK (
        domain = 'work' OR visibility = 'private'
    ),
    CONSTRAINT tasks_waiting_needs_wake CHECK (
        state <> 'waiting' OR wake IS NOT NULL
    )
);

-- 调度器领取 ready 任务：优先级降序、创建时间升序，保证老任务不被饿死。
CREATE INDEX IF NOT EXISTS tasks_ready_idx
    ON tasks (priority DESC, created_at)
    WHERE state = 'ready';

-- 崩溃恢复：扫描租约过期的 running 任务。
CREATE INDEX IF NOT EXISTS tasks_lease_idx
    ON tasks (lease_expires_at)
    WHERE state = 'running';

-- 定时唤醒：扫描到点的 waiting 任务。
CREATE INDEX IF NOT EXISTS tasks_wake_at_idx
    ON tasks (((wake ->> 'at')))
    WHERE state = 'waiting';


CREATE TABLE IF NOT EXISTS task_steps (
    id               TEXT PRIMARY KEY,
    task_id          TEXT        NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    seq              INTEGER     NOT NULL,

    type             TEXT        NOT NULL,
    status           TEXT        NOT NULL,
    tool_name        TEXT        NOT NULL DEFAULT '',

    -- 决定崩溃恢复时能否自动重放。不可逆操作绝不自动重试。
    side_effect      TEXT        NOT NULL,

    input_hash       TEXT        NOT NULL DEFAULT '',
    input            JSONB,
    output           JSONB,
    output_trust     SMALLINT    NOT NULL DEFAULT 0,

    -- 由 task_id + seq 哈希派生，透传给外部服务用于去重。
    idempotency_key  TEXT        NOT NULL,

    tokens_in        BIGINT      NOT NULL DEFAULT 0,
    tokens_out       BIGINT      NOT NULL DEFAULT 0,
    cost_cents       BIGINT      NOT NULL DEFAULT 0,

    err              TEXT        NOT NULL DEFAULT '',

    started_at       TIMESTAMPTZ NOT NULL,
    ended_at         TIMESTAMPTZ,

    CONSTRAINT task_steps_seq_unique UNIQUE (task_id, seq),
    CONSTRAINT task_steps_status_valid CHECK (
        status IN ('pending', 'done', 'failed', 'skipped')
    ),
    CONSTRAINT task_steps_side_effect_valid CHECK (
        side_effect IN ('none', 'read_only', 'reversible', 'irreversible')
    ),
    -- 静默失败是被禁止的：失败步骤必须带原因。
    CONSTRAINT task_steps_failed_needs_reason CHECK (
        status <> 'failed' OR err <> ''
    )
);

CREATE INDEX IF NOT EXISTS task_steps_task_seq_idx ON task_steps (task_id, seq);

-- 恢复时定位遗留的 pending 步骤。
CREATE INDEX IF NOT EXISTS task_steps_pending_idx
    ON task_steps (task_id)
    WHERE status = 'pending';


-- 一切留痕。这张表是"隐私账本"视图的数据来源。
CREATE TABLE IF NOT EXISTS audit_log (
    id            TEXT PRIMARY KEY,
    at            TIMESTAMPTZ NOT NULL,
    actor         TEXT        NOT NULL,
    action        TEXT        NOT NULL,
    task_id       TEXT        NOT NULL DEFAULT '',
    detail        TEXT        NOT NULL DEFAULT '',
    trust_at_op   SMALLINT    NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS audit_log_at_idx ON audit_log (at DESC);
CREATE INDEX IF NOT EXISTS audit_log_task_idx ON audit_log (task_id, at DESC);

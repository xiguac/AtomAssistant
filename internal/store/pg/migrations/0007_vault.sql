-- 0007_vault: 隔离保险库
--
-- 这张表里只有密文。密钥只存在于环境变量 ATOM_VAULT_KEY，不在库里——
-- 数据库被整个拖走，拿到的也只是解不开的密文。见 internal/vault。

CREATE TABLE IF NOT EXISTS vault_items (
    id             TEXT PRIMARY KEY,
    label          TEXT        NOT NULL UNIQUE,

    -- 给人和模型看的用途说明，不是秘密。
    note           TEXT        NOT NULL DEFAULT '',

    -- 可以使用这一条的工具全名。空数组表示谁都不能用。只能由用户在后台修改。
    allowed_tools  TEXT[]      NOT NULL DEFAULT '{}',

    sealed         BYTEA       NOT NULL,
    nonce          BYTEA       NOT NULL,

    created_at     TIMESTAMPTZ NOT NULL
);

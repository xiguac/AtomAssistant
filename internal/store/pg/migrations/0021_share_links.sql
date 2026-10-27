-- 0021_share_links: 工作区文件的临时下载链接
--
-- 见 internal/share。令牌本身不存，只存它的 SHA-256；分享的路径和文件清单是加密的。
-- 链接最长 24 小时，过期的在建新链接时顺手删掉。

CREATE TABLE IF NOT EXISTS share_links (
    id          TEXT PRIMARY KEY,
    token_hash  TEXT        NOT NULL UNIQUE,
    task_id     TEXT        NOT NULL DEFAULT '',
    path        TEXT        NOT NULL,
    is_dir      BOOLEAN     NOT NULL DEFAULT false,
    -- 文件清单：路径、大小和建链接时的内容摘要，JSON 数组。
    files       TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    CONSTRAINT share_links_ttl_valid CHECK (expires_at > created_at AND expires_at <= created_at + interval '24 hours')
);

CREATE INDEX IF NOT EXISTS share_links_expires_idx ON share_links (expires_at);

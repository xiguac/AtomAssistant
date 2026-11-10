-- 0024_devices: 手机端 App 作为数据源
--
-- 见 internal/device。设备令牌和配对码本身不存，只存它们的 SHA-256；
-- 设备名、上报的内容、授权需求的原因与参数是加密的。

CREATE TABLE IF NOT EXISTS devices (
    id              TEXT PRIMARY KEY,
    name            TEXT        NOT NULL,
    -- 配对码的摘要，配对成功后清空；过了 pair_expires_at 就不能再用。
    pair_hash       TEXT        UNIQUE,
    pair_expires_at TIMESTAMPTZ,
    -- 设备令牌的摘要，配对成功后才有。
    token_hash      TEXT        UNIQUE,
    model           TEXT        NOT NULL DEFAULT '',
    os              TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL,
    paired_at       TIMESTAMPTZ,
    last_seen_at    TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS device_requests (
    id          TEXT PRIMARY KEY,
    device_id   TEXT        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind        TEXT        NOT NULL,
    reason      TEXT        NOT NULL,
    params      TEXT        NOT NULL,
    task_id     TEXT        NOT NULL DEFAULT '',
    -- pending 等用户授权；done 已采集上传；declined 用户拒绝。过期的不改状态，按 expires_at 判断。
    status      TEXT        NOT NULL DEFAULT 'pending',
    created_at  TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    answered_at TIMESTAMPTZ,
    CONSTRAINT device_requests_status_valid CHECK (status IN ('pending', 'done', 'declined'))
);

CREATE INDEX IF NOT EXISTS device_requests_device_idx ON device_requests (device_id, status, expires_at);

CREATE TABLE IF NOT EXISTS device_data (
    id          BIGSERIAL PRIMARY KEY,
    device_id   TEXT        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind        TEXT        NOT NULL,
    -- 设备给每条数据起的稳定标识，重传同一条不会重复入库。
    item_key    TEXT        NOT NULL,
    request_id  TEXT        NOT NULL DEFAULT '',
    captured_at TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    payload     TEXT        NOT NULL,
    UNIQUE (device_id, kind, item_key)
);

CREATE INDEX IF NOT EXISTS device_data_kind_idx ON device_data (kind, captured_at DESC);

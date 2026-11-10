-- 0025_device_sync_interval: 每台手机的上报间隔，在后台设置，App 每次同步时读取
--
-- 单位是分钟，默认 20。允许的取值见 internal/device 的 SyncChoices。

ALTER TABLE devices ADD COLUMN IF NOT EXISTS sync_minutes INTEGER NOT NULL DEFAULT 20;

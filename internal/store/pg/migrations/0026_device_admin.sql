-- 0026_device_admin: 允许某台手机通过 App 管理后台
--
-- 默认关。光有设备令牌只能上报数据；在后台「手机」页打开这个开关，那台手机的 App 才能用管理接口。

ALTER TABLE devices ADD COLUMN IF NOT EXISTS admin_allowed BOOLEAN NOT NULL DEFAULT false;

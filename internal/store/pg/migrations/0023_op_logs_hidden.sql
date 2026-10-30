-- 0023_op_logs_hidden: 日志可以在后台按时间范围删除
--
-- 后台操作类的日志涉及安全，删除时只标记为不显示，服务器上照样留满 300 天才真正删掉；
-- 其余类别直接删除。
ALTER TABLE op_logs ADD COLUMN IF NOT EXISTS hidden BOOLEAN NOT NULL DEFAULT false;

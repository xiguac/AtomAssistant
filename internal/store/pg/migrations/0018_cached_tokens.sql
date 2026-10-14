-- 0018_cached_tokens: 任务用量里命中模型服务前缀缓存的那部分输入
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS usage_cached_tokens BIGINT NOT NULL DEFAULT 0;

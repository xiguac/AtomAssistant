-- 0016_mcp_by_user: 用户在对话里给出地址、让助手装上的 MCP server
--
-- 它和助手自己找来装的存在同一张表里，但不是一回事：地址是用户亲口给的，按用户接入的对待，
-- 不套"助手自己装的"那套固定的保守边界。它要的密钥存在保险库里，这里只留句柄和参数名。
ALTER TABLE mcp_installs ADD COLUMN IF NOT EXISTS by_user     BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE mcp_installs ADD COLUMN IF NOT EXISTS secret_name TEXT    NOT NULL DEFAULT '';
ALTER TABLE mcp_installs ADD COLUMN IF NOT EXISTS secret      TEXT    NOT NULL DEFAULT '';

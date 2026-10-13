-- 0017_mcp_secrets: 对话里安装的 MCP server 可以要不止一项密钥
--
-- 有的 server 要两三样（如 ID 加密钥）。第一项仍在 secret_name / secret 两列里，
-- 其余的以 JSON 存在这一列：[{"name":参数名,"secret":保险库句柄}]。这里同样只有句柄，没有值。
ALTER TABLE mcp_installs ADD COLUMN IF NOT EXISTS secrets TEXT NOT NULL DEFAULT '';

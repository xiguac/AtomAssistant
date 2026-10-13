package pg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiguac/atomhelper/internal/market"
)

// MCPInstallStore 是 market.ServerStore 的 Postgres 实现。
type MCPInstallStore struct {
	pool *pgxpool.Pool
	z    sealer
}

// MCPInstalls 返回基于同一连接池的、助手安装的 MCP server 的存储。
func (s *Store) MCPInstalls() *MCPInstallStore { return &MCPInstallStore{pool: s.pool, z: s.z} }

var _ market.ServerStore = (*MCPInstallStore)(nil)

// PutServer 写入一个 server，同名则整条替换。
func (s *MCPInstallStore) PutServer(ctx context.Context, srv *market.Server) error {
	z := s.z
	extra := ""
	if len(srv.Secrets) > 0 {
		raw, err := json.Marshal(srv.Secrets)
		if err != nil {
			return fmt.Errorf("pg: 序列化 MCP server 的密钥参数失败: %w", err)
		}
		extra = string(raw)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mcp_installs (name, url, registry_name, description, payload, scan_report, installed_at,
		                          by_user, secret_name, secret, secrets)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (name) DO UPDATE
		   SET url = EXCLUDED.url, registry_name = EXCLUDED.registry_name, description = EXCLUDED.description,
		       payload = EXCLUDED.payload, scan_report = EXCLUDED.scan_report, installed_at = EXCLUDED.installed_at,
		       by_user = EXCLUDED.by_user, secret_name = EXCLUDED.secret_name, secret = EXCLUDED.secret,
		       secrets = EXCLUDED.secrets`,
		srv.Name, z.str("mcp_installs.url", srv.URL), z.str("mcp_installs.registry_name", srv.RegistryName),
		z.str("mcp_installs.description", srv.Description), z.str("mcp_installs.payload", srv.Payload),
		z.str("mcp_installs.scan_report", srv.ScanReport), srv.InstalledAt,
		srv.ByUser, z.str("mcp_installs.secret_name", srv.SecretName), z.str("mcp_installs.secret", srv.Secret),
		z.str("mcp_installs.secrets", extra))
	if err != nil {
		return fmt.Errorf("pg: 写入 MCP server 失败: %w", err)
	}
	return nil
}

func (s *MCPInstallStore) ListServers(ctx context.Context) ([]*market.Server, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, url, registry_name, description, payload, scan_report, installed_at,
		       by_user, secret_name, secret, secrets
		  FROM mcp_installs ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取 MCP server 失败: %w", err)
	}
	defer rows.Close()

	out := make([]*market.Server, 0)
	for rows.Next() {
		var srv market.Server
		var extra string
		if err := rows.Scan(&srv.Name, &srv.URL, &srv.RegistryName, &srv.Description,
			&srv.Payload, &srv.ScanReport, &srv.InstalledAt, &srv.ByUser, &srv.SecretName, &srv.Secret, &extra); err != nil {
			return nil, fmt.Errorf("pg: 解析 MCP server 失败: %w", err)
		}
		if err := s.z.opens("mcp_installs.url", &srv.URL, "mcp_installs.registry_name", &srv.RegistryName,
			"mcp_installs.description", &srv.Description, "mcp_installs.payload", &srv.Payload,
			"mcp_installs.scan_report", &srv.ScanReport,
			"mcp_installs.secret_name", &srv.SecretName, "mcp_installs.secret", &srv.Secret,
			"mcp_installs.secrets", &extra); err != nil {
			return nil, fmt.Errorf("pg: 解析 MCP server 失败: %w", err)
		}
		if extra != "" {
			if err := json.Unmarshal([]byte(extra), &srv.Secrets); err != nil {
				return nil, fmt.Errorf("pg: 解析 MCP server %s 的密钥参数失败: %w", srv.Name, err)
			}
		}
		out = append(out, &srv)
	}
	return out, rows.Err()
}

func (s *MCPInstallStore) RemoveServer(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM mcp_installs WHERE name = $1`, name)
	if err != nil {
		return fmt.Errorf("pg: 删除 MCP server 失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return market.ErrNotInstalled
	}
	return nil
}

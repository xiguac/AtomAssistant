package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiguac/atomhelper/internal/market"
)

// MCPInstallStore 是 market.ServerStore 的 Postgres 实现。
type MCPInstallStore struct {
	pool *pgxpool.Pool
}

// MCPInstalls 返回基于同一连接池的、助手安装的 MCP server 的存储。
func (s *Store) MCPInstalls() *MCPInstallStore { return &MCPInstallStore{pool: s.pool} }

var _ market.ServerStore = (*MCPInstallStore)(nil)

// PutServer 写入一个 server，同名则整条替换。
func (s *MCPInstallStore) PutServer(ctx context.Context, srv *market.Server) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mcp_installs (name, url, registry_name, description, payload, scan_report, installed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (name) DO UPDATE
		   SET url = EXCLUDED.url, registry_name = EXCLUDED.registry_name, description = EXCLUDED.description,
		       payload = EXCLUDED.payload, scan_report = EXCLUDED.scan_report, installed_at = EXCLUDED.installed_at`,
		srv.Name, srv.URL, srv.RegistryName, srv.Description, srv.Payload, srv.ScanReport, srv.InstalledAt)
	if err != nil {
		return fmt.Errorf("pg: 写入 MCP server 失败: %w", err)
	}
	return nil
}

func (s *MCPInstallStore) ListServers(ctx context.Context) ([]*market.Server, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, url, registry_name, description, payload, scan_report, installed_at
		  FROM mcp_installs ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取 MCP server 失败: %w", err)
	}
	defer rows.Close()

	out := make([]*market.Server, 0)
	for rows.Next() {
		var srv market.Server
		if err := rows.Scan(&srv.Name, &srv.URL, &srv.RegistryName, &srv.Description,
			&srv.Payload, &srv.ScanReport, &srv.InstalledAt); err != nil {
			return nil, fmt.Errorf("pg: 解析 MCP server 失败: %w", err)
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

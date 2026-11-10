package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/xiguac/atomassistant/internal/device"
)

var _ device.Store = (*Store)(nil)

const deviceColumns = `id, name, model, os, created_at, paired_at, last_seen_at, revoked_at, pair_expires_at, sync_minutes, admin_allowed`

func (s *Store) scanDevice(row pgx.Row) (*device.Device, error) {
	var d device.Device
	var paired, seen, revoked, pairExp *time.Time
	if err := row.Scan(&d.ID, &d.Name, &d.Model, &d.OS, &d.CreatedAt, &paired, &seen, &revoked, &pairExp, &d.SyncMinutes, &d.Admin); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("pg: 读取设备失败: %w", err)
	}
	if err := s.z.open("devices.name", &d.Name); err != nil {
		return nil, fmt.Errorf("pg: 解开设备名失败: %w", err)
	}
	if paired != nil {
		d.PairedAt = *paired
	}
	if seen != nil {
		d.LastSeenAt = *seen
	}
	if revoked != nil {
		d.RevokedAt = *revoked
	}
	if pairExp != nil {
		d.PairExpiresAt = *pairExp
	}
	return &d, nil
}

// CreateDevice 登记一台待配对的设备。
func (s *Store) CreateDevice(ctx context.Context, d *device.Device, pairHash string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO devices (id, name, pair_hash, pair_expires_at, created_at, sync_minutes)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		d.ID, s.z.str("devices.name", d.Name), pairHash, d.PairExpiresAt, d.CreatedAt, d.SyncMinutes)
	if err != nil {
		return fmt.Errorf("pg: 登记设备失败: %w", err)
	}
	return nil
}

// PairDevice 用配对码摘要认领一台设备。
func (s *Store) PairDevice(ctx context.Context, pairHash, tokenHash, model, os string, now time.Time) (*device.Device, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE devices SET token_hash = $2, model = $3, os = $4, paired_at = $5, last_seen_at = $5,
			pair_hash = NULL, pair_expires_at = NULL
		WHERE pair_hash = $1 AND pair_expires_at > $5 AND revoked_at IS NULL
		RETURNING `+deviceColumns, pairHash, tokenHash, model, os, now)
	d, err := s.scanDevice(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, device.ErrNotFound
	}
	return d, err
}

// DeviceByToken 按令牌摘要找一台配对过且没被吊销的设备。
func (s *Store) DeviceByToken(ctx context.Context, tokenHash string) (*device.Device, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+deviceColumns+` FROM devices WHERE token_hash = $1 AND revoked_at IS NULL`, tokenHash)
	d, err := s.scanDevice(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, device.ErrNotFound
	}
	return d, err
}

// SetSyncMinutes 设置一台设备的上报间隔。
func (s *Store) SetSyncMinutes(ctx context.Context, id string, minutes int) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE devices SET sync_minutes = $2 WHERE id = $1 AND revoked_at IS NULL`, id, minutes)
	if err != nil {
		return false, fmt.Errorf("pg: 设置上报间隔失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetAdminAllowed 打开或关闭一台设备的管理权限。
func (s *Store) SetAdminAllowed(ctx context.Context, id string, allowed bool) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE devices SET admin_allowed = $2 WHERE id = $1 AND revoked_at IS NULL`, id, allowed)
	if err != nil {
		return false, fmt.Errorf("pg: 设置设备管理权限失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// TouchDevice 记下设备刚刚来过。
func (s *Store) TouchDevice(ctx context.Context, id string, now time.Time) error {
	if _, err := s.pool.Exec(ctx, `UPDATE devices SET last_seen_at = $2 WHERE id = $1`, id, now); err != nil {
		return fmt.Errorf("pg: 更新设备在线时间失败: %w", err)
	}
	return nil
}

// ListDevices 列出全部设备，新的在前。
func (s *Store) ListDevices(ctx context.Context) ([]*device.Device, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+deviceColumns+` FROM devices ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("pg: 列出设备失败: %w", err)
	}
	defer rows.Close()
	var out []*device.Device
	for rows.Next() {
		d, err := s.scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 列出设备失败: %w", err)
	}
	return out, nil
}

// RevokeDevice 吊销设备：令牌立刻失效，等它处理的需求一并作废。
func (s *Store) RevokeDevice(ctx context.Context, id string, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("pg: 吊销设备失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE devices SET revoked_at = $2, admin_allowed = false, pair_hash = NULL, pair_expires_at = NULL
		WHERE id = $1 AND revoked_at IS NULL`, id, now)
	if err != nil {
		return false, fmt.Errorf("pg: 吊销设备失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE device_requests SET status = 'declined', answered_at = $2
		WHERE device_id = $1 AND status = 'pending'`, id, now); err != nil {
		return false, fmt.Errorf("pg: 作废设备的待处理需求失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("pg: 吊销设备失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteDevice 删除设备及它的全部数据和需求。
func (s *Store) DeleteDevice(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM devices WHERE id = $1`, id); err != nil {
		return fmt.Errorf("pg: 删除设备失败: %w", err)
	}
	return nil
}

// AddData 存入数据，重复的 Key 跳过。
func (s *Store) AddData(ctx context.Context, deviceID, kind, requestID string, items []device.Item, now time.Time) (int, error) {
	batch := &pgx.Batch{}
	for _, it := range items {
		batch.Queue(`
			INSERT INTO device_data (device_id, kind, item_key, request_id, captured_at, received_at, payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (device_id, kind, item_key) DO NOTHING`,
			deviceID, kind, it.Key, requestID, it.CapturedAt.UTC(), now, s.z.str("device_data.payload", string(it.Payload)))
	}
	res := s.pool.SendBatch(ctx, batch)
	defer res.Close()
	added := 0
	for range items {
		tag, err := res.Exec()
		if err != nil {
			return 0, fmt.Errorf("pg: 保存设备数据失败: %w", err)
		}
		added += int(tag.RowsAffected())
	}
	return added, nil
}

// QueryData 按条件查数据，新的在前。
func (s *Store) QueryData(ctx context.Context, q device.Query) ([]device.Stored, error) {
	until := q.Until
	if until.IsZero() {
		until = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT device_id, kind, captured_at, received_at, request_id, payload FROM device_data
		WHERE ($1 = '' OR kind = $1) AND captured_at >= $2 AND captured_at <= $3 AND ($5 = '' OR device_id = $5)
		ORDER BY captured_at DESC, id DESC LIMIT $4`, q.Kind, q.Since.UTC(), until.UTC(), q.Limit, q.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("pg: 查询设备数据失败: %w", err)
	}
	defer rows.Close()
	var out []device.Stored
	for rows.Next() {
		var d device.Stored
		var payload string
		if err := rows.Scan(&d.DeviceID, &d.Kind, &d.CapturedAt, &d.ReceivedAt, &d.RequestID, &payload); err != nil {
			return nil, fmt.Errorf("pg: 读取设备数据失败: %w", err)
		}
		if err := s.z.open("device_data.payload", &payload); err != nil {
			return nil, fmt.Errorf("pg: 解开设备数据失败: %w", err)
		}
		d.Payload = json.RawMessage(payload)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 读取设备数据失败: %w", err)
	}
	return out, nil
}

// DataStats 统计一台设备上各种数据的条数和最近一条的时间。
func (s *Store) DataStats(ctx context.Context, deviceID string) ([]device.KindStat, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT kind, count(*), max(captured_at) FROM device_data WHERE device_id = $1 GROUP BY kind ORDER BY kind`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("pg: 统计设备数据失败: %w", err)
	}
	defer rows.Close()
	var out []device.KindStat
	for rows.Next() {
		var k device.KindStat
		if err := rows.Scan(&k.Kind, &k.Count, &k.Latest); err != nil {
			return nil, fmt.Errorf("pg: 统计设备数据失败: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteData 删掉某台设备某种数据（kind 为空是全部）。
func (s *Store) DeleteData(ctx context.Context, deviceID, kind string) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM device_data WHERE device_id = $1 AND ($2 = '' OR kind = $2)`, deviceID, kind)
	if err != nil {
		return 0, fmt.Errorf("pg: 删除设备数据失败: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// CreateRequest 存下一条授权需求。
func (s *Store) CreateRequest(ctx context.Context, r *device.Request) error {
	params := string(r.Params)
	if params == "" {
		params = "{}"
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO device_requests (id, device_id, kind, reason, params, task_id, status, created_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		r.ID, r.DeviceID, r.Kind, s.z.str("device_requests.reason", r.Reason), s.z.str("device_requests.params", params),
		r.TaskID, r.Status, r.CreatedAt, r.ExpiresAt)
	if err != nil {
		return fmt.Errorf("pg: 保存授权需求失败: %w", err)
	}
	return nil
}

const requestColumns = `id, device_id, kind, reason, params, task_id, status, created_at, expires_at, answered_at`

func (s *Store) scanRequest(row pgx.Row) (*device.Request, error) {
	var r device.Request
	var params string
	var answered *time.Time
	if err := row.Scan(&r.ID, &r.DeviceID, &r.Kind, &r.Reason, &params, &r.TaskID, &r.Status, &r.CreatedAt, &r.ExpiresAt, &answered); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("pg: 读取授权需求失败: %w", err)
	}
	if err := s.z.opens("device_requests.reason", &r.Reason, "device_requests.params", &params); err != nil {
		return nil, fmt.Errorf("pg: 解开授权需求失败: %w", err)
	}
	r.Params = json.RawMessage(params)
	if answered != nil {
		r.AnsweredAt = *answered
	}
	return &r, nil
}

// PendingRequests 列出此刻还有效、等用户处理的需求，新的在前。
func (s *Store) PendingRequests(ctx context.Context, deviceID string, now time.Time) ([]*device.Request, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+requestColumns+` FROM device_requests
		WHERE status = 'pending' AND expires_at > $2 AND ($1 = '' OR device_id = $1)
		ORDER BY created_at DESC, id`, deviceID, now)
	if err != nil {
		return nil, fmt.Errorf("pg: 列出授权需求失败: %w", err)
	}
	defer rows.Close()
	var out []*device.Request
	for rows.Next() {
		r, err := s.scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 列出授权需求失败: %w", err)
	}
	return out, nil
}

// RequestByID 按编号读一条需求，没有返回 device.ErrNotFound。
func (s *Store) RequestByID(ctx context.Context, id string) (*device.Request, error) {
	r, err := s.scanRequest(s.pool.QueryRow(ctx, `SELECT `+requestColumns+` FROM device_requests WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, device.ErrNotFound
	}
	return r, err
}

// AnswerRequest 把一条等待中且没过期的需求标成 done 或 declined。
func (s *Store) AnswerRequest(ctx context.Context, id, status string, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE device_requests SET status = $2, answered_at = $3
		WHERE id = $1 AND status = 'pending' AND expires_at > $3`, id, status, now)
	if err != nil {
		return false, fmt.Errorf("pg: 更新授权需求失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xiguac/atomassistant/internal/skill"
)

// SkillStore 是 skill.Store 的 Postgres 实现。
type SkillStore struct {
	pool *pgxpool.Pool
	z    sealer
}

// Skills 返回基于同一连接池的 Skill 存储。
func (s *Store) Skills() *SkillStore { return &SkillStore{pool: s.pool, z: s.z} }

var _ skill.Store = (*SkillStore)(nil)

const skillColumns = `id, name, version, goal, author, required_tools, steps, raw, scan_report, installed_at, format, source`

// PutSkill 写入一个 Skill，同 ID 则整条替换。
func (s *SkillStore) PutSkill(ctx context.Context, sk *skill.Skill) error {
	tools := sk.RequiredTools
	if tools == nil {
		tools = []string{}
	}
	format := sk.Format
	if format == "" {
		format = skill.FormatManifest
	}
	z := s.z
	_, err := s.pool.Exec(ctx, `
		INSERT INTO skills (`+skillColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (id) DO UPDATE
		   SET name = EXCLUDED.name, version = EXCLUDED.version, goal = EXCLUDED.goal,
		       author = EXCLUDED.author, required_tools = EXCLUDED.required_tools,
		       steps = EXCLUDED.steps, raw = EXCLUDED.raw, scan_report = EXCLUDED.scan_report,
		       installed_at = EXCLUDED.installed_at, format = EXCLUDED.format, source = EXCLUDED.source`,
		sk.ID, z.str("skills.name", sk.Name), sk.Version, z.str("skills.goal", sk.Goal), z.str("skills.author", sk.Author),
		tools, z.strs("skills.steps", sk.Steps), z.str("skills.raw", sk.Raw), z.str("skills.scan_report", sk.ScanReport),
		sk.InstalledAt, format, z.str("skills.source", sk.Source))
	if err != nil {
		return fmt.Errorf("pg: 写入 Skill 失败: %w", err)
	}
	return nil
}

func (s *SkillStore) GetSkill(ctx context.Context, id string) (*skill.Skill, error) {
	sk, err := s.z.scanSkill(s.pool.QueryRow(ctx, `SELECT `+skillColumns+` FROM skills WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, skill.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pg: 读取 Skill 失败: %w", err)
	}
	return sk, nil
}

func (s *SkillStore) ListSkills(ctx context.Context) ([]*skill.Skill, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+skillColumns+` FROM skills ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取 Skill 失败: %w", err)
	}
	defer rows.Close()

	out := make([]*skill.Skill, 0)
	for rows.Next() {
		sk, err := s.z.scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("pg: 解析 Skill 失败: %w", err)
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

func (s *SkillStore) RemoveSkill(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM skills WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("pg: 删除 Skill 失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return skill.ErrNotFound
	}
	return nil
}

func (z sealer) scanSkill(sc scanner) (*skill.Skill, error) {
	var sk skill.Skill
	if err := sc.Scan(&sk.ID, &sk.Name, &sk.Version, &sk.Goal, &sk.Author,
		&sk.RequiredTools, &sk.Steps, &sk.Raw, &sk.ScanReport, &sk.InstalledAt, &sk.Format, &sk.Source); err != nil {
		return nil, err
	}
	if err := z.opens("skills.name", &sk.Name, "skills.goal", &sk.Goal, "skills.author", &sk.Author,
		"skills.raw", &sk.Raw, "skills.scan_report", &sk.ScanReport, "skills.source", &sk.Source); err != nil {
		return nil, err
	}
	if err := z.openAll("skills.steps", sk.Steps); err != nil {
		return nil, err
	}
	return &sk, nil
}

package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

// 导出导入用于迁移服务器：在旧机器上按板块导出成文件，到新机器上导入。
//
// 行数据直接用 Postgres 的 to_jsonb / jsonb_populate_recordset 往返，
// 所以这里不需要随表结构变化而改——加了新列，导出导入自动带上。

// Section 是可以单独导出导入的一个数据板块。
type Section string

const (
	// SectionMemory 长期记忆：记忆、版本链、待裁决候选，以及学到的表达风格。
	SectionMemory Section = "memory"
	// SectionTasks 任务：任务、步骤日志、审计、定时任务。
	SectionTasks Section = "tasks"
	// SectionEvents 事件：事件与订阅。它们引用任务，导入前任务板块必须已经在库里。
	SectionEvents Section = "events"
	// SectionVault 保险库。导出的是密文，密钥不在其中——迁移时要把 ATOM_VAULT_KEY 一起带过去。
	SectionVault Section = "vault"
	// SectionSkills 已安装的 Skill，连同安装时的清单原文与扫描报告。
	SectionSkills Section = "skills"
)

// sectionTables 列出每个板块的表，顺序即导入顺序：被引用的表在前。
// 表名只来自这里，不接受外部输入，所以拼进 SQL 是安全的。
var sectionTables = map[Section][]string{
	SectionMemory: {"memories", "memory_revisions", "memory_pending", "style_samples", "style_profiles"},
	SectionTasks:  {"tasks", "task_steps", "audit_log", "schedules", "channel_chats", "channel_notices"},
	SectionEvents: {"events", "event_subscriptions"},
	SectionVault:  {"vault_items"},
	SectionSkills: {"skills"},
}

// sectionOrder 是导入全部板块时的顺序：事件引用任务，必须排在任务之后。
var sectionOrder = []Section{SectionMemory, SectionTasks, SectionEvents, SectionVault, SectionSkills}

// Sections 返回全部板块，按导入顺序。
func Sections() []Section { return append([]Section(nil), sectionOrder...) }

// ParseSection 校验板块名。
func ParseSection(name string) (Section, error) {
	if _, ok := sectionTables[Section(name)]; !ok {
		return "", fmt.Errorf("pg: 未知的板块 %q（可选 memory / tasks / events / vault / skills）", name)
	}
	return Section(name), nil
}

const exportFormat = "atomhelper-export"

// exportFile 是导出文件的结构。
type exportFile struct {
	Format     string    `json:"format"`
	ExportedAt time.Time `json:"exported_at"`
	// Migrations 是导出时源库已执行的迁移。导入的目标库必须不比它旧。
	Migrations []string `json:"migrations"`
	// Sections 是 板块 -> 表 -> 行数组。
	Sections map[Section]map[string]json.RawMessage `json:"sections"`
}

// Export 把指定板块导出到 w。
//
// 所有表在同一个快照里读，导出的数据彼此一致——
// 不会出现步骤在、它所属的任务却不在的情况。
func (s *Store) Export(ctx context.Context, sections []Section, w io.Writer) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pg: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY`); err != nil {
		return fmt.Errorf("pg: 设置导出快照失败: %w", err)
	}

	out := exportFile{
		Format:     exportFormat,
		ExportedAt: time.Now().UTC(),
		Sections:   make(map[Section]map[string]json.RawMessage),
	}
	rows, err := tx.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("pg: 读取迁移记录失败: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("pg: 读取迁移记录失败: %w", err)
		}
		out.Migrations = append(out.Migrations, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pg: 读取迁移记录失败: %w", err)
	}

	for _, sec := range sections {
		tables, ok := sectionTables[sec]
		if !ok {
			return fmt.Errorf("pg: 未知的板块 %q", sec)
		}
		out.Sections[sec] = make(map[string]json.RawMessage, len(tables))
		for _, table := range tables {
			var data []byte
			err := tx.QueryRow(ctx,
				`SELECT COALESCE(jsonb_agg(to_jsonb(t)), '[]'::jsonb) FROM `+table+` t`).Scan(&data)
			if err != nil {
				return fmt.Errorf("pg: 导出表 %s 失败: %w", table, err)
			}
			out.Sections[sec][table] = data
		}
	}

	enc := json.NewEncoder(w)
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("pg: 写出导出文件失败: %w", err)
	}
	return nil
}

// ImportReport 是一次导入的结果：每张表新写入了多少行。
type ImportReport struct {
	Inserted map[string]int64
}

// Import 从 r 读入导出文件并写入数据库。
//
// 整个导入在一个事务里，失败就什么都不留下。已存在的行（主键相同）跳过不覆盖，
// 所以同一个文件导两次是安全的，也可以把板块分几次导。
func (s *Store) Import(ctx context.Context, r io.Reader) (ImportReport, error) {
	rep := ImportReport{Inserted: make(map[string]int64)}

	var in exportFile
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return rep, fmt.Errorf("pg: 解析导出文件失败: %w", err)
	}
	if in.Format != exportFormat {
		return rep, fmt.Errorf("pg: 这不是原子助手的导出文件（format=%q）", in.Format)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return rep, fmt.Errorf("pg: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 目标库的表结构不能比导出时旧，否则新列的数据会被悄悄丢掉。
	have := make(map[string]bool)
	rows, err := tx.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return rep, fmt.Errorf("pg: 读取迁移记录失败（目标库是否已执行过迁移？）: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return rep, fmt.Errorf("pg: 读取迁移记录失败: %w", err)
		}
		have[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, fmt.Errorf("pg: 读取迁移记录失败: %w", err)
	}
	var missing []string
	for _, v := range in.Migrations {
		if !have[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return rep, fmt.Errorf("pg: 目标库比导出文件旧，缺少迁移 %v——请先把新服务器上的程序升级到相同或更新的版本", missing)
	}

	for _, sec := range sectionOrder {
		tables, ok := in.Sections[sec]
		if !ok {
			continue
		}
		for _, table := range sectionTables[sec] {
			data, ok := tables[table]
			if !ok {
				continue
			}
			tag, err := tx.Exec(ctx,
				`INSERT INTO `+table+` SELECT * FROM jsonb_populate_recordset(NULL::`+table+`, $1::jsonb)
				 ON CONFLICT DO NOTHING`, []byte(data))
			if err != nil {
				return rep, fmt.Errorf("pg: 导入表 %s 失败: %w", table, err)
			}
			rep.Inserted[table] = tag.RowsAffected()
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return rep, fmt.Errorf("pg: 提交导入失败: %w", err)
	}
	return rep, nil
}

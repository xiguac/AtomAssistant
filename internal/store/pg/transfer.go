package pg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/xiguac/atomassistant/internal/seal"
)

// 导出导入用于迁移服务器：在旧机器上按板块导出成文件，到新机器上导入。
//
// 行数据直接用 Postgres 的 to_jsonb / jsonb_populate_recordset 往返，
// 所以这里不需要随表结构变化而改——加了新列，导出导入自动带上。
//
// 库里的内容是用这台机器的密钥加密的，那把密钥和硬件绑在一起，带不走。所以：
// 导出时先解密成明文，再整个用用户设的口令加密成文件；
// 导入时用口令解开，再用新机器自己的密钥加密后存进去。

// Section 是可以单独导出导入的一个数据板块。
type Section string

const (
	// SectionMemory 长期记忆：记忆、版本链、待裁决候选，以及学到的表达风格。
	SectionMemory Section = "memory"
	// SectionTasks 任务：任务、步骤日志、审计、定时任务，以及用户与助手的会话。
	SectionTasks Section = "tasks"
	// SectionEvents 事件：事件与订阅。它们引用任务，导入前任务板块必须已经在库里。
	SectionEvents Section = "events"
	// SectionVault 保险库。条目的值仍是保险库自己的密文，密钥不在其中——迁移时要把 ATOM_VAULT_KEY 一起带过去。
	SectionVault Section = "vault"
	// SectionSkills 已安装的 Skill 与助手安装的 MCP server，连同安装时的原文与扫描报告。
	SectionSkills Section = "skills"
)

// sectionTables 列出每个板块的表，顺序即导入顺序：被引用的表在前。
// 表名只来自这里，不接受外部输入，所以拼进 SQL 是安全的。
var sectionTables = map[Section][]string{
	SectionMemory: {"memories", "memory_revisions", "memory_pending", "style_samples", "style_profiles"},
	SectionTasks: {"tasks", "task_steps", "audit_log", "schedules", "channel_chats", "channel_notices", "team_work",
		"session_messages", "session_summaries"},
	SectionEvents: {"events", "event_subscriptions"},
	SectionVault:  {"vault_items"},
	SectionSkills: {"skills", "mcp_installs"},
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

const exportFormat = "atomassistant-export"

// exportFile 是导出文件的结构。
type exportFile struct {
	Format     string    `json:"format"`
	ExportedAt time.Time `json:"exported_at"`
	// Migrations 是导出时源库已执行的迁移。导入的目标库必须不比它旧。
	Migrations []string `json:"migrations"`
	// Sections 是 板块 -> 表 -> 行数组。
	Sections map[Section]map[string]json.RawMessage `json:"sections"`
}

// Export 把指定板块导出到 w，整个文件用 passphrase 加密。
//
// 所有表在同一个快照里读，导出的数据彼此一致——
// 不会出现步骤在、它所属的任务却不在的情况。
func (s *Store) Export(ctx context.Context, sections []Section, w io.Writer, passphrase string) error {
	// 口令不合格要在读库之前就报出来。
	if len([]rune(passphrase)) < seal.MinPassphrase {
		return fmt.Errorf("pg: 导出文件的口令至少要 %d 个字符", seal.MinPassphrase)
	}
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
			if data, err = s.z.mapRows(table, data, false); err != nil {
				return fmt.Errorf("pg: 导出表 %s 失败: %w", table, err)
			}
			out.Sections[sec][table] = data
		}
	}

	plain, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("pg: 生成导出文件失败: %w", err)
	}
	sealed, err := seal.SealWithPassphrase(passphrase, plain)
	if err != nil {
		return err
	}
	if _, err := w.Write(sealed); err != nil {
		return fmt.Errorf("pg: 写出导出文件失败: %w", err)
	}
	return nil
}

// mapRows 把一张表的全部行（JSON 数组）里的内容列解密或加密。
func (z sealer) mapRows(table string, data []byte, sealing bool) ([]byte, error) {
	if len(sealedColumns[table]) == 0 {
		return data, nil
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("行数据无法解析: %w", err)
	}
	for _, row := range rows {
		if err := z.mapRow(table, row, sealing); err != nil {
			return nil, err
		}
	}
	return json.Marshal(rows)
}

// ImportReport 是一次导入的结果：每张表新写入了多少行。
type ImportReport struct {
	Inserted map[string]int64
}

// Import 从 r 读入导出文件并写入数据库，passphrase 是导出时设的口令。
//
// 整个导入在一个事务里，失败就什么都不留下。已存在的行（主键相同）跳过不覆盖，
// 所以同一个文件导两次是安全的，也可以把板块分几次导。
//
// 加密功能上线之前导出的文件是没有加密的，仍然可以导入，这时不需要口令。
func (s *Store) Import(ctx context.Context, r io.Reader, passphrase string) (ImportReport, error) {
	rep := ImportReport{Inserted: make(map[string]int64)}

	raw, err := io.ReadAll(r)
	if err != nil {
		return rep, fmt.Errorf("pg: 读取导出文件失败: %w", err)
	}
	if seal.IsPassphraseSealed(raw) {
		if raw, err = seal.OpenWithPassphrase(passphrase, raw); err != nil {
			return rep, err
		}
	}
	var in exportFile
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&in); err != nil {
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
			sealed, err := s.z.mapRows(table, data, true)
			if err != nil {
				return rep, fmt.Errorf("pg: 导入表 %s 失败: %w", table, err)
			}
			data = sealed
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

package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/xiguac/atomhelper/internal/seal"
)

// 落库即加密，出库即解密。
//
// 凡是"内容"的列——任务写了什么、步骤输入输出了什么、记住了什么——写进数据库之前先加密，
// 读出来再解密，上层的代码看到的始终是明文。调度要用的列保持原样：编号、状态、时间、
// 计数、工具名这些不加密，否则数据库没法按它们筛选和排序，任务也就调度不了。
//
// 要按值查找的内容列（记忆的主体、任务的标签）用"同值同密文"的方式加密，
// 查的时候把要找的值同样加密一遍再比。

// colKind 是一个加密列的形态。
type colKind int

const (
	// colText 是一段文字，每次加密结果不同。
	colText colKind = iota
	// colTextEq 是一段要按值查找的文字，同值同密文。
	colTextEq
	// colArray 是文字数组，逐个元素加密。
	colArray
	// colArrayEq 是要按元素查找的文字数组。
	colArrayEq
	// colJSON 是 JSONB 列：整段内容加密后存成一个 JSON 字符串。
	colJSON
)

// sealedColumns 列出全部加密列。表名和列名只来自这里，不接受外部输入。
//
// 存储代码里写的位置名（如 "tasks.goal"）必须与这里一致；导出导入、
// 以及给升级前的旧数据补加密，都按这张表办。新增内容列时要记得加进来——
// 有一个测试会把每张表读一遍，发现这里列出的列里有明文就报错。
var sealedColumns = map[string]map[string]colKind{
	"tasks": {"title": colText, "goal": colText, "reason": colText, "visibility_target": colText,
		"tags": colArrayEq, "wake": colJSON},
	"task_steps":          {"input_hash": colText, "input": colJSON, "output": colJSON, "err": colText},
	"audit_log":           {"detail": colText},
	"memories":            {"subject": colTextEq, "domain": colTextEq, "attribute": colText, "condition": colJSON, "identity_key": colTextEq, "tags": colArrayEq},
	"memory_revisions":    {"value": colText, "source_ref": colText, "evidence": colArray, "override_reason": colText},
	"memory_pending":      {"subject": colTextEq, "candidate": colJSON, "arbitration": colJSON},
	"events":              {"payload": colJSON, "source": colText},
	"event_subscriptions": {"expr": colText},
	"schedules":           {"title": colText, "goal": colText},
	"skills":              {"name": colText, "goal": colText, "author": colText, "steps": colArray, "raw": colText, "scan_report": colText, "source": colText},
	"style_samples":       {"text": colText, "digest": colTextEq, "ref": colText},
	"style_profiles":      {"description": colText},
	"channel_chats":       {"chat": colTextEq, "last_msg_id": colText},
	"channel_notices":     {"fingerprint": colText},
	"team_work":           {"title": colText},
	"mcp_installs":        {"url": colText, "registry_name": colText, "description": colText, "payload": colText, "scan_report": colText},
	// 保险库的值本来就由保险库自己的密钥加密，这里管的是名字和用途说明。
	"vault_items": {"label": colTextEq, "note": colText},
	"settings":    {"value": colText},
	// 用户发给任务的图片，存的是 Base64 文字。
	"task_images": {"data": colText},
}

// sealer 是各个存储共用的加解密入口。
type sealer struct{ box *seal.Box }

// str 加密一段文字。
func (z sealer) str(scope, v string) string { return z.box.Seal(scope, v) }

// eq 加密一段要按值查找的文字。
func (z sealer) eq(scope, v string) string { return z.box.SealEq(scope, v) }

// open 把一段密文就地换成明文。
func (z sealer) open(scope string, v *string) error {
	plain, err := z.box.Open(scope, *v)
	if err != nil {
		return err
	}
	*v = plain
	return nil
}

// opens 依次解开几段文字，pairs 是 位置名、指针 交替出现。
func (z sealer) opens(pairs ...any) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if err := z.open(pairs[i].(string), pairs[i+1].(*string)); err != nil {
			return err
		}
	}
	return nil
}

func (z sealer) array(scope string, vs []string, each func(string, string) string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = each(scope, v)
	}
	return out
}

// strs 加密一个文字数组。结果不会是 nil：这些列都是 NOT NULL。
func (z sealer) strs(scope string, vs []string) []string { return z.array(scope, vs, z.box.Seal) }

// eqs 加密一个要按元素查找的文字数组。
func (z sealer) eqs(scope string, vs []string) []string { return z.array(scope, vs, z.box.SealEq) }

// openAll 把一个数组里的密文就地换成明文。
func (z sealer) openAll(scope string, vs []string) error {
	for i := range vs {
		if err := z.open(scope, &vs[i]); err != nil {
			return err
		}
	}
	return nil
}

// json 加密一段 JSON，结果是一个 JSON 字符串，可以直接写进 JSONB 列。空的仍然是 NULL。
func (z sealer) json(scope string, raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	// 密文只含 base64 字符和一个点，加上引号就是合法的 JSON 字符串。
	return []byte(`"` + z.box.Seal(scope, string(raw)) + `"`)
}

// openJSON 解开 json 的结果。
func (z sealer) openJSON(scope string, raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var token string
	if err := json.Unmarshal(raw, &token); err != nil {
		return nil, fmt.Errorf("%w（%s）", seal.ErrNotSealed, scope)
	}
	plain, err := z.box.Open(scope, token)
	if err != nil {
		return nil, err
	}
	if plain == "" {
		return nil, nil
	}
	return []byte(plain), nil
}

// ---- 升级：给旧数据补加密 ----

// sealStateLock 是补加密时用的咨询锁编号，防止两个进程同时做。
const sealStateLock = 0x61746f6d73656131

// ensureSealed 确认数据库里的内容是用当前这把密钥加密的。
//
// 第一次带着密钥启动时，库里可能还有升级前留下的明文：在一个事务里把它们全部加密，
// 然后记下密钥的指纹。之后每次启动只核对指纹——密钥换了立刻报错，
// 不等到读出第一条解不开的数据。
func (s *Store) ensureSealed(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pg: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(sealStateLock)); err != nil {
		return fmt.Errorf("pg: 获取加密状态锁失败: %w", err)
	}
	var keyID string
	err = tx.QueryRow(ctx, `SELECT key_id FROM seal_state WHERE id = 1`).Scan(&keyID)
	switch {
	case err == nil:
		if keyID != s.z.box.KeyID() {
			return fmt.Errorf("pg: 数据库里的内容是用另一把密钥加密的（库里记的是 %s，现在这把是 %s）。"+
				"换了机器请用导出导入迁移；密钥文件丢了或者硬件标识变了，用 atomctl key restore 从备份恢复",
				keyID, s.z.box.KeyID())
		}
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("pg: 读取加密状态失败: %w", err)
	}

	tables := make([]string, 0, len(sealedColumns))
	for t := range sealedColumns {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, table := range tables {
		if err := s.sealTable(ctx, tx, table); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO seal_state (id, key_id) VALUES (1, $1)`, s.z.box.KeyID()); err != nil {
		return fmt.Errorf("pg: 记录加密状态失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg: 提交加密失败: %w", err)
	}
	return nil
}

// sealTable 把一张表里的明文全部加密。只在 ensureSealed 里、整库都还是明文时调用。
func (s *Store) sealTable(ctx context.Context, tx pgx.Tx, table string) error {
	cols := sortedColumns(table)
	// 独占这张表：补加密期间不能有别的写入混进来。
	if _, err := tx.Exec(ctx, `LOCK TABLE `+table+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("pg: 锁定表 %s 失败: %w", table, err)
	}
	// 行用 to_jsonb 整行读出来，和导出走同一条路；定位行用 ctid，不必知道每张表的主键。
	rows, err := tx.Query(ctx, `SELECT ctid::text, to_jsonb(t) FROM `+table+` t`)
	if err != nil {
		return fmt.Errorf("pg: 读取表 %s 失败: %w", table, err)
	}
	type pending struct {
		ctid string
		row  map[string]json.RawMessage
	}
	var all []pending
	for rows.Next() {
		var p pending
		var raw []byte
		if err := rows.Scan(&p.ctid, &raw); err != nil {
			rows.Close()
			return fmt.Errorf("pg: 读取表 %s 失败: %w", table, err)
		}
		if err := json.Unmarshal(raw, &p.row); err != nil {
			rows.Close()
			return fmt.Errorf("pg: 解析表 %s 的行失败: %w", table, err)
		}
		all = append(all, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pg: 读取表 %s 失败: %w", table, err)
	}

	for _, p := range all {
		if err := s.z.sealRow(table, p.row); err != nil {
			return err
		}
		patch := make(map[string]json.RawMessage, len(cols))
		var sets []string
		for _, c := range cols {
			// 空的列不动它：to_jsonb 分不出 SQL 的 NULL 和 JSON 的 null，写回去可能把后者变成前者。
			if v := p.row[c]; len(v) > 0 && string(v) != "null" {
				patch[c] = v
				// 从 JSON 里按列的原类型取回来：文字数组和 JSONB 都由数据库自己转换。
				sets = append(sets, fmt.Sprintf(`%s = (jsonb_populate_record(NULL::%s, $2::jsonb)).%s`, c, table, c))
			}
		}
		if len(sets) == 0 {
			continue
		}
		body, err := json.Marshal(patch)
		if err != nil {
			return fmt.Errorf("pg: 序列化表 %s 的行失败: %w", table, err)
		}
		update := `UPDATE ` + table + ` SET ` + strings.Join(sets, ", ") + ` WHERE ctid = $1::tid`
		if _, err := tx.Exec(ctx, update, p.ctid, body); err != nil {
			return fmt.Errorf("pg: 加密表 %s 失败: %w", table, err)
		}
	}
	return nil
}

func sortedColumns(table string) []string {
	cols := make([]string, 0, len(sealedColumns[table]))
	for c := range sealedColumns[table] {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	return cols
}

// sealRow 把一行（to_jsonb 的结果）里的内容列就地加密。导入和补加密共用。
func (z sealer) sealRow(table string, row map[string]json.RawMessage) error {
	return z.mapRow(table, row, true)
}

// openRow 把一行里的内容列就地解密。导出用。
func (z sealer) openRow(table string, row map[string]json.RawMessage) error {
	return z.mapRow(table, row, false)
}

func (z sealer) mapRow(table string, row map[string]json.RawMessage, sealing bool) error {
	for col, kind := range sealedColumns[table] {
		raw, ok := row[col]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		scope := table + "." + col
		var out any
		switch kind {
		case colText, colTextEq:
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				return fmt.Errorf("pg: %s 不是文字: %w", scope, err)
			}
			if sealing {
				if kind == colTextEq {
					v = z.eq(scope, v)
				} else {
					v = z.str(scope, v)
				}
			} else if err := z.open(scope, &v); err != nil {
				return err
			}
			out = v
		case colArray, colArrayEq:
			var vs []string
			if err := json.Unmarshal(raw, &vs); err != nil {
				return fmt.Errorf("pg: %s 不是文字数组: %w", scope, err)
			}
			if sealing {
				if kind == colArrayEq {
					vs = z.eqs(scope, vs)
				} else {
					vs = z.strs(scope, vs)
				}
			} else if err := z.openAll(scope, vs); err != nil {
				return err
			}
			out = vs
		case colJSON:
			if sealing {
				row[col] = z.json(scope, raw)
				continue
			}
			plain, err := z.openJSON(scope, raw)
			if err != nil {
				return err
			}
			if plain == nil {
				plain = []byte("null")
			}
			row[col] = plain
			continue
		}
		b, err := json.Marshal(out)
		if err != nil {
			return fmt.Errorf("pg: 序列化 %s 失败: %w", scope, err)
		}
		row[col] = b
	}
	return nil
}

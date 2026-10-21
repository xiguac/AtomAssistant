package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomhelper/internal/channel"
	"github.com/xiguac/atomhelper/internal/core"
	"github.com/xiguac/atomhelper/internal/event"
	"github.com/xiguac/atomhelper/internal/market"
	"github.com/xiguac/atomhelper/internal/memory"
	"github.com/xiguac/atomhelper/internal/schedule"
	"github.com/xiguac/atomhelper/internal/seal"
	"github.com/xiguac/atomhelper/internal/session"
	"github.com/xiguac/atomhelper/internal/skill"
	"github.com/xiguac/atomhelper/internal/store"
	"github.com/xiguac/atomhelper/internal/style"
	"github.com/xiguac/atomhelper/internal/team"
	"github.com/xiguac/atomhelper/internal/vault"
)

// 这些测试直接看数据库里实际存的是什么，所以放在包内。需要真实数据库，设置 ATOM_TEST_DSN 后运行。

func sealedStore(t *testing.T, box *seal.Box) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("ATOM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 ATOM_TEST_DSN，跳过落库加密测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	s, err := Open(ctx, dsn, box)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	t.Cleanup(s.Close)
	return s, ctx
}

func resetSealed(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	if err := s.ResetForTest(ctx); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
}

// plainColumns 是确认过不算内容、保持明文的文字列。调度、筛选和排序要用它们，
// 里面只有编号、状态、枚举值、工具名和程序自己起的名字。
var plainColumns = map[string][]string{
	"tasks":               {"id", "parent_id", "state", "domain", "visibility", "lease_owner"},
	"task_steps":          {"id", "task_id", "type", "status", "tool_name", "side_effect", "idempotency_key"},
	"audit_log":           {"id", "actor", "action", "task_id"},
	"memories":            {"id", "kind", "status", "temporality", "current_revision_id"},
	"memory_revisions":    {"id", "memory_id", "source_kind", "superseded_by"},
	"memory_pending":      {"id", "memory_id"},
	"events":              {"id", "topic", "inbox_task_id"},
	"event_subscriptions": {"id", "task_id", "topic_prefix"},
	"schedules":           {"id", "domain", "last_task_id"},
	"skills":              {"id", "version", "required_tools", "format"},
	"style_samples":       {"id", "scene"},
	"style_profiles":      {"scene"},
	"channel_chats":       {"channel"},
	"channel_notices":     {"task_id"},
	"team_work":           {"id", "task_id", "worker", "category", "rated_by"},
	"mcp_installs":        {"name"},
	"vault_items":         {"id", "allowed_tools"},
	"settings":            {"key"},
	"task_images":         {"id", "task_id", "mime"},
	"op_logs":             {"id", "category"},
	"session_messages":    {"id", "day", "role", "channel", "task_id"},
	"session_summaries":   {"day"},
	"seal_state":          {"key_id"},
	"schema_migrations":   {"version"},
}

// 每一个能装文字的列都必须表过态：要么加密，要么列在上面说明为什么不用。
// 以后加了新列却忘了加密，这个测试会指出来。
func TestSealed_每个文字列都有归属(t *testing.T) {
	s, ctx := sealedStore(t, seal.TestBox())
	resetSealed(t, s, ctx)

	rows, err := s.pool.Query(ctx, `
		SELECT table_name, column_name FROM information_schema.columns
		 WHERE table_schema = current_schema() AND data_type IN ('text', 'jsonb', 'ARRAY', 'character varying')
		 ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			t.Fatal(err)
		}
		if _, ok := sealedColumns[table][col]; ok {
			continue
		}
		plain := false
		for _, c := range plainColumns[table] {
			plain = plain || c == col
		}
		if !plain {
			t.Errorf("%s.%s 既没有加密，也没有登记为不需要加密的列", table, col)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// leaks 数出一张表里登记为加密的列中有多少个值不是密文。
func leaks(t *testing.T, s *Store, ctx context.Context) []string {
	t.Helper()
	var found []string
	for table, cols := range sealedColumns {
		for col, kind := range cols {
			var sql string
			switch kind {
			case colText, colTextEq:
				sql = fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s <> '' AND %s !~ '^(v1|d1)\.'`, table, col, col)
			case colArray, colArrayEq:
				sql = fmt.Sprintf(`SELECT count(*) FROM %s, unnest(%s) AS e WHERE e <> '' AND e !~ '^(v1|d1)\.'`, table, col)
			case colJSON:
				sql = fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s IS NOT NULL AND %s <> 'null'::jsonb AND %s::text !~ '^"v1\.'`, table, col, col, col)
			}
			var n int
			if err := s.pool.QueryRow(ctx, sql).Scan(&n); err != nil {
				t.Fatalf("检查 %s.%s 失败: %v", table, col, err)
			}
			if n > 0 {
				found = append(found, fmt.Sprintf("%s.%s（%d 个值）", table, col, n))
			}
		}
	}
	return found
}

func TestSealed_写进去的都是密文(t *testing.T) {
	s, ctx := sealedStore(t, seal.TestBox())
	resetSealed(t, s, ctx)
	now := time.Now().UTC().Truncate(time.Millisecond)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	task := core.NewTask("订去杭州的票", "周五下午出发", core.TrustUser)
	task.Tags = []string{"chat:qq/c2c:OWNER", "domain:出行"}
	must(s.CreateTask(ctx, task))
	must(s.Transition(ctx, task.ID, core.StateNew, core.StateReady, "用户创建"))
	step := core.NewStep(task.ID, 1, core.StepTool, json.RawMessage(`{"query":"杭州天气"}`))
	step.ToolName, step.SideEffect = "web.search", core.SideEffectReadOnly
	step.Complete(json.RawMessage(`{"answer":"晴"}`), core.TrustExternal)
	must(s.AppendStep(ctx, step))
	must(s.Audit(ctx, store.AuditEntry{Actor: "agent", Action: "tool.call", TaskID: task.ID, Detail: "查了杭州天气"}))

	src := memory.Source{Kind: memory.SourceUser, Ref: "app", Trust: core.TrustUser}
	m := memory.New(memory.KindPreference, "user", "出行", "优先高铁", src, now)
	m.Temporality, m.Attribute, m.Tags = memory.TemporalDurable, "交通方式", []string{"domain:出行"}
	m.Condition = memory.Condition{"场景": "国内"}
	m.Current.Evidence = []string{"用户说：能坐高铁就不坐飞机"}
	must(s.Memories().Put(ctx, m))
	must(s.Memories().PutPending(ctx, memory.Pending{ID: core.NewID(), CreatedAt: now,
		Candidate: memory.Candidate{Kind: memory.KindFact, Subject: "user", Value: "报销上限 5000",
			Temporality: memory.TemporalDurable, Source: src},
		Arb: memory.Arbitration{Kind: memory.ConflictNone, Resolution: memory.ResolveHoldAsCandidate}}))

	must(s.Events().SaveForTask(ctx, event.New(event.TopicUserMessage, json.RawMessage(`{"text":"改成周六"}`), core.TrustUser, "qq"), task.ID))
	must(s.Events().AddSubscription(ctx, &event.Subscription{ID: core.NewID(), TaskID: task.ID,
		Expr: `topic == "user.message"`, Once: true, Active: true, CreatedAt: now}))
	must(s.Schedules().Add(ctx, schedule.New("早报", "看今天的天气和日程", 24*time.Hour, now.Add(time.Hour))))
	must(s.Skills().PutSkill(ctx, &skill.Skill{ID: "acme.notes", Name: "会议纪要", Goal: "整理纪要", Author: "acme",
		Steps: []string{"先读录音稿"}, Raw: "# 正文", ScanReport: "通过", Format: skill.FormatMarkdown, Source: "github:acme", InstalledAt: now}))
	if _, err := s.Style().AddSample(ctx, &style.Sample{ID: "s1", Scene: style.SceneChat, Text: "行，就这么办", Ref: "task:x", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	must(s.Style().PutProfile(ctx, &style.Profile{Scene: style.SceneChat, Description: "句子很短", UpTo: now, UpdatedAt: now}))
	must(s.Channels().PutChat(ctx, &channel.ChatState{Channel: "qq", Chat: "c2c:OWNER", LastMsgID: "m1", LastMsgAt: now}))
	must(s.Channels().PutNotice(ctx, task.ID, "指纹"))
	must(s.Team().PutWork(ctx, &team.Work{ID: team.WorkID(task.ID, 1, 0), TaskID: task.ID, StepSeq: 1,
		Worker: "写手", Category: team.Categories[0].ID, Title: "写一封道歉信", CreatedAt: now}))
	must(s.MCPInstalls().PutServer(ctx, &market.Server{Name: "weather", URL: "https://mcp.example.com/mcp",
		RegistryName: "com.example/weather", Description: "查天气", Payload: "{}", ScanReport: "通过", InstalledAt: now}))
	must(s.Vault().PutItem(ctx, &vault.Item{ID: core.NewID(), Label: "银行卡", Note: "工资卡卡号",
		AllowedTools: []string{"pay.alipay_submit"}, Sealed: []byte{1}, Nonce: []byte{2}, CreatedAt: now}))
	must(s.PutSetting(ctx, "admin.totp", `{"secret":"JBSWY3DPEHPK3PXP"}`))
	if _, err := s.PutTaskImage(ctx, task.ID, "image/png", []byte("PNG 图片内容")); err != nil {
		t.Fatal(err)
	}
	must(s.Sessions().Append(ctx, session.Message{At: now, Role: session.RoleUser, Channel: "qq", Text: "帮我订周五去杭州的票"}))
	must(s.Sessions().PutSummary(ctx, session.Summary{Day: "2026-10-20", Text: "订了去杭州的票"}))

	// 每张登记了加密列的表都得真的有数据，否则下面的检查是空转。
	for table := range sealedColumns {
		var n int
		must(s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n))
		if n == 0 {
			t.Errorf("测试没有往 %s 里写数据，这张表的加密没有被检查到", table)
		}
	}
	if found := leaks(t, s, ctx); len(found) > 0 {
		t.Fatalf("这些列里有明文: %s", strings.Join(found, "、"))
	}

	// 密文读回来是原样的，按值查找照常能用。
	got, err := s.GetTask(ctx, task.ID)
	if err != nil || got.Title != "订去杭州的票" || got.Reason != "用户创建" || len(got.Tags) != 2 || got.Tags[0] != "chat:qq/c2c:OWNER" {
		t.Fatalf("任务没有原样读回: %v, %+v", err, got)
	}
	if byTag, err := s.TasksByTag(ctx, "chat:qq/c2c:OWNER", true, 10); err != nil || len(byTag) != 1 {
		t.Fatalf("按标签查找失效: %v, %d 个", err, len(byTag))
	}
	if changed, err := s.TasksChangedWithTagPrefix(ctx, "chat:", now.Add(-time.Hour), 10); err != nil || len(changed) != 1 {
		t.Fatalf("按标签前缀查找失效: %v, %d 个", err, len(changed))
	}
	if none, err := s.TasksChangedWithTagPrefix(ctx, "skill:", now.Add(-time.Hour), 10); err != nil || len(none) != 0 {
		t.Fatalf("不带这个前缀的任务不应被找出来: %v, %d 个", err, len(none))
	}
	mems, err := s.Memories().List(ctx, memory.Query{Subject: "user", Domain: "出行", Tags: []string{"domain:出行"}})
	if err != nil || len(mems) != 1 || mems[0].Current.Value != "优先高铁" || mems[0].Condition["场景"] != "国内" ||
		len(mems[0].Current.Evidence) != 1 {
		t.Fatalf("按主体、领域、标签检索记忆失效: %v, %+v", err, mems)
	}
	if byKey, err := s.Memories().FindByKey(ctx, m.Key()); err != nil || byKey.ID != m.ID {
		t.Fatalf("按身份键查找记忆失效: %v", err)
	}
	if v, err := s.Setting(ctx, "admin.totp"); err != nil || !strings.Contains(v, "JBSWY3DPEHPK3PXP") {
		t.Fatalf("设置没有原样读回: %v", err)
	}
	if items, err := s.Vault().ListItems(ctx); err != nil || len(items) != 1 || items[0].Label != "银行卡" || items[0].Note != "工资卡卡号" {
		t.Fatalf("保险库条目没有原样读回: %v", err)
	}
}

// 升级：库里是加密功能上线之前留下的明文，第一次带着密钥启动时应当全部补上加密，内容不变。
func TestSealed_升级时给旧数据补加密(t *testing.T) {
	s, ctx := sealedStore(t, seal.TestBox())
	resetSealed(t, s, ctx)
	// 回到"还没加密过"的状态，再照旧版本的样子写几行明文。
	if _, err := s.pool.Exec(ctx, `DELETE FROM seal_state`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	wake := fmt.Sprintf(`{"kind":"time","at":%q}`, at.Format(time.RFC3339))
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO tasks (id, title, goal, state, reason, domain, visibility, origin_trust, tags, wake, wake_at, created_at, updated_at)
		  VALUES ('t1', '续签护照', '下个月到期', 'waiting', '等到明天', 'personal', 'private', 4, ARRAY['skill:x','chat:qq/A'], $1::jsonb, $2, now(), now())`, []any{wake, at}},
		{`INSERT INTO task_steps (id, task_id, seq, type, status, tool_name, side_effect, input, output, idempotency_key, err, started_at)
		  VALUES ('s1', 't1', 1, 'tool', 'failed', 'mail.read', 'read_only', '{"id":"42"}'::jsonb, NULL, 'k1', '连不上邮箱', now())`, nil},
		{`INSERT INTO memories (id, kind, subject, domain, attribute, condition, identity_key, status, temporality, valid_from, confidence, tags, current_revision_id, created_at, updated_at)
		  VALUES ('m1', 'fact', 'user', '证件', '护照号', 'null'::jsonb, 'fact|user|证件|护照号', 'active', 'durable', now(), 0.9, ARRAY['domain:证件'], 'r1', now(), now())`, nil},
		{`INSERT INTO memory_revisions (id, memory_id, value, source_kind, source_ref, source_trust, evidence, confidence, created_at)
		  VALUES ('r1', 'm1', 'E12345678', 'user', 'app', 4, ARRAY['用户亲口说的'], 0.9, now())`, nil},
	} {
		if _, err := s.pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("准备旧数据失败: %v", err)
		}
	}
	if found := leaks(t, s, ctx); len(found) == 0 {
		t.Fatal("准备的旧数据应当是明文")
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("补加密失败: %v", err)
	}
	if found := leaks(t, s, ctx); len(found) > 0 {
		t.Fatalf("补加密之后仍有明文: %s", strings.Join(found, "、"))
	}
	task, err := s.GetTask(ctx, "t1")
	if err != nil || task.Title != "续签护照" || task.Reason != "等到明天" || len(task.Tags) != 2 ||
		task.Wake == nil || task.Wake.At == nil || !task.Wake.At.Equal(at) {
		t.Fatalf("任务内容变了: %v, %+v", err, task)
	}
	steps, err := s.ListSteps(ctx, "t1")
	if err != nil || len(steps) != 1 || string(steps[0].Input) != `{"id": "42"}` || steps[0].Err != "连不上邮箱" || len(steps[0].Output) != 0 {
		t.Fatalf("步骤内容变了: %v, %+v", err, steps)
	}
	mem, err := s.Memories().FindByKey(ctx, "fact|user|证件|护照号")
	if err != nil || mem.Current.Value != "E12345678" || mem.Subject != "user" || len(mem.Condition) != 0 {
		t.Fatalf("记忆内容变了: %v, %+v", err, mem)
	}
	// 到点的任务照常能被找出来：唤醒时间单独存了一列。
	due, err := s.DueWaiting(ctx, at.Add(time.Minute), 10)
	if err != nil || len(due) != 1 || due[0].ID != "t1" {
		t.Fatalf("到点的任务没有被找出来: %v, %d 个", err, len(due))
	}
	// 再启动一次什么都不该发生。
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("重复迁移失败: %v", err)
	}
	if again, err := s.GetTask(ctx, "t1"); err != nil || again.Title != "续签护照" {
		t.Fatalf("重复迁移后内容变了: %v", err)
	}
}

func TestSealed_换了密钥启动时立刻报错(t *testing.T) {
	s, ctx := sealedStore(t, seal.TestBox())
	resetSealed(t, s, ctx)

	other, err := seal.NewBox([]byte("another-key-another-key-another!"))
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := sealedStore(t, other)
	if err := wrong.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "另一把密钥") {
		t.Fatalf("用另一把密钥打开已经加密的库应当报错，实际 %v", err)
	}
}

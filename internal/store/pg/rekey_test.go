package pg

import (
	"strings"
	"testing"

	"github.com/xiguac/atomassistant/internal/seal"
)

func TestRekeyFrom_旧密文换成新密文(t *testing.T) {
	old := seal.TestBox()
	s, ctx := sealedStore(t, old)
	resetSealed(t, s, ctx)
	// 照旧版本的样子写几行明文，让旧密钥把它们加密。
	if _, err := s.pool.Exec(ctx, `DELETE FROM seal_state`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO tasks (id, title, goal, state, reason, domain, visibility, origin_trust, tags, created_at, updated_at)
		 VALUES ('t1', '续签护照', '下个月到期', 'suspended', '等到明天', 'personal', 'private', 4, ARRAY['skill:x'], now(), now())`,
		`INSERT INTO memories (id, kind, subject, domain, attribute, condition, identity_key, status, temporality, valid_from, confidence, tags, current_revision_id, created_at, updated_at)
		 VALUES ('m1', 'fact', 'user', '证件', '护照号', 'null'::jsonb, 'fact|user|证件|护照号', 'active', 'durable', now(), 0.9, ARRAY['domain:证件'], 'r1', now(), now())`,
		`INSERT INTO memory_revisions (id, memory_id, value, source_kind, source_ref, source_trust, evidence, confidence, created_at)
		 VALUES ('r1', 'm1', 'E12345678', 'user', 'app', 4, ARRAY['用户亲口说的'], 0.9, now())`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("准备旧数据失败: %v", err)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("用旧密钥加密失败: %v", err)
	}

	cur, err := seal.NewBox([]byte("another-key-another-key-another!"))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := sealedStore(t, cur)
	if changed, err := n.RekeyFrom(ctx, old); err != nil || !changed {
		t.Fatalf("重新加密失败: changed=%v err=%v", changed, err)
	}
	if err := n.Migrate(ctx); err != nil {
		t.Fatalf("重新加密之后应当能用新密钥打开: %v", err)
	}
	if task, err := n.GetTask(ctx, "t1"); err != nil || task.Title != "续签护照" || task.Reason != "等到明天" || len(task.Tags) != 1 {
		t.Fatalf("任务内容变了: %v, %+v", err, task)
	}
	// 同值同密文的列换了密钥之后照样能按值找到。
	if mem, err := n.Memories().FindByKey(ctx, "fact|user|证件|护照号"); err != nil || mem.Current.Value != "E12345678" {
		t.Fatalf("记忆内容变了: %v, %+v", err, mem)
	}
	if changed, err := n.RekeyFrom(ctx, old); err != nil || changed {
		t.Fatalf("已经换过的库重跑应当什么都不做: changed=%v err=%v", changed, err)
	}
	if err := s.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "另一把密钥") {
		t.Fatalf("换过之后旧密钥应当打不开，实际 %v", err)
	}
}

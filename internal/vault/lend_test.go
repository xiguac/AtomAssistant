package vault

import (
	"context"
	"errors"
	"testing"
)

func TestLend_连接器取密码走的是同一张授权名单(t *testing.T) {
	ctx := context.Background()
	v, _ := opened(t)
	if err := v.Seal(ctx, "icloud", "abcd-efgh-ijkl-mnop", "iCloud App 专用密码", []string{"connector.calendar"}); err != nil {
		t.Fatalf("存入失败: %v", err)
	}

	take, err := v.Lend("vault://icloud", "connector.calendar")
	if err != nil {
		t.Fatalf("借出失败: %v", err)
	}
	if got, err := take(ctx); err != nil || got != "abcd-efgh-ijkl-mnop" {
		t.Fatalf("授权过的连接器应取得到: %q %v", got, err)
	}

	// 名单里没有邮件连接器：同一条密码它取不到。
	other, err := v.Lend("vault://icloud", "connector.mail")
	if err != nil {
		t.Fatalf("借出失败: %v", err)
	}
	if _, err := other(ctx); !errors.Is(err, ErrDenied) {
		t.Fatalf("没授权的连接器应被拒绝: %v", err)
	}

	// 条目是每次现取的：用户在后台删掉它，下一次连接就取不到了。
	if err := v.Remove(ctx, "icloud"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := take(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("条目删除后应取不到: %v", err)
	}

	// 配置里只能写句柄。有人图省事直接写了密码，要在启动时就拦下。
	for _, bad := range []string{"abcd-efgh-ijkl-mnop", "", "vault://", "vault://有 空格"} {
		if _, err := v.Lend(bad, "connector.calendar"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q 不是句柄，应被拒绝: %v", bad, err)
		}
	}

	// 锁着的保险库借不出东西。
	locked, err := Open(NewMemStore(), "")
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	take, err = locked.Lend("vault://icloud", "connector.calendar")
	if err != nil {
		t.Fatalf("借出失败: %v", err)
	}
	if _, err := take(ctx); !errors.Is(err, ErrLocked) {
		t.Fatalf("锁定时应取不到: %v", err)
	}
}

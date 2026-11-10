package device

import (
	"os"
	"testing"
)

// 用真实的 ip2region 库验证接口真的能查。需要设置 IP2REGION_TEST_DB 指向 ip2region_v4.xdb，没设就跳过。
func TestIP2Region_真实库(t *testing.T) {
	path := os.Getenv("IP2REGION_TEST_DB")
	if path == "" {
		t.Skip("未设置 IP2REGION_TEST_DB，跳过真实库测试")
	}
	db, err := OpenIP2Region(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"114.114.114.114", "202.96.209.133", "8.8.8.8", "119.29.29.29"} {
		r, ok := db.Lookup(ip)
		t.Logf("%s -> %v %+v", ip, ok, r)
		if !ok {
			t.Errorf("%s 应当能查到", ip)
		}
	}
	for _, ip := range []string{"192.168.1.1", "127.0.0.1", "10.0.0.5", "::1", "not-an-ip", "2001:4860:4860::8888"} {
		if _, ok := db.Lookup(ip); ok {
			t.Errorf("%s 不该查到（内网、回环、IPv6、非法）", ip)
		}
	}
}

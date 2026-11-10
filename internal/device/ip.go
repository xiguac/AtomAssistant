package device

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
)

// 公网 IP 归属地：用 ip2region 的离线库把请求来源的 IP 查成国家、省、市、运营商。
//
// 只到市级，而且不一定准（代理、VPN、运营商的出口都会让它偏），所以它只在精确位置缺失或
// 明显离谱的时候才拿来用，见 Where。库里查到的是文字，不含坐标。IP 本身不存，只存查出来的归属地。

// Region 是一个 IP 的归属地。
type Region struct {
	Country  string `json:"country,omitempty"`
	Province string `json:"province,omitempty"`
	City     string `json:"city,omitempty"`
	ISP      string `json:"isp,omitempty"`
}

// Key 用来判断两次查到的归属地是不是同一个。
func (r Region) Key() string { return r.Country + "|" + r.Province + "|" + r.City + "|" + r.ISP }

// RegionLookup 把 IP 查成归属地，由 *IP2Region 提供。查不到返回 false。
type RegionLookup interface {
	Lookup(ip string) (Region, bool)
}

// ParseRegion 解析 ip2region 的结果：国家|省|市|运营商|国家代码。库里用 "0" 表示没有。
func ParseRegion(s string) Region {
	parts := strings.Split(s, "|")
	get := func(i int) string {
		if i >= len(parts) || parts[i] == "0" {
			return ""
		}
		return parts[i]
	}
	return Region{Country: get(0), Province: get(1), City: get(2), ISP: get(3)}
}

// IP2Region 是 ip2region 的 IPv4 库，整个读进内存，并发安全。
type IP2Region struct {
	searcher *xdb.Searcher
}

// OpenIP2Region 打开 ip2region_v4.xdb。
func OpenIP2Region(path string) (*IP2Region, error) {
	buf, err := xdb.LoadContentFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("device: 读取 IP 归属地库 %s 失败: %w", path, err)
	}
	s, err := xdb.NewWithBuffer(xdb.IPv4, buf)
	if err != nil {
		return nil, fmt.Errorf("device: 打开 IP 归属地库失败（文件是 IPv4 的 xdb 吗）: %w", err)
	}
	return &IP2Region{searcher: s}, nil
}

// Lookup 查一个 IPv4 地址。内网、回环地址和 IPv6（库里没有）一律查不到。
func (d *IP2Region) Lookup(ip string) (Region, bool) {
	p := net.ParseIP(ip)
	if p == nil || p.To4() == nil || !isPublic(p) {
		return Region{}, false
	}
	res, err := d.searcher.Search(p.To4().String())
	if err != nil || res == "" {
		return Region{}, false
	}
	r := ParseRegion(res)
	return r, r.Country != "" || r.Province != "" || r.City != ""
}

func isPublic(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast())
}

// clientIP 取请求真正的来源 IP。后台前面有 nginx 反代，请求是从本机转进来的：
// 只有来自本机或内网的连接才信 X-Forwarded-For，否则任何人都能伪造来源。
// nginx 里写的是 X-Forwarded-For $remote_addr（覆盖而不是追加），所以取第一个就是真实来源。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer != nil && (peer.IsLoopback() || peer.IsPrivate()) {
		if xff := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); net.ParseIP(xff) != nil {
			return xff
		}
	}
	return host
}

// ipNote 记录一台设备上次存下的归属地，避免每个请求都写库。
type ipNote struct {
	key string
	at  time.Time
}

// ipNoteEvery 是归属地没变时，隔多久再存一条（用来知道“最近一次在这里”是什么时候）。
const ipNoteEvery = 30 * time.Minute

// NoteIP 记下这台设备此刻的来源 IP 对应的归属地。没有归属地库、查不到、归属地没变且刚存过，都不写。
// 写失败只记日志：它是旁路信息，不该让设备的正常请求失败。
func (s *Service) NoteIP(ctx context.Context, d *Device, ip string) {
	if s.IPRegion == nil || ip == "" {
		return
	}
	reg, ok := s.IPRegion.Lookup(ip)
	if !ok {
		return
	}
	now := s.now()
	s.ipMu.Lock()
	if s.ipLast == nil {
		s.ipLast = map[string]ipNote{}
	}
	last := s.ipLast[d.ID]
	if last.key == reg.Key() && now.Sub(last.at) < ipNoteEvery {
		s.ipMu.Unlock()
		return
	}
	s.ipLast[d.ID] = ipNote{key: reg.Key(), at: now}
	s.ipMu.Unlock()

	payload, err := json.Marshal(reg)
	if err != nil {
		return
	}
	// key 里带上归属地：同一个 10 分钟里归属地变了，也得各存各的，不能被当成重复丢掉。
	h := fnv.New32a()
	_, _ = h.Write([]byte(reg.Key()))
	item := Item{Key: fmt.Sprintf("ip-%d-%08x", now.Unix()/600, h.Sum32()), CapturedAt: now, Payload: payload}
	if _, err := s.Store.AddData(ctx, d.ID, "ip.region", "", []Item{item}, now); err != nil {
		slog.Warn("存公网 IP 归属地失败", "设备", d.Name, "err", err)
	}
}

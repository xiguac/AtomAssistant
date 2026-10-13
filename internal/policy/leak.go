package policy

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/xiguac/atomhelper/internal/core"
)

// 出站过滤：凡是网址或内容由模型拼出来的 HTTP 请求，发出去之前都要过这一层。
//
// 它防的是这样的事：一封钓鱼邮件让助手"访问一下这个链接"，链接的参数里拼着用户的手机号。
// 请求是只读的，不改动任何东西，但数据就这样出去了。审批是一道防线，这里是另一道——
// 它不看这个请求该不该发，只看里面带没带个人敏感信息，带了就拦，不问任何人。
//
// 它拦得住的是认得出形态的东西（证件号、银行卡号、手机号、邮箱、健康信息），以及经过
// 网址编码、Base64 编码或者夹了分隔符之后仍然认得出的。认不出形态的内容（一个名字、一段住址）
// 和别的编码方式它拦不住，所以它是审批之外的第二道防线，不是替代。

var (
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(\.[A-Za-z0-9\-]+)+`)
	// reEncoded 是可能藏着 Base64 内容的片段。
	reEncoded = regexp.MustCompile(`[A-Za-z0-9+/_\-]{16,}={0,2}`)
	// reSeparators 是常被用来把一串数字隔开的字符。
	reSeparators = regexp.MustCompile(`[\s\-_.·]+`)
)

// ErrLeakBlocked 表示一个请求因为带着个人敏感信息而没有发出去。
var ErrLeakBlocked = errors.New("policy: 请求里带着个人敏感信息")

// Finding 是在一段要发出去的内容里认出来的一样敏感信息。
type Finding struct {
	Level core.PrivacyLevel
	// What 说的是哪一类信息，不含它的值：这句话会进日志和报错。
	What string
}

// ScanText 在一段自由文本里找个人敏感信息，返回最敏感的那一样。没找到时 Level 为 PrivacyPublic。
//
// 和 ClassifyField 不同，它不靠字段名，只看内容，而且会把内容解码之后再看一遍。
// 长串的字母数字不算：网址里带着订单号、资源编号是常态，这里分不出它是编号还是密钥。
func (c *Classifier) ScanText(text string) Finding {
	best := Finding{Level: core.PrivacyPublic}
	for _, v := range variants(text) {
		if f := scanPlain(v); f.Level > best.Level {
			best = f
		}
	}
	return best
}

func scanPlain(s string) Finding {
	switch {
	case reIDCard.MatchString(s):
		return Finding{core.PrivacySecret, "身份证号"}
	case looksLikeBankCard(s):
		return Finding{core.PrivacySecret, "银行卡号"}
	}
	for _, kw := range secretValueHints {
		if strings.Contains(s, kw) {
			return Finding{core.PrivacySecret, "健康信息"}
		}
	}
	switch {
	case rePhoneCN.MatchString(s):
		return Finding{core.PrivacySensitive, "手机号"}
	case reEmail.MatchString(s):
		return Finding{core.PrivacySensitive, "邮箱地址"}
	}
	return Finding{Level: core.PrivacyPublic}
}

// variants 给出一段内容的几种读法：原样、网址解码后、去掉分隔符后、其中的 Base64 片段解码后。
func variants(text string) []string {
	out := []string{text}
	add := func(s string) {
		if s != "" && s != text {
			out = append(out, s)
		}
	}
	decoded := text
	// 网址编码可以套好几层。
	for i := 0; i < 3; i++ {
		next, err := url.QueryUnescape(decoded)
		if err != nil || next == decoded {
			break
		}
		decoded = next
		add(decoded)
	}
	add(reSeparators.ReplaceAllString(decoded, ""))
	for _, chunk := range reEncoded.FindAllString(decoded, 32) {
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
			if raw, err := enc.DecodeString(chunk); err == nil && utf8.Valid(raw) {
				add(string(raw))
				break
			}
		}
	}
	return out
}

// maxScanBody 是请求体里参与检查的长度上限。
const maxScanBody = 1 << 20

// EgressFilter 是套在 HTTP 客户端外面的出站过滤。
type EgressFilter struct {
	// Base 是实际发请求的传输，nil 用 http.DefaultTransport。
	Base http.RoundTripper
	// Max 是允许带出去的最高隐私等级，高于它的请求被拦下。
	Max core.PrivacyLevel
	// What 说明这是谁在发请求，用在报错里。
	What string

	classifier Classifier
}

// NewEgressFilter 构造出站过滤：请求的网址和请求体里出现高于 max 的个人敏感信息时不发出去。
func NewEgressFilter(base http.RoundTripper, max core.PrivacyLevel, what string) *EgressFilter {
	return &EgressFilter{Base: base, Max: max, What: what}
}

// RoundTrip 检查后放行或拦下。被拦下的请求不会离开这台机器。
func (f *EgressFilter) RoundTrip(req *http.Request) (*http.Response, error) {
	// 网址里能带东西的地方都看：路径、查询参数、片段。
	found := f.classifier.ScanText(req.URL.Path + " " + req.URL.RawQuery + " " + req.URL.Fragment)
	if req.Body != nil && req.Body != http.NoBody {
		body, err := io.ReadAll(io.LimitReader(req.Body, maxScanBody+1))
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("policy: 读取请求体失败: %w", err)
		}
		if len(body) > maxScanBody {
			// 看不完的内容不放行：否则把要带出去的东西垫在后面就绕过去了。
			return nil, fmt.Errorf("%w: %s 的请求体超过 %d KB，无法检查", ErrLeakBlocked, f.What, maxScanBody>>10)
		}
		if b := f.classifier.ScanText(string(body)); b.Level > found.Level {
			found = b
		}
		// 请求体读过了，换一个新的给下游。
		req = req.Clone(req.Context())
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	if found.Level > f.Max {
		return nil, fmt.Errorf("%w: %s 的请求里有%s，没有发出去", ErrLeakBlocked, f.What, found.What)
	}
	base := f.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

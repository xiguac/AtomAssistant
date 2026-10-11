package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"html/template"
	"net/url"
	"strings"
	"time"

	"rsc.io/qr"
)

// 动态验证码（TOTP，RFC 6238）：手机上的验证器和这里各自用同一份种子、按当前时间算出 6 位数字。
//
// 用的是所有验证器都认的那一组参数——SHA-1、6 位、30 秒一换——
// Microsoft Authenticator、Google Authenticator 等扫码即可添加。

const (
	totpPeriod = 30 * time.Second
	totpDigits = 6
	// totpSkew 是允许的时间偏差，以周期计：手机和服务器的钟差上半分钟仍然能过。
	totpSkew = 1
)

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newTOTPSecret 生成一份新的种子，返回它的文字形式（验证器里手动输入的就是它）。
func newTOTPSecret() string {
	raw := make([]byte, 20)
	_, _ = rand.Read(raw)
	return totpEncoding.EncodeToString(raw)
}

// totpAt 算出某个时间周期上的验证码。
func totpAt(secret string, step int64) (string, error) {
	key, err := totpEncoding.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(key) == 0 {
		return "", fmt.Errorf("admin: 动态验证码的种子无法识别")
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, n%1_000_000), nil
}

// verifyTOTP 核对一个验证码。通过时返回它对应的时间周期。
//
// lastStep 是上一次通过的验证码所在的周期：不晚于它的一律不认。
// 一个验证码只能用一次——被人从背后看去、或者在网络上截到的验证码，在它过期之前也用不了第二次。
func verifyTOTP(secret, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	current := now.Unix() / int64(totpPeriod/time.Second)
	matched, ok := int64(0), false
	// 每个候选周期都比一遍，不在第一个对上的地方提前返回。
	for step := current - totpSkew; step <= current+totpSkew; step++ {
		want, err := totpAt(secret, step)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 && step > lastStep {
			matched, ok = step, true
		}
	}
	return matched, ok
}

// totpURI 是给验证器扫码用的地址。
func totpURI(secret, account string) string {
	const issuer = "原子助手"
	q := url.Values{
		"secret":    {secret},
		"issuer":    {issuer},
		"algorithm": {"SHA1"},
		"digits":    {fmt.Sprint(totpDigits)},
		"period":    {fmt.Sprint(int(totpPeriod / time.Second))},
	}
	// 空格要写成 %20：有的验证器不认加号。
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + strings.ReplaceAll(q.Encode(), "+", "%20")
}

// qrSVG 把一段文字画成二维码，直接嵌在页面里。
//
// 在服务器上画，不调用任何在线的二维码服务：这段文字里是动态验证码的种子。
func qrSVG(text string) (template.HTML, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", fmt.Errorf("admin: 生成二维码失败: %w", err)
	}
	const quiet = 4
	size := code.Size + 2*quiet
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="200" height="200" shape-rendering="crispEdges" role="img" aria-label="动态验证码二维码">`, size, size)
	fmt.Fprintf(&sb, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, size, size)
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				fmt.Fprintf(&sb, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	sb.WriteString(`"/></svg>`)
	// 内容全部是这里自己拼出来的数字和固定的标签，没有外来的文字。
	return template.HTML(sb.String()), nil
}

// Package policy 实现数据出口管控与审批策略。
//
// 这是整个系统的安全承重墙：沙箱能防止工具乱动系统，但防不住我们主动把数据交给它。
// 真正的防线在出口。见 docs/安全模型.md §4。
package policy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/xiguac/atomhelper/internal/core"
)

// 字段名匹配优先于值匹配：字段叫 password 时，不管值长什么样都是 L3。
var secretFieldNames = []string{
	"password", "passwd", "pwd", "secret", "token", "api_key", "apikey",
	"access_token", "refresh_token", "credential", "private_key", "session",
	"cvv", "verification_code", "otp",
	"密码", "口令", "密钥", "验证码", "令牌",
}

var secretValueHints = []string{
	"过敏", "病史", "确诊", "用药", "残疾", "抑郁", "肿瘤", "手术",
}

var sensitiveFieldNames = []string{
	"address", "home", "phone", "mobile", "email", "contact", "amount",
	"balance", "salary", "地址", "住址", "电话", "手机", "邮箱", "金额", "余额",
}

var (
	// 中国大陆手机号。
	rePhoneCN = regexp.MustCompile(`\b1[3-9]\d{9}\b`)
	// 18 位身份证（含 X 结尾）。只做形态匹配，不校验校验位——
	// 宁可误判为敏感，也不能漏判。
	reIDCard = regexp.MustCompile(`\b\d{17}[\dXx]\b`)
	// 13~19 位连续数字，进一步用 Luhn 校验确认是不是银行卡。
	reDigits13To19 = regexp.MustCompile(`\b\d{13,19}\b`)
	// 常见的 API Key 形态：长串字母数字混合。
	reLongToken = regexp.MustCompile(`\b[A-Za-z0-9_\-]{32,}\b`)
)

// Classifier 对字段做隐私分级。
//
// 它刻意偏向保守：分不清的时候往高了判。漏判一次的代价（隐私泄露）
// 远大于误判一次的代价（多一次用户确认）。
type Classifier struct{}

// NewClassifier 构造分级器。
func NewClassifier() *Classifier { return &Classifier{} }

// ClassifyField 判断单个字段的隐私等级。
func (c *Classifier) ClassifyField(name string, value any) core.PrivacyLevel {
	lower := strings.ToLower(name)
	for _, kw := range secretFieldNames {
		if strings.Contains(lower, kw) {
			return core.PrivacySecret
		}
	}

	s := stringify(value)
	if s == "" {
		return c.levelFromFieldName(lower)
	}

	switch {
	case reIDCard.MatchString(s):
		return core.PrivacySecret
	case looksLikeBankCard(s):
		return core.PrivacySecret
	case reLongToken.MatchString(s) && !hasSpace(s):
		return core.PrivacySecret
	}
	for _, kw := range secretValueHints {
		if strings.Contains(s, kw) {
			// 健康信息属于 L3，见 docs/安全模型.md §4.1。
			return core.PrivacySecret
		}
	}
	if rePhoneCN.MatchString(s) {
		return core.PrivacySensitive
	}
	return c.levelFromFieldName(lower)
}

func (c *Classifier) levelFromFieldName(lower string) core.PrivacyLevel {
	for _, kw := range sensitiveFieldNames {
		if strings.Contains(lower, kw) {
			return core.PrivacySensitive
		}
	}
	return core.PrivacyNormal
}

// FieldLevel 是一个字段的分级结果，路径用点号连接（如 `user.contact.phone`）。
type FieldLevel struct {
	Path  string
	Level core.PrivacyLevel
}

// ClassifyJSON 递归分级一个 JSON 对象，返回每个叶子字段的等级与整体最高等级。
func (c *Classifier) ClassifyJSON(raw json.RawMessage) ([]FieldLevel, core.PrivacyLevel, error) {
	if len(raw) == 0 {
		return nil, core.PrivacyPublic, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, core.PrivacySecret, fmt.Errorf("policy: 参数不是合法 JSON，无法分级: %w", err)
	}
	var out []FieldLevel
	max := core.PrivacyPublic
	c.walk("", v, &out, &max)
	return out, max, nil
}

func (c *Classifier) walk(path string, v any, out *[]FieldLevel, max *core.PrivacyLevel) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			c.walk(join(path, k), child, out, max)
		}
	case []any:
		for i, child := range t {
			c.walk(fmt.Sprintf("%s[%d]", path, i), child, out, max)
		}
	default:
		name := path
		if i := strings.LastIndex(path, "."); i >= 0 {
			name = path[i+1:]
		}
		lv := c.ClassifyField(name, v)
		*out = append(*out, FieldLevel{Path: path, Level: lv})
		if lv > *max {
			*max = lv
		}
	}
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// JSON 数字统一是 float64。银行卡号如果被当成数字传进来，
		// 格式化后仍然要能被识别。
		return strings.TrimSuffix(fmt.Sprintf("%.0f", t), ".0")
	case bool, nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func hasSpace(s string) bool { return strings.ContainsAny(s, " \t\n") }

// looksLikeBankCard 用 Luhn 校验判断是否为银行卡号。
func looksLikeBankCard(s string) bool {
	for _, m := range reDigits13To19.FindAllString(s, -1) {
		if luhnValid(m) {
			return true
		}
	}
	return false
}

func luhnValid(digits string) bool {
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

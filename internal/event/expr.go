package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 订阅表达式的语法：
//
//	expr    := and { "OR" and }
//	and     := unary { "AND" unary }
//	unary   := "NOT" unary | "(" expr ")" | compare
//	compare := operand op operand
//	op      := "==" | "!=" | ">" | ">=" | "<" | "<=" | "CONTAINS"
//	operand := 字符串 | 数字 | true | false | null | 路径
//	路径     := topic | source | payload { "." 标识符 }
//
// 刻意不支持函数调用、算术和正则：表达式可能由模型生成，
// 能力越少，求值成本和出错面越可控。

const (
	// maxExprLen 限制表达式长度。表达式是不可信输入，不能让它任意膨胀。
	maxExprLen = 2048
	// maxExprDepth 限制括号与 NOT 的嵌套深度，防止递归下降把栈打爆。
	maxExprDepth = 32
)

// ErrInvalidExpr 表示订阅表达式语法不合法。
var ErrInvalidExpr = errors.New("event: 订阅表达式不合法")

// Node 是编译后的表达式节点。
type Node interface {
	// Eval 对一个事件求值。实现不得 panic，也不得产生副作用。
	Eval(env *Env) (bool, error)
}

// Env 是表达式求值时能看到的事件内容。
type Env struct {
	topic   string
	source  string
	payload any
}

func newEnv(e *Event) (*Env, error) {
	env := &Env{topic: e.Topic, source: e.Source}
	if len(e.Payload) == 0 {
		return env, nil
	}
	if err := json.Unmarshal(e.Payload, &env.payload); err != nil {
		return nil, fmt.Errorf("%w: Payload 不是合法 JSON: %v", ErrInvalidEvent, err)
	}
	return env, nil
}

// Parse 把表达式编译成可求值的节点。
func Parse(expr string) (Node, error) {
	if len(expr) > maxExprLen {
		return nil, fmt.Errorf("%w: 长度 %d 超过上限 %d", ErrInvalidExpr, len(expr), maxExprLen)
	}
	toks, err := tokenize(expr)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	n, err := p.parseOr(0)
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokEOF {
		return nil, fmt.Errorf("%w: 第 %d 个字符处有多余内容 %q",
			ErrInvalidExpr, p.peek().pos+1, p.peek().text)
	}
	return n, nil
}

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokIdent
	tokString
	tokNumber
	tokOp
	tokLParen
	tokRParen
)

type token struct {
	kind tokenKind
	text string
	num  float64
	pos  int
}

func tokenize(s string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			toks = append(toks, token{kind: tokLParen, text: "(", pos: i})
			i++
		case c == ')':
			toks = append(toks, token{kind: tokRParen, text: ")", pos: i})
			i++
		case c == '"':
			text, next, err := scanString(s, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tokString, text: text, pos: i})
			i = next
		case c == '=' || c == '!' || c == '>' || c == '<':
			op := string(c)
			if i+1 < len(s) && s[i+1] == '=' {
				op += "="
			}
			if op == "=" || op == "!" {
				return nil, fmt.Errorf("%w: 第 %d 个字符处的运算符 %q 不完整", ErrInvalidExpr, i+1, op)
			}
			toks = append(toks, token{kind: tokOp, text: op, pos: i})
			i += len(op)
		case c == '-' || (c >= '0' && c <= '9'):
			j := i + 1
			for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == '.') {
				j++
			}
			num, err := strconv.ParseFloat(s[i:j], 64)
			if err != nil {
				return nil, fmt.Errorf("%w: %q 不是合法数字", ErrInvalidExpr, s[i:j])
			}
			toks = append(toks, token{kind: tokNumber, text: s[i:j], num: num, pos: i})
			i = j
		case isIdentStart(c):
			j := i + 1
			for j < len(s) && (isIdentStart(s[j]) || (s[j] >= '0' && s[j] <= '9') || s[j] == '.') {
				j++
			}
			toks = append(toks, token{kind: tokIdent, text: s[i:j], pos: i})
			i = j
		default:
			r, _ := utf8.DecodeRuneInString(s[i:])
			return nil, fmt.Errorf("%w: 第 %d 个字符处出现意外的 %q（字符串必须用双引号包裹）",
				ErrInvalidExpr, i+1, r)
		}
	}
	toks = append(toks, token{kind: tokEOF, pos: len(s)})
	return toks, nil
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

// scanString 读取一个双引号字符串，支持 \" 与 \\ 转义。
func scanString(s string, start int) (string, int, error) {
	var sb strings.Builder
	i := start + 1
	for i < len(s) {
		switch s[i] {
		case '"':
			return sb.String(), i + 1, nil
		case '\\':
			if i+1 >= len(s) || (s[i+1] != '"' && s[i+1] != '\\') {
				return "", 0, fmt.Errorf("%w: 第 %d 个字符处的转义不合法", ErrInvalidExpr, i+1)
			}
			sb.WriteByte(s[i+1])
			i += 2
		default:
			sb.WriteByte(s[i])
			i++
		}
	}
	return "", 0, fmt.Errorf("%w: 第 %d 个字符处的字符串没有闭合", ErrInvalidExpr, start+1)
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != tokEOF {
		p.pos++
	}
	return t
}

func (p *parser) isKeyword(word string) bool {
	t := p.peek()
	return t.kind == tokIdent && t.text == word
}

func (p *parser) parseOr(depth int) (Node, error) {
	left, err := p.parseAnd(depth)
	if err != nil {
		return nil, err
	}
	for p.isKeyword("OR") {
		p.next()
		right, err := p.parseAnd(depth)
		if err != nil {
			return nil, err
		}
		left = orNode{left, right}
	}
	return left, nil
}

func (p *parser) parseAnd(depth int) (Node, error) {
	left, err := p.parseUnary(depth)
	if err != nil {
		return nil, err
	}
	for p.isKeyword("AND") {
		p.next()
		right, err := p.parseUnary(depth)
		if err != nil {
			return nil, err
		}
		left = andNode{left, right}
	}
	return left, nil
}

func (p *parser) parseUnary(depth int) (Node, error) {
	if depth > maxExprDepth {
		return nil, fmt.Errorf("%w: 嵌套深度超过上限 %d", ErrInvalidExpr, maxExprDepth)
	}
	if p.isKeyword("NOT") {
		p.next()
		inner, err := p.parseUnary(depth + 1)
		if err != nil {
			return nil, err
		}
		return notNode{inner}, nil
	}
	if p.peek().kind == tokLParen {
		p.next()
		inner, err := p.parseOr(depth + 1)
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokRParen {
			return nil, fmt.Errorf("%w: 第 %d 个字符处缺少右括号", ErrInvalidExpr, p.peek().pos+1)
		}
		p.next()
		return inner, nil
	}
	return p.parseCompare()
}

func (p *parser) parseCompare() (Node, error) {
	left, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	t := p.next()
	op := t.text
	if t.kind != tokOp && !(t.kind == tokIdent && t.text == "CONTAINS") {
		return nil, fmt.Errorf("%w: 第 %d 个字符处应为比较运算符，实际是 %q",
			ErrInvalidExpr, t.pos+1, t.text)
	}
	right, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	return compareNode{op: op, left: left, right: right}, nil
}

func (p *parser) parseOperand() (operand, error) {
	t := p.next()
	switch t.kind {
	case tokString:
		return operand{value: t.text}, nil
	case tokNumber:
		return operand{value: t.num}, nil
	case tokIdent:
		switch t.text {
		case "true":
			return operand{value: true}, nil
		case "false":
			return operand{value: false}, nil
		case "null":
			return operand{}, nil
		case "AND", "OR", "NOT", "CONTAINS":
			return operand{}, fmt.Errorf("%w: 第 %d 个字符处应为操作数，实际是关键字 %s",
				ErrInvalidExpr, t.pos+1, t.text)
		}
		path := strings.Split(t.text, ".")
		for _, seg := range path {
			if seg == "" {
				return operand{}, fmt.Errorf("%w: 路径 %q 含空层级", ErrInvalidExpr, t.text)
			}
		}
		switch path[0] {
		case "topic", "source":
			if len(path) > 1 {
				return operand{}, fmt.Errorf("%w: %s 没有子字段", ErrInvalidExpr, path[0])
			}
		case "payload":
		default:
			return operand{}, fmt.Errorf("%w: 未知的字段 %q，只能用 topic、source、payload",
				ErrInvalidExpr, path[0])
		}
		return operand{path: path}, nil
	default:
		return operand{}, fmt.Errorf("%w: 第 %d 个字符处应为操作数", ErrInvalidExpr, t.pos+1)
	}
}

// operand 是比较的一侧：字面量或事件里的一个路径。
type operand struct {
	path  []string
	value any
}

// resolve 取出操作数的值。路径不存在时返回 nil 而不是报错——
// 事件没有某个字段是常态，不应让整条订阅求值失败。
func (o operand) resolve(env *Env) any {
	if o.path == nil {
		return o.value
	}
	switch o.path[0] {
	case "topic":
		return env.topic
	case "source":
		return env.source
	}
	cur := env.payload
	for _, seg := range o.path[1:] {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[seg]
	}
	return cur
}

type orNode struct{ left, right Node }

func (n orNode) Eval(env *Env) (bool, error) {
	ok, err := n.left.Eval(env)
	if err != nil || ok {
		return ok, err
	}
	return n.right.Eval(env)
}

type andNode struct{ left, right Node }

func (n andNode) Eval(env *Env) (bool, error) {
	ok, err := n.left.Eval(env)
	if err != nil || !ok {
		return false, err
	}
	return n.right.Eval(env)
}

type notNode struct{ inner Node }

func (n notNode) Eval(env *Env) (bool, error) {
	ok, err := n.inner.Eval(env)
	if err != nil {
		return false, err
	}
	return !ok, nil
}

type compareNode struct {
	op          string
	left, right operand
}

func (n compareNode) Eval(env *Env) (bool, error) {
	l, r := n.left.resolve(env), n.right.resolve(env)
	switch n.op {
	case "==":
		return scalarEqual(l, r), nil
	case "!=":
		return !scalarEqual(l, r), nil
	case "CONTAINS":
		return contains(l, r), nil
	}

	// 大小比较只在同类型之间有意义；类型不一致或字段缺失一律不命中。
	switch lv := l.(type) {
	case float64:
		rv, ok := r.(float64)
		if !ok {
			return false, nil
		}
		return order(n.op, compareFloat(lv, rv))
	case string:
		rv, ok := r.(string)
		if !ok {
			return false, nil
		}
		// 字符串按字典序比较，RFC3339 时间戳在同一时区下可以直接比。
		return order(n.op, strings.Compare(lv, rv))
	}
	return false, nil
}

func order(op string, cmp int) (bool, error) {
	switch op {
	case ">":
		return cmp > 0, nil
	case ">=":
		return cmp >= 0, nil
	case "<":
		return cmp < 0, nil
	case "<=":
		return cmp <= 0, nil
	}
	return false, fmt.Errorf("%w: 未知的运算符 %q", ErrInvalidExpr, op)
}

func compareFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// scalarEqual 比较两个标量。对象与数组不参与相等比较，一律视为不等。
func scalarEqual(a, b any) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	}
	return false
}

// contains 的语义取决于左侧：数组看是否有元素等于右侧，字符串看是否包含子串。
func contains(l, r any) bool {
	switch lv := l.(type) {
	case []any:
		for _, item := range lv {
			if scalarEqual(item, r) {
				return true
			}
		}
	case string:
		rv, ok := r.(string)
		return ok && strings.Contains(lv, rv)
	}
	return false
}

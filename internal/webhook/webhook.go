// Package webhook 是外部系统往事件总线里投递事件的 HTTP 入口。
//
//	POST /events/<主题>      请求体是 JSON，作为事件的 payload
//	Authorization: Bearer <令牌>
//
// 两条不让步的规则：
//
//  1. **从这里进来的一切都是外部内容。** 事件的信任等级固定为 external，
//     不接受调用方自报。持有令牌只证明"允许投递"，不证明"说的是真的"——
//     令牌泄露、对方系统被入侵、对方只是在转发第三方写的东西，都很平常。
//     这样的事件能把任务叫醒，但它的内容进了上下文是不可信材料，
//     由它驱动的写操作一律要用户确认。
//  2. **没有令牌就不开门。** 不存在"不鉴权的调试模式"。
package webhook

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/event"
)

// MinTokenLen 是令牌的最短长度。短令牌能被暴力猜出来。
const MinTokenLen = 24

// maxBody 限制请求体大小。事件内容最终要进模型的上下文，大了既费钱又是攻击面。
const maxBody = 256 << 10

// Publisher 是处理器依赖的发布能力，由 *event.Bus 提供。
type Publisher interface {
	Publish(ctx context.Context, e *event.Event) (int, error)
}

// Handler 返回 Webhook 的 HTTP 处理器。
func Handler(bus Publisher, token string, log *slog.Logger) (http.Handler, error) {
	if log == nil {
		log = slog.Default()
	}
	if len(token) < MinTokenLen {
		return nil, fmt.Errorf("webhook: 令牌至少 %d 个字符，实际 %d", MinTokenLen, len(token))
	}
	want := []byte(token)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /events/{topic}", func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		// 常数时间比较：按字节提前返回的比较会让人靠响应时间一位一位猜出令牌。
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			reply(w, http.StatusUnauthorized, map[string]any{"error": "令牌不正确"})
			return
		}

		topic := r.PathValue("topic")
		if err := event.ValidateTopic(topic); err != nil {
			reply(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				reply(w, http.StatusRequestEntityTooLarge,
					map[string]any{"error": fmt.Sprintf("请求体不能超过 %d 字节", maxBody)})
				return
			}
			reply(w, http.StatusBadRequest, map[string]any{"error": "读取请求体失败"})
			return
		}
		var payload json.RawMessage
		if len(body) > 0 {
			if !json.Valid(body) {
				reply(w, http.StatusBadRequest, map[string]any{"error": "请求体必须是 JSON"})
				return
			}
			payload = body
		}

		e := event.New(topic, payload, core.TrustExternal, "webhook")
		// 调用方发错了东西，要明确告诉它错在哪。
		if err := e.Validate(); err != nil {
			reply(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		woken, err := bus.Publish(r.Context(), e)
		if err != nil {
			// 我们自己这边出的错：细节留在服务端日志里，不回给调用方。
			log.Error("Webhook 事件处理失败", "event", e.ID, "topic", topic, "err", err)
			reply(w, http.StatusInternalServerError, map[string]any{"error": "事件处理失败", "event_id": e.ID})
			return
		}
		reply(w, http.StatusAccepted, map[string]any{"event_id": e.ID, "woken": woken})
	})
	return mux, nil
}

func reply(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// 响应已经开始写，这里再出错也无从告知调用方。
	_ = json.NewEncoder(w).Encode(body)
}

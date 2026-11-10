package device

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Prefix 是设备接口的路径前缀。
const Prefix = "/api/device/v1/"

const (
	maxBody = 4 << 20
	// 配对码有 31^8 种，十分钟有效；再限制每分钟的失败次数，猜是猜不出来的。
	pairFailLimit  = 10
	pairFailWindow = time.Minute
)

// Handler 返回设备接口的 HTTP 处理器，路径都在 Prefix 之下。
//
//	POST pair                      配对码换设备令牌
//	GET  me                        设备自己的信息与服务器时间，也用来确认令牌还有效
//	GET  requests                  等用户处理的授权需求
//	POST data                      上报一批数据 {kind, items:[{key, captured_at, payload}]}
//	POST ask                       把用户说的话交给助手 {text, screenshot_base64?, screenshot_mime?}，答复走消息渠道发回
//	POST requests/{id}/fulfil      用户同意并采集完，交上数据 {items:[...]}
//	POST requests/{id}/decline     用户拒绝
//
// 除 pair 外都要 Authorization: Bearer <设备令牌>。
func Handler(svc *Service, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	h := &handler{svc: svc, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+Prefix+"pair", h.pair)
	mux.HandleFunc("GET "+Prefix+"me", h.authed(h.me))
	mux.HandleFunc("GET "+Prefix+"requests", h.authed(h.requests))
	mux.HandleFunc("POST "+Prefix+"data", h.authed(h.data))
	mux.HandleFunc("POST "+Prefix+"ask", h.authed(h.ask))
	mux.HandleFunc("POST "+Prefix+"requests/{id}/fulfil", h.authed(h.fulfil))
	mux.HandleFunc("POST "+Prefix+"requests/{id}/decline", h.authed(h.decline))
	return mux
}

type handler struct {
	svc *Service
	log *slog.Logger

	mu    sync.Mutex
	fails []time.Time
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// 响应已经开始写，这里再出错也无从告知调用方。
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]any{"error": msg})
}

// serverError 把我们自己这边的错误细节留在日志里，不回给调用方。
func (h *handler) serverError(w http.ResponseWriter, what string, err error) {
	h.log.Error("设备接口出错", "操作", what, "err", err)
	fail(w, http.StatusInternalServerError, "服务器出错了")
}

// failure 把业务错误翻成状态码；不认识的当作服务器内部错误。
func (h *handler) failure(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, http.StatusNotFound, "不存在或已失效")
	case errors.Is(err, ErrForbidden):
		fail(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrInvalid):
		fail(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrUnavailable):
		fail(w, http.StatusServiceUnavailable, err.Error())
	default:
		h.serverError(w, what, err)
	}
}

func (h *handler) authed(next func(http.ResponseWriter, *http.Request, *Device)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			fail(w, http.StatusUnauthorized, "缺少设备令牌")
			return
		}
		d, err := h.svc.Authenticate(r.Context(), tok)
		if errors.Is(err, ErrNotFound) {
			fail(w, http.StatusUnauthorized, "设备令牌不正确或已被吊销")
			return
		}
		if err != nil {
			h.serverError(w, "认证", err)
			return
		}
		// 顺手记下这台手机此刻的公网 IP 归属地；它是旁路信息，记不记得上都不影响这次请求。
		h.svc.NoteIP(r.Context(), d, clientIP(r))
		next(w, r, d)
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return readJSONLimit(w, r, v, maxBody)
}

func readJSONLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, http.StatusRequestEntityTooLarge, "请求体太大")
		} else {
			fail(w, http.StatusBadRequest, "读取请求体失败")
		}
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法的 JSON："+err.Error())
		return false
	}
	return true
}

// tooManyPairFails 报告最近一分钟里配对失败是否已经太多，同时清掉过期的记录。
func (h *handler) tooManyPairFails() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	cut := time.Now().Add(-pairFailWindow)
	kept := h.fails[:0]
	for _, t := range h.fails {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	h.fails = kept
	return len(h.fails) >= pairFailLimit
}

func (h *handler) recordPairFail() {
	h.mu.Lock()
	h.fails = append(h.fails, time.Now())
	h.mu.Unlock()
}

func (h *handler) pair(w http.ResponseWriter, r *http.Request) {
	if h.tooManyPairFails() {
		w.Header().Set("Retry-After", "60")
		fail(w, http.StatusTooManyRequests, "配对失败次数太多，请一分钟后再试")
		return
	}
	var in struct {
		Code  string `json:"code"`
		Model string `json:"model"`
		OS    string `json:"os"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	d, tok, err := h.svc.Pair(r.Context(), in.Code, in.Model, in.OS)
	if errors.Is(err, ErrNotFound) {
		h.recordPairFail()
		fail(w, http.StatusNotFound, "配对码不对或已过期")
		return
	}
	if err != nil {
		h.failure(w, "配对", err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"device_id": d.ID, "name": d.Name, "token": tok})
}

func (h *handler) me(w http.ResponseWriter, _ *http.Request, d *Device) {
	reply(w, http.StatusOK, map[string]any{
		"device_id": d.ID, "name": d.Name, "server_time": h.svc.now(), "kinds": kindList(), "sync_minutes": syncMinutes(d),
	})
}

// syncMinutes 是这台设备该用的上报间隔；老数据没有值时用默认。
func syncMinutes(d *Device) int {
	if d.SyncMinutes <= 0 {
		return DefaultSyncMinutes
	}
	return d.SyncMinutes
}

func kindList() []map[string]any {
	out := make([]map[string]any, 0, len(Kinds))
	for _, k := range Kinds {
		out = append(out, map[string]any{"id": k.ID, "label": k.Label, "per_use": k.PerUse})
	}
	return out
}

func (h *handler) requests(w http.ResponseWriter, r *http.Request, d *Device) {
	list, err := h.svc.Pending(r.Context(), d)
	if err != nil {
		h.failure(w, "列出需求", err)
		return
	}
	if list == nil {
		list = []*Request{}
	}
	reply(w, http.StatusOK, map[string]any{"requests": list, "sync_minutes": syncMinutes(d)})
}

type itemsBody struct {
	Kind  string `json:"kind"`
	Items []Item `json:"items"`
}

func (h *handler) data(w http.ResponseWriter, r *http.Request, d *Device) {
	var in itemsBody
	if !readJSON(w, r, &in) {
		return
	}
	n, err := h.svc.Ingest(r.Context(), d, in.Kind, in.Items)
	if err != nil {
		h.failure(w, "上报", err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"received": len(in.Items), "added": n})
}

// maxAskBody 是提问请求体的上限：一张 3MB 以内的截屏转成 base64 要大约 4MB。
const maxAskBody = 6 << 20

func (h *handler) ask(w http.ResponseWriter, r *http.Request, d *Device) {
	var in struct {
		Text       string `json:"text"`
		Screenshot string `json:"screenshot_base64"`
		MIME       string `json:"screenshot_mime"`
	}
	if !readJSONLimit(w, r, &in, maxAskBody) {
		return
	}
	var img *AskImage
	if in.Screenshot != "" {
		data, err := base64.StdEncoding.DecodeString(in.Screenshot)
		if err != nil {
			fail(w, http.StatusBadRequest, "截屏不是合法的 base64")
			return
		}
		img = &AskImage{MIME: in.MIME, Data: data}
	}
	if err := h.svc.Ask(r.Context(), in.Text, img); err != nil {
		h.failure(w, "提问", err)
		return
	}
	h.log.Info("手机提问已交给助手", "设备", d.Name, "字数", utf8.RuneCountInString(in.Text), "带截屏", img != nil)
	reply(w, http.StatusAccepted, map[string]any{"accepted": true})
}

func (h *handler) fulfil(w http.ResponseWriter, r *http.Request, d *Device) {
	var in itemsBody
	if !readJSON(w, r, &in) {
		return
	}
	n, err := h.svc.Fulfil(r.Context(), d, r.PathValue("id"), in.Items)
	if err != nil {
		h.failure(w, "交付需求", err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"received": len(in.Items), "added": n})
}

func (h *handler) decline(w http.ResponseWriter, r *http.Request, d *Device) {
	if err := h.svc.Decline(r.Context(), d, r.PathValue("id")); err != nil {
		h.failure(w, "拒绝需求", err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"declined": true})
}

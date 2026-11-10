package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/device"
	"github.com/xiguac/atomassistant/internal/kernel"
	"github.com/xiguac/atomassistant/internal/policy"
)

// 手机 App 用的管理接口，前缀 /api/app/v1/。给手机上的原子助手 App 代替网页后台用。
//
// 鉴权：Authorization: Bearer <设备令牌>，并且这台设备必须在后台「手机」页被打开了「允许管理后台」。
// 光有令牌只能上报数据，没有开关就什么都管不了——手机丢了、令牌泄露，对方也只能往里传数据。
// 令牌在手机上由 Face ID 把门（App 打开管理页要先刷脸），服务端这一层管的是令牌被别人拿走的情况。
//
// 约定：GET 读，POST 写；返回 JSON；出错是 {"error": "..."} 加相应的状态码。
// 能在网页后台做的操作，在这里做的就是同样的操作，走同一套内核入口，没有另一套规则。
const appPrefix = "/api/app/v1/"

// appHandler 是挂在后台监听上的手机管理接口。
func (s *Server) appHandler() http.Handler {
	mux := http.NewServeMux()
	a := s.appAuthed
	mux.HandleFunc("GET "+appPrefix+"overview", a(s.appOverview))
	mux.HandleFunc("GET "+appPrefix+"tasks", a(s.appTasks))
	mux.HandleFunc("POST "+appPrefix+"tasks", a(s.appTaskCreate))
	mux.HandleFunc("GET "+appPrefix+"tasks/{id}", a(s.appTask))
	mux.HandleFunc("POST "+appPrefix+"tasks/{id}/approve", a(s.appTaskApprove))
	mux.HandleFunc("POST "+appPrefix+"tasks/{id}/reject", a(s.appTaskReject))
	mux.HandleFunc("POST "+appPrefix+"tasks/{id}/cancel", a(s.appTaskCancel))
	mux.HandleFunc("POST "+appPrefix+"tasks/{id}/reply", a(s.appTaskReply))
	mux.HandleFunc("POST "+appPrefix+"tasks/{id}/budget", a(s.appTaskBudget))
	s.appProxyRoutes(mux)
	return mux
}

func appReply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// 响应已经开始写，这里再出错也无从告知调用方。
	_ = json.NewEncoder(w).Encode(body)
}

func appFail(w http.ResponseWriter, status int, msg string) {
	appReply(w, status, map[string]any{"error": msg})
}

func appOK(w http.ResponseWriter, msg string) {
	appReply(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
}

// appAuthed 认出设备，并要求它有管理权限。
func (s *Server) appAuthed(next func(http.ResponseWriter, *http.Request, *device.Device)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Devices == nil {
			appFail(w, http.StatusNotFound, "手机数据源未启用")
			return
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			appFail(w, http.StatusUnauthorized, "缺少设备令牌")
			return
		}
		d, err := s.opts.Devices.Authenticate(r.Context(), tok)
		if errors.Is(err, device.ErrNotFound) {
			appFail(w, http.StatusUnauthorized, "设备令牌不正确或已被吊销")
			return
		}
		if err != nil {
			s.opts.Log.Error("手机管理接口认证出错", "err", err)
			appFail(w, http.StatusInternalServerError, "服务器出错了")
			return
		}
		if !d.Admin {
			appFail(w, http.StatusForbidden, "这台手机没有管理权限。请在网页后台的「手机」页，给它打开「允许管理后台」")
			return
		}
		next(w, r, d)
	}
}

func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		appFail(w, http.StatusBadRequest, "读取请求体失败")
		return false
	}
	if len(body) == 0 {
		return true
	}
	if err := json.Unmarshal(body, v); err != nil {
		appFail(w, http.StatusBadRequest, "请求体不是合法的 JSON："+err.Error())
		return false
	}
	return true
}

// ---- 任务 ----

type appTask struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	NeedsUser bool      `json:"needs_user"`
	Live      bool      `json:"live"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Tokens    int64     `json:"tokens"`
	CostYuan  float64   `json:"cost_yuan"`
}

func toAppTask(t *core.Task) appTask {
	return appTask{
		ID: t.ID, Title: t.Title, State: string(t.State), Reason: t.Reason,
		NeedsUser: needsUser(t), Live: !t.State.Terminal(),
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		Tokens: t.Usage.Tokens, CostYuan: float64(t.Usage.CostCents) / 100,
	}
}

func (s *Server) appOverview(w http.ResponseWriter, r *http.Request, _ *device.Device) {
	ctx := r.Context()
	out := map[string]any{"assistant_ok": true}
	if s.opts.Status != nil {
		if msg := s.opts.Status(); msg != "" {
			out["assistant_ok"], out["assistant_problem"] = false, msg
		}
	}
	waiting, active := 0, 0
	recent := []appTask{}
	tasks, err := s.opts.Store.ListTasks(ctx, true, 1000)
	if err != nil {
		appFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, t := range tasks {
		switch {
		case isSession(t):
			// 每天的会话只是个容器，不算一件在办的事。
		case needsUser(t):
			waiting++
			recent = append(recent, toAppTask(t))
		default:
			active++
			recent = append(recent, toAppTask(t))
		}
	}
	if len(recent) > 8 {
		recent = recent[:8]
	}
	out["waiting"], out["active"], out["recent"] = waiting, active, recent
	if reqs, err := s.requests().List(ctx); err == nil {
		out["vault_requests"] = len(reqs)
	}
	if pending, err := s.opts.Store.Memories().ListPending(ctx, ""); err == nil {
		out["memory_pending"] = len(pending)
	}
	now := time.Now()
	if u, err := s.opts.Store.UsageOn(ctx, now); err == nil {
		out["today_tokens"], out["today_cost_yuan"] = u.Tokens(), float64(u.CostMicros)/1e6
	}
	appReply(w, http.StatusOK, out)
}

func (s *Server) appTasks(w http.ResponseWriter, r *http.Request, _ *device.Device) {
	// 手机上要直接看到聊天里交代的事（它们是当天会话的子任务），所以连子任务一起列。
	q := r.URL.Query()
	q.Set("flat", "1")
	l, err := s.taskList(r.Context(), q, time.Now())
	if err != nil {
		appFail(w, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]appTask, 0, len(l.Tasks))
	for _, t := range l.Tasks {
		if isSession(t) {
			continue
		}
		out = append(out, toAppTask(t))
	}
	appReply(w, http.StatusOK, map[string]any{"tasks": out, "total": l.Total, "page": l.Page, "pages": l.Pages})
}

type appStep struct {
	Seq        int    `json:"seq"`
	Type       string `json:"type"`
	Tool       string `json:"tool,omitempty"`
	Status     string `json:"status"`
	SideEffect string `json:"side_effect,omitempty"`
	Input      string `json:"input,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
}

type appField struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type appApproval struct {
	Tool         string     `json:"tool"`
	Fields       []appField `json:"fields"`
	Irreversible bool       `json:"irreversible"`
	Why          string     `json:"why,omitempty"`
	Rule         string     `json:"rule,omitempty"`
}

type appBudget struct {
	SuggestTokens int64   `json:"suggest_tokens"`
	SuggestYuan   float64 `json:"suggest_yuan"`
	SuggestHours  float64 `json:"suggest_hours"`
	UsageYuan     float64 `json:"usage_yuan"`
}

func (s *Server) appTask(w http.ResponseWriter, r *http.Request, _ *device.Device) {
	ctx := r.Context()
	id := r.PathValue("id")
	t, err := s.opts.Store.GetTask(ctx, id)
	if err != nil {
		appFail(w, http.StatusNotFound, "任务不存在")
		return
	}
	if isSession(t) {
		appFail(w, http.StatusNotFound, "这是一天的会话容器，不是一件任务")
		return
	}
	steps, err := s.opts.Store.ListSteps(ctx, id)
	if err != nil {
		appFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{"task": toAppTask(t), "goal": t.Goal}
	list := make([]appStep, 0, len(steps))
	for _, st := range steps {
		list = append(list, appStep{
			Seq: st.Seq, Type: string(st.Type), Tool: st.ToolName, Status: string(st.Status),
			SideEffect: string(st.SideEffect), Input: pretty(st.Input, 1500), Output: pretty(st.Output, 1500), Error: st.Err,
		})
	}
	out["steps"] = list

	if t.State == core.StateSuspended && strings.HasPrefix(t.Reason, "预算超限") {
		out["over_budget"] = appBudget{
			SuggestTokens: max(t.Budget.MaxTokens*2, t.Usage.Tokens*2),
			SuggestYuan:   float64(max(t.Budget.MaxCostCents*2, t.Usage.CostCents*2)) / 100,
			SuggestHours:  max(t.Budget.MaxWallClock*2, t.Usage.WallClock*2).Hours(),
			UsageYuan:     float64(t.Usage.CostCents) / 100,
		}
	}
	var pendingFound bool
	if t.State == core.StateWaiting && t.Wake != nil && t.Wake.Kind == core.WakeApproval {
		if p, err := s.opts.Tasks.PendingAction(ctx, id); err == nil && p != nil {
			pendingFound = true
			ap := appApproval{Tool: p.Tool}
			for _, f := range fields(p.Params) {
				ap.Fields = append(ap.Fields, appField{Key: f.Key, Value: f.Value})
			}
			ap.Rule, ap.Why = kernel.SplitApprovalReason(t.Reason)
			if ap.Rule == policy.ReasonNoRule {
				ap.Rule = ""
			}
			for _, st := range steps {
				if st.Status == core.StepAwaiting {
					ap.Irreversible = st.SideEffect == core.SideEffectIrreversible
				}
			}
			out["approval"] = ap
		}
	}
	if needsUser(t) && !pendingFound && t.State == core.StateWaiting {
		out["question"] = strings.TrimPrefix(t.Reason, "等待你的回复：")
	}
	appReply(w, http.StatusOK, out)
}

func (s *Server) appTaskCreate(w http.ResponseWriter, r *http.Request, d *device.Device) {
	var in struct {
		Goal string `json:"goal"`
	}
	if !readBody(w, r, &in) {
		return
	}
	goal := strings.TrimSpace(in.Goal)
	if goal == "" {
		appFail(w, http.StatusBadRequest, "请填写任务内容")
		return
	}
	title := goal
	if i := strings.IndexAny(title, "\n。"); i > 0 {
		title = title[:i]
	}
	if rs := []rune(title); len(rs) > 24 {
		title = string(rs[:24]) + "…"
	}
	// 手机管理端和网页后台一样，是用户亲手使用的地方，这里建的任务是用户等级的。
	id, err := s.opts.Tasks.Create(r.Context(), title, goal, nil)
	if err != nil {
		appFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.opts.Log.Info("手机上新建了任务", "任务", id, "设备", d.Name)
	appReply(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) appTaskApprove(w http.ResponseWriter, r *http.Request, d *device.Device) {
	id := r.PathValue("id")
	if err := s.opts.Tasks.Approve(r.Context(), id); err != nil {
		appFail(w, http.StatusConflict, err.Error())
		return
	}
	s.opts.Log.Info("任务已在手机上批准", "任务", id, "设备", d.Name)
	appOK(w, "已批准，任务将继续执行。")
}

func (s *Server) appTaskReject(w http.ResponseWriter, r *http.Request, d *device.Device) {
	var in struct {
		Reason string `json:"reason"`
	}
	if !readBody(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	if err := s.opts.Tasks.Reject(r.Context(), id, strings.TrimSpace(in.Reason)); err != nil {
		appFail(w, http.StatusConflict, err.Error())
		return
	}
	s.opts.Log.Info("任务已在手机上驳回", "任务", id, "设备", d.Name)
	appOK(w, "已驳回，助手将按修改意见重新处理。")
}

func (s *Server) appTaskCancel(w http.ResponseWriter, r *http.Request, d *device.Device) {
	id := r.PathValue("id")
	_, err := kernel.Cancel(r.Context(), s.opts.Store, id, "用户在手机上取消")
	if errors.Is(err, kernel.ErrAlreadyEnded) {
		appFail(w, http.StatusConflict, "任务已结束，无需取消")
		return
	}
	if err != nil {
		appFail(w, http.StatusConflict, err.Error())
		return
	}
	s.opts.Log.Info("任务已在手机上取消", "任务", id, "设备", d.Name)
	appOK(w, "任务已取消。")
}

func (s *Server) appTaskReply(w http.ResponseWriter, r *http.Request, d *device.Device) {
	var in struct {
		Text string `json:"text"`
	}
	if !readBody(w, r, &in) {
		return
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		appFail(w, http.StatusBadRequest, "请填写回复内容")
		return
	}
	if err := s.opts.Tasks.Reply(r.Context(), r.PathValue("id"), text, "app"); err != nil {
		appFail(w, http.StatusConflict, err.Error())
		return
	}
	appOK(w, "已发送。")
}

func (s *Server) appTaskBudget(w http.ResponseWriter, r *http.Request, d *device.Device) {
	var in struct {
		MaxTokens   *int64   `json:"max_tokens"`
		MaxCostYuan *float64 `json:"max_cost_yuan"`
		MaxHours    *float64 `json:"max_hours"`
	}
	if !readBody(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	t, err := s.opts.Store.GetTask(r.Context(), id)
	if err != nil {
		appFail(w, http.StatusNotFound, "任务不存在")
		return
	}
	// 没填的项沿用原来的上限。
	b := t.Budget
	if in.MaxTokens != nil {
		if *in.MaxTokens < 0 {
			appFail(w, http.StatusBadRequest, "token 上限须为非负整数")
			return
		}
		b.MaxTokens = *in.MaxTokens
	}
	if in.MaxCostYuan != nil {
		if *in.MaxCostYuan < 0 {
			appFail(w, http.StatusBadRequest, "金额上限须为非负数")
			return
		}
		b.MaxCostCents = int64(*in.MaxCostYuan*100 + 0.5)
	}
	if in.MaxHours != nil {
		if *in.MaxHours < 0 {
			appFail(w, http.StatusBadRequest, "执行时长上限须为非负数")
			return
		}
		b.MaxWallClock = time.Duration(*in.MaxHours * float64(time.Hour))
	}
	switch err := kernel.RaiseBudget(r.Context(), s.opts.Store, id, b); {
	case errors.Is(err, kernel.ErrNotSuspended):
		appFail(w, http.StatusConflict, "任务不在挂起状态，无需继续")
	case errors.Is(err, kernel.ErrBudgetTooLow):
		appFail(w, http.StatusConflict, "新的上限须高于已用量，否则任务会立即再次挂起")
	case err != nil:
		appFail(w, http.StatusInternalServerError, err.Error())
	default:
		s.opts.Log.Info("任务预算已在手机上调高", "任务", id, "设备", d.Name)
		appOK(w, "已设置新的上限，任务继续执行。")
	}
}

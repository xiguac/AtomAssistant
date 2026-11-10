package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"

	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/device"
)

// 手机端的「通用代理」：网页后台已有的每一个页面和每一个操作，手机 App 都能原样用，不用逐个重写。
//
// 做法：Handler 把所有需要登录的路由，同时登记到一张内部路由表 appMux（不带登录会话和防伪令牌那层，
// 因为手机这边的认证是设备令牌 + 管理权限 + 面容 ID）。手机发来的请求在这里转成一次对 appMux 的内部调用：
//
//	GET  /api/app/v1/page?path=/vault   调页面的处理函数，截下它要渲染的数据，转成手机界面的描述（见 appui.go）
//	POST /api/app/v1/do                 调写操作的处理函数，{path, form}，返回 {ok, message}
//	POST /api/app/v1/upload             上传文件到工作区（转给 POST /files）
//	GET  /api/app/v1/raw?path=/files/download?...   原样转回文件下载这类不是页面的响应
//
// 所以规则一处管：校验、操作日志、权限，都还是后台原来的那一套，没有第二份。

// renderCapturer 由「截数据」用的响应写入器实现：render 看到它，就把要渲染的数据交给它，不去执行模板。
type renderCapturer interface {
	capture(name string, v *view)
}

// pageCapture 是一次内部调用的响应：既记下普通的响应，也能截下页面要渲染的数据。
type pageCapture struct {
	*httptest.ResponseRecorder
	name string
	view *view
}

func newCapture() *pageCapture { return &pageCapture{ResponseRecorder: httptest.NewRecorder()} }

func (c *pageCapture) capture(name string, v *view) { c.name, c.view = name, v }

// registerApp 把一个需要登录的路由也登记到手机用的内部路由表。
func (s *Server) registerApp(pattern string, h func(http.ResponseWriter, *http.Request, *view)) {
	// 退出登录是网页会话的事，手机上没有这个概念。
	if pattern == "POST /logout" {
		return
	}
	s.appMux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		v := &view{Notice: r.URL.Query().Get("ok"), Error: r.URL.Query().Get("e")}
		if r.Method != http.MethodPost {
			h(w, r, v)
			return
		}
		// 和网页里一样，每次改动记一条操作日志；结果由 back 填上。
		o := &outcome{}
		h(w, r.WithContext(context.WithValue(r.Context(), outcomeKey{}, o)), v)
		s.logOperation(r, o)
	})
}

// dispatch 把一次请求交给内部路由表，返回响应。
func (s *Server) dispatch(r *http.Request) *pageCapture {
	c := newCapture()
	s.appMux.ServeHTTP(c, r)
	return c
}

// internalPath 校验手机给的路径：必须是站内的绝对路径，不能带主机，不能绕出去。
func internalPath(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host != "" || u.Scheme != "" || !strings.HasPrefix(u.Path, "/") || strings.Contains(u.Path, "..") {
		return nil, false
	}
	// 手机管理接口和设备接口本身不能再经代理绕一圈。
	if strings.HasPrefix(u.Path, "/api/") {
		return nil, false
	}
	return u, true
}

func (s *Server) appProxyRoutes(mux *http.ServeMux) {
	a := s.appAuthed
	mux.HandleFunc("GET "+appPrefix+"hub", a(s.appHub))
	mux.HandleFunc("GET "+appPrefix+"sessions", a(s.appSessions))
	mux.HandleFunc("GET "+appPrefix+"page", a(s.appPage))
	mux.HandleFunc("POST "+appPrefix+"do", a(s.appDo))
	mux.HandleFunc("POST "+appPrefix+"upload", a(s.appUpload))
	mux.HandleFunc("GET "+appPrefix+"raw", a(s.appRaw))
}

// appPage 取一个页面，返回手机界面的描述。
func (s *Server) appPage(w http.ResponseWriter, r *http.Request, _ *device.Device) {
	u, ok := internalPath(r.URL.Query().Get("path"))
	if !ok {
		appFail(w, http.StatusBadRequest, "路径不对")
		return
	}
	req := r.Clone(r.Context())
	req.Method, req.URL, req.RequestURI, req.Body = http.MethodGet, u, "", http.NoBody
	c := s.dispatch(req)
	if c.view == nil {
		// 不是一个渲染出来的页面：可能是 404，或者处理函数自己直接给了别的响应。
		if loc := c.Header().Get("Location"); loc != "" {
			appReply(w, http.StatusOK, map[string]any{"redirect": loc})
			return
		}
		status := c.Code
		if status < 400 {
			status = http.StatusNotFound
		}
		appFail(w, status, "这个页面在手机上打不开")
		return
	}
	page, ok := s.uiPage(c.name, c.view, u)
	if !ok {
		appFail(w, http.StatusNotFound, "这个页面还没有手机版")
		return
	}
	appReply(w, http.StatusOK, page)
}

// appDo 执行一个写操作。
func (s *Server) appDo(w http.ResponseWriter, r *http.Request, d *device.Device) {
	var in struct {
		Path string              `json:"path"`
		Form map[string][]string `json:"form"`
	}
	if !readBody(w, r, &in) {
		return
	}
	u, ok := internalPath(in.Path)
	if !ok {
		appFail(w, http.StatusBadRequest, "路径不对")
		return
	}
	form := url.Values(in.Form)
	if form == nil {
		form = url.Values{}
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, u.String(), bytes.NewBufferString(form.Encode()))
	if err != nil {
		appFail(w, http.StatusBadRequest, "路径不对")
		return
	}
	req.RemoteAddr = r.RemoteAddr
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// 让 back 直接回 JSON，而不是跳转。
	req.Header.Set("X-Requested-With", "fetch")
	s.proxyReply(w, s.dispatch(req))
}

// appUpload 把手机上选的文件交给「文件」页的上传处理。
func (s *Server) appUpload(w http.ResponseWriter, r *http.Request, d *device.Device) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+(1<<20))
	req := r.Clone(r.Context())
	req.URL = &url.URL{Path: "/files"}
	req.RequestURI = ""
	req.Header.Set("X-Requested-With", "fetch")
	s.proxyReply(w, s.dispatch(req))
}

// appRaw 原样转回一个不是页面的响应（比如下载一个文件）。
func (s *Server) appRaw(w http.ResponseWriter, r *http.Request, _ *device.Device) {
	u, ok := internalPath(r.URL.Query().Get("path"))
	if !ok {
		appFail(w, http.StatusBadRequest, "路径不对")
		return
	}
	req := r.Clone(r.Context())
	req.Method, req.URL, req.RequestURI, req.Body = http.MethodGet, u, "", http.NoBody
	c := s.dispatch(req)
	for _, h := range []string{"Content-Type", "Content-Disposition", "Content-Length"} {
		if v := c.Header().Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(c.Code)
	_, _ = io.Copy(w, c.Body)
}

// proxyReply 把一次内部调用的结果转成统一的 {ok, message}：back 已经给了 JSON 就用它；
// 处理函数选择了跳转的（带 ?ok= 或 ?e=），把跳转里的说明取出来；渲染了页面的（比如添加设备后显示配对码），带上页面。
func (s *Server) proxyReply(w http.ResponseWriter, c *pageCapture) {
	if c.view != nil {
		out := map[string]any{"ok": c.view.Error == "", "message": firstNonEmpty(c.view.Error, c.view.Notice)}
		if page, ok := s.uiPage(c.name, c.view, &url.URL{Path: "/"}); ok {
			out["page"] = page
		}
		appReply(w, http.StatusOK, out)
		return
	}
	if strings.HasPrefix(c.Header().Get("Content-Type"), "application/json") {
		var m map[string]any
		if json.Unmarshal(c.Body.Bytes(), &m) == nil {
			appReply(w, http.StatusOK, m)
			return
		}
	}
	if loc := c.Header().Get("Location"); loc != "" {
		out := map[string]any{"ok": true, "redirect": loc}
		if u, err := url.Parse(loc); err == nil {
			if e := u.Query().Get("e"); e != "" {
				out["ok"], out["message"] = false, e
			} else if m := u.Query().Get("ok"); m != "" {
				out["message"] = m
			}
		}
		appReply(w, http.StatusOK, out)
		return
	}
	if c.Code >= 400 {
		appFail(w, c.Code, strings.TrimSpace(c.Body.String()))
		return
	}
	appReply(w, http.StatusOK, map[string]any{"ok": true})
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// appSessions 列出每一天的会话（最近的在前），点进去是那一天的摘要和原话。
func (s *Server) appSessions(w http.ResponseWriter, r *http.Request, _ *device.Device) {
	tasks, err := s.opts.Store.ListTasks(r.Context(), false, 400)
	if err != nil {
		appFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	sec := uiSection{Footer: "每天的会话会在过了凌晨 4 点后收尾。摘要和原话只保留 " + strconv.Itoa(summaryKeepDays) + " 天。"}
	for _, t := range tasks {
		day, ok := channel.SessionDay(t.Tags)
		if !ok {
			continue
		}
		it := row(day, t.Title)
		it.Page = "/tasks/" + t.ID
		sec.Items = append(sec.Items, it)
	}
	if len(sec.Items) == 0 {
		sec.Items = append(sec.Items, textItem("还没有会话记录。"))
	}
	appReply(w, http.StatusOK, uiPage{Title: "会话记录", Tabs: tabsFor("/_sessions"), Sections: []uiSection{sec}})
}

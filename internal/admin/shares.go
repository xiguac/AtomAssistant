package admin

import (
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/share"
)

// 「文件」页里的「下载链接」一节：助手给工作区文件建的临时下载链接。
// 在这里设置链接的外部访问地址，查看此刻有效的链接，提前撤销。

// sharesData 是「下载链接」一节要显示的东西。
type sharesData struct {
	// On 为真表示开了下载入口（ATOM_SHARE_ADDR）。
	On bool
	// Base 是已保存的外部访问地址；Suggest 是还没保存时按当前访问的域名给出的建议值。
	Base    string
	Suggest string
	Links   []*share.Link
}

// loadShares 读出「下载链接」一节的数据。读失败时返回错误，页面其余部分照常显示。
func (s *Server) loadShares(r *http.Request) (sharesData, error) {
	d := sharesData{On: s.opts.ShareOn}
	base, err := s.opts.Store.Setting(r.Context(), share.BaseURLSetting)
	if err != nil {
		return d, err
	}
	d.Base = base
	if base == "" {
		d.Suggest = suggestBase(r)
	}
	if d.Links, err = s.opts.Store.ActiveShares(r.Context(), time.Now().UTC()); err != nil {
		return d, err
	}
	return d, nil
}

// suggestBase 按当前访问后台用的地址给出外部访问地址的建议值：同一个域名下的 /download。
func suggestBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/download"
}

// shareBaseSave 保存外部访问地址。之后新建的链接以它开头，已经建好的链接不受影响。
func (s *Server) shareBaseSave(w http.ResponseWriter, r *http.Request, _ *view) {
	base, err := share.NormalizeBaseURL(r.PostFormValue("base"))
	if err == nil {
		err = s.opts.Store.PutSetting(r.Context(), share.BaseURLSetting, base)
	}
	if err != nil {
		http.Redirect(w, r, "/files?e="+template.URLQueryEscaper(err.Error()), http.StatusSeeOther)
		return
	}
	s.opts.Log.Info("下载链接的外部访问地址已在后台修改", "地址", base, "来自", r.RemoteAddr)
	http.Redirect(w, r, "/files?ok="+template.URLQueryEscaper("已保存外部访问地址"), http.StatusSeeOther)
}

// shareRevoke 提前撤销一个下载链接。
func (s *Server) shareRevoke(w http.ResponseWriter, r *http.Request, _ *view) {
	id := strings.TrimSpace(r.PostFormValue("id"))
	ok, err := s.opts.Store.RevokeShare(r.Context(), id, time.Now().UTC())
	if err == nil && !ok {
		err = share.ErrNotFound
	}
	if err != nil {
		http.Redirect(w, r, "/files?e="+template.URLQueryEscaper(err.Error()), http.StatusSeeOther)
		return
	}
	s.opts.Log.Info("下载链接已在后台撤销", "链接", id, "来自", r.RemoteAddr)
	http.Redirect(w, r, "/files?ok="+template.URLQueryEscaper("已撤销下载链接"), http.StatusSeeOther)
}

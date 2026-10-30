package share

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/files"
)

// maxConcurrent 是同时进行的下载数。文件整个读进内存再发出去，不限的话几个大文件就能把内存吃光。
const maxConcurrent = 4

// contentTypes 是按扩展名给出的内容类型。只列不会被浏览器当成网页执行的；其余一律按二进制文件给。
// 别的服务按地址取图片、PDF、音视频时往往要看内容类型，所以这几类给准。
var contentTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".pdf": "application/pdf", ".txt": "text/plain; charset=utf-8", ".csv": "text/csv; charset=utf-8",
	".json": "application/json", ".mp3": "audio/mpeg", ".wav": "audio/wav", ".mp4": "video/mp4",
	".zip":  "application/zip",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// Handler 返回下载入口：
//
//	GET /s/<令牌>            文件链接：下载这个文件；目录链接：列出其中的文件和各自的下载地址（JSON）
//	GET /s/<令牌>/<路径>     目录链接里的一个文件，路径相对于分享的目录
//
// 不提供别的任何东西。
func (s *Service) Handler(log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	slots := make(chan struct{}, maxConcurrent)
	mux := http.NewServeMux()
	serve := func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		token := r.PathValue("token")
		if !validToken(token) {
			http.NotFound(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		l, err := s.Store.ShareByToken(ctx, hashToken(token), s.clock())
		cancel()
		if errors.Is(err, ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			log.Error("查找分享链接失败", "err", err)
			http.Error(w, "服务暂时不可用", http.StatusServiceUnavailable)
			return
		}
		sub := r.PathValue("sub")
		if sub == "" && l.Dir {
			base, err := s.Settings.Setting(r.Context(), BaseURLSetting)
			if err != nil {
				log.Error("读取外部访问地址失败", "err", err)
				http.Error(w, "服务暂时不可用", http.StatusServiceUnavailable)
				return
			}
			listing(w, l, base, token)
			return
		}
		p, ok := pick(l, sub)
		if !ok {
			http.NotFound(w, r)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.Header().Set("Retry-After", "5")
			http.Error(w, "下载的人太多，请稍后再试", http.StatusServiceUnavailable)
			return
		}
		data, err := s.Workspace.SharedBytes(p)
		if err != nil {
			// 文件被删了、改了：对外只说没有，原因记在日志里。
			log.Warn("分享的文件取不出来", "链接", l.ID, "文件", p.Path, "err", err)
			http.NotFound(w, r)
			return
		}
		log.Info("分享的文件被下载", "链接", l.ID, "文件", p.Path, "来源", client(r))
		name := path.Base(p.Path)
		ct, known := contentTypes[strings.ToLower(path.Ext(name))]
		if !known {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		// 一律按附件给，不让浏览器直接打开。
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+template.URLQueryEscaper(name))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(data)
	}
	mux.HandleFunc("GET /s/{token}", serve)
	mux.HandleFunc("GET /s/{token}/{sub...}", serve)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		http.NotFound(w, r)
	})
	return mux
}

// pick 在链接的文件清单里找要下载的那个。sub 只拿来和清单比对，不拿去拼磁盘路径。
func pick(l *Link, sub string) (files.Pinned, bool) {
	if !l.Dir {
		if sub != "" {
			return files.Pinned{}, false
		}
		return l.Files[0], true
	}
	for _, p := range l.Files {
		if rel, ok := strings.CutPrefix(p.Path, l.Path+"/"); ok && rel == sub {
			return p, true
		}
	}
	return files.Pinned{}, false
}

// client 给出下载者的地址。经本机的反向代理转过来时取代理记下的 X-Forwarded-For，只用于日志。
func client(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); err == nil && ip != nil && ip.IsLoopback() {
		if fwd := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); fwd != "" {
			return fwd
		}
	}
	return r.RemoteAddr
}

// listing 列出目录链接里的文件。
func listing(w http.ResponseWriter, l *Link, base, token string) {
	type item struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
		URL  string `json:"url"`
	}
	out := struct {
		Name      string    `json:"name"`
		ExpiresAt time.Time `json:"expires_at"`
		Files     []item    `json:"files"`
	}{Name: path.Base(l.Path), ExpiresAt: l.ExpiresAt}
	for _, p := range l.Files {
		rel := strings.TrimPrefix(p.Path, l.Path+"/")
		out.Files = append(out.Files, item{Path: rel, Size: p.Size, URL: link(base, token, rel)})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(out)
}

// secureHeaders 设置每个响应都带的安全头：不缓存、不嗅探内容类型、不执行任何东西、不带 Referer 出去。
func secureHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Robots-Tag", "noindex, nofollow")
}

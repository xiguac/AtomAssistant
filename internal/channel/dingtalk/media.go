package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/media"
)

// 发图片、语音、视频和文件。钉钉的单聊消息只引用上传过的媒体文件，而上传只有旧版接口（oapi）。

// defaultOAPIBase 是钉钉旧版接口的地址。
const defaultOAPIBase = "https://oapi.dingtalk.com"

// fileTypes 是钉钉单聊消息能发的文件类型（「机器人消息类型」文档里 sampleFile 的说明）。
var fileTypes = map[string]bool{"xlsx": true, "pdf": true, "zip": true, "rar": true, "doc": true, "docx": true}

// imageFormats 是钉钉上传图片收的格式（「上传媒体文件」文档：jpg、gif、png、bmp）。
var imageFormats = map[string]bool{"image/jpeg": true, "image/gif": true, "image/png": true, "image/bmp": true}

// media 把要发的文件按类传上去，返回消息模板和参数。
//
// 图片发 sampleImageMsg（photoURL 可以直接填上传后的 media_id）；语音发 sampleAudio，先转成 amr、
// 带上毫秒时长；视频发 sampleVideo，只收 mp4，带一张从视频里取的封面和秒数；
// 文件发 sampleFile，钉钉只收 xlsx、pdf、zip、rar、doc、docx。
func (b *Bot) media(ctx context.Context, f *channel.File) (string, map[string]string, error) {
	switch f.Kind {
	case media.KindImage:
		if !imageFormats[media.Sniff(f.Data)] {
			return "", nil, channel.ErrUnsupportedKind
		}
		id, err := b.upload(ctx, "image", f.Name, f.Data)
		if err != nil {
			return "", nil, err
		}
		return "sampleImageMsg", map[string]string{"photoURL": id}, nil
	case media.KindAudio:
		amr, ms, err := media.Default.AMR(ctx, f.Data)
		if err != nil {
			return "", nil, err
		}
		id, err := b.upload(ctx, "voice", strings.TrimSuffix(f.Name, path.Ext(f.Name))+".amr", amr)
		if err != nil {
			return "", nil, err
		}
		return "sampleAudio", map[string]string{"mediaId": id, "duration": strconv.Itoa(ms)}, nil
	case media.KindVideo:
		if media.Sniff(f.Data) != media.MIMEMP4 {
			return "", nil, channel.ErrUnsupportedKind
		}
		cover, err := media.Default.Cover(ctx, f.Data)
		if err != nil {
			return "", nil, err
		}
		// 秒数量不出来（比如没有音轨）时写 1：这一项是必填的，只影响显示。
		secs, err := media.Default.VideoSeconds(ctx, f.Data)
		if err != nil || secs == 0 {
			secs = 1
		}
		pic, err := b.upload(ctx, "image", "cover.jpg", cover)
		if err != nil {
			return "", nil, err
		}
		id, err := b.upload(ctx, "video", f.Name, f.Data)
		if err != nil {
			return "", nil, err
		}
		return "sampleVideo", map[string]string{"duration": strconv.Itoa(secs), "videoMediaId": id, "videoType": "mp4", "picMediaId": pic}, nil
	}
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(f.Name)), ".")
	if !fileTypes[ext] {
		return "", nil, fmt.Errorf("dingtalk: 钉钉只能发 xlsx、pdf、zip、rar、doc、docx 文件，%s 发不了", f.Name)
	}
	id, err := b.upload(ctx, "file", f.Name, f.Data)
	if err != nil {
		return "", nil, err
	}
	return "sampleFile", map[string]string{"mediaId": id, "fileName": f.Name, "fileType": ext}, nil
}

func (b *Bot) oapi() string {
	if b.OAPIBase != "" {
		return strings.TrimSuffix(b.OAPIBase, "/")
	}
	return defaultOAPIBase
}

// legacyToken 换取旧版接口用的凭证（GET /gettoken?appkey=&appsecret=），快过期时再换。
// 和新版接口的凭证分开换、分开存：两者能否通用，钉钉的文档没有写明。
func (b *Bot) legacyToken(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.oapiToken != "" && time.Until(b.oapiExpiry) > tokenSlack {
		return b.oapiToken, nil
	}
	secret, err := b.secret(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{"appkey": {b.AppID}, "appsecret": {secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.oapi()+"/gettoken?"+q.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("dingtalk: 构造请求失败")
	}
	var out struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := b.legacyDo(req, &out); err != nil {
		return "", fmt.Errorf("dingtalk: 换取旧版接口凭证失败: %w", err)
	}
	if out.ErrCode != 0 || out.AccessToken == "" {
		return "", fmt.Errorf("dingtalk: 换取旧版接口凭证失败: %d %s", out.ErrCode, out.ErrMsg)
	}
	b.oapiToken, b.oapiExpiry = out.AccessToken, time.Now().Add(time.Duration(out.ExpiresIn)*time.Second)
	return b.oapiToken, nil
}

// upload 用旧版的"上传媒体文件"接口传一个文件，返回 media_id。typ 是 image、voice、video 或 file。
func (b *Bot) upload(ctx context.Context, typ, name string, data []byte) (string, error) {
	token, err := b.legacyToken(ctx)
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("type", typ); err != nil {
		return "", err
	}
	part, err := w.CreateFormFile("media", name)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.oapi()+"/media/upload?"+url.Values{"access_token": {token}}.Encode(), &body)
	if err != nil {
		return "", fmt.Errorf("dingtalk: 构造请求失败")
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	var out struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		MediaID string `json:"media_id"`
	}
	if err := b.legacyDo(req, &out); err != nil {
		return "", fmt.Errorf("dingtalk: 上传 %s 失败: %w", typ, err)
	}
	if out.ErrCode != 0 || out.MediaID == "" {
		return "", fmt.Errorf("dingtalk: 上传 %s 失败: %d %s", typ, out.ErrCode, out.ErrMsg)
	}
	return out.MediaID, nil
}

// legacyDo 发一个旧版接口的请求并解出应答。报错里不带请求地址：地址里有凭证和密钥。
func (b *Bot) legacyDo(req *http.Request, out any) error {
	resp, err := b.http().Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取应答失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, out)
}

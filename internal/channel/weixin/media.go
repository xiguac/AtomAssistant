package weixin

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/media"
)

// 发图片、视频和文件。做法照官方客户端（@tencent-weixin/openclaw-weixin 2.4.10 的 cdn/upload.js、
// messaging/send.js）：先向 getuploadurl 要上传地址，把文件用随机的 AES-128-ECB 密钥加密后传到 CDN，
// CDN 在应答头 x-encrypted-param 里给回下载参数，再在消息里引用它。
//
// 官方客户端没有发语音的实现，这里也不发：语音由中枢改成按文件发。

// 上传接口里各类的编号，取自官方客户端的 UploadMediaType。
const (
	uploadImage = 1
	uploadVideo = 2
	uploadFile  = 3
)

// 消息里各类的编号，取自官方客户端的 MessageItemType。
const (
	itemVoice = 3
	itemVideo = 5
)

// uploaded 是传到 CDN 之后的一个文件。
type uploaded struct {
	downloadParam string
	// aesKey 是十六进制写的密钥。消息里要的是"这串十六进制字符"的 Base64，和官方客户端一致。
	aesKey     string
	rawSize    int
	cipherSize int
}

// encryptECB 用 AES-128-ECB 加密，PKCS7 填充。
func encryptECB(plain, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("weixin: 密钥不对: %w", err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	data := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	return out, nil
}

// upload 把一个文件加密后传到微信的 CDN。
func (b *Bot) upload(ctx context.Context, token, user string, mediaType int, data []byte) (uploaded, error) {
	key := make([]byte, 16)
	fileKey := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return uploaded{}, fmt.Errorf("weixin: 生成密钥失败: %w", err)
	}
	if _, err := rand.Read(fileKey); err != nil {
		return uploaded{}, fmt.Errorf("weixin: 生成文件编号失败: %w", err)
	}
	cipher, err := encryptECB(data, key)
	if err != nil {
		return uploaded{}, err
	}
	sum := md5.Sum(data)
	hexKey, fk := hex.EncodeToString(key), hex.EncodeToString(fileKey)
	var resp struct {
		Ret           int    `json:"ret"`
		ErrCode       int    `json:"errcode"`
		ErrMsg        string `json:"errmsg"`
		UploadFullURL string `json:"upload_full_url"`
		UploadParam   string `json:"upload_param"`
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := post(callCtx, b.client(15*time.Second), b.base()+"/ilink/bot/getuploadurl", token, map[string]any{
		"filekey": fk, "media_type": mediaType, "to_user_id": user,
		"rawsize": len(data), "rawfilemd5": hex.EncodeToString(sum[:]), "filesize": len(cipher),
		"no_need_thumb": true, "aeskey": hexKey, "base_info": baseInfo(),
	}, &resp); err != nil {
		return uploaded{}, fmt.Errorf("weixin: 申请上传地址失败: %w", err)
	}
	if resp.Ret == staleToken || resp.ErrCode == staleToken {
		return uploaded{}, ErrLoggedOut
	}
	target := strings.TrimSpace(resp.UploadFullURL)
	if target == "" {
		if resp.UploadParam == "" {
			return uploaded{}, fmt.Errorf("weixin: 申请上传地址失败: ret=%d %s", resp.Ret, resp.ErrMsg)
		}
		target = b.cdn() + "/upload?encrypted_query_param=" + url.QueryEscape(resp.UploadParam) + "&filekey=" + url.QueryEscape(fk)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(cipher))
	if err != nil {
		return uploaded{}, fmt.Errorf("weixin: 上传地址不合法: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	up, err := b.client(2 * time.Minute).Do(req)
	if err != nil {
		return uploaded{}, fmt.Errorf("weixin: 上传到 CDN 失败: %w", err)
	}
	defer up.Body.Close()
	if up.StatusCode != http.StatusOK {
		return uploaded{}, fmt.Errorf("weixin: 上传到 CDN 失败: %d %s", up.StatusCode, up.Header.Get("x-error-message"))
	}
	param := up.Header.Get("x-encrypted-param")
	if param == "" {
		return uploaded{}, fmt.Errorf("weixin: CDN 的应答里没有下载参数")
	}
	return uploaded{downloadParam: param, aesKey: hexKey, rawSize: len(data), cipherSize: len(cipher)}, nil
}

// mediaItem 把要发的文件传上去，返回消息里的那一项。
func (b *Bot) mediaItem(ctx context.Context, token, user string, f *channel.File) (map[string]any, error) {
	ref := func(u uploaded) map[string]any {
		return map[string]any{
			"encrypt_query_param": u.downloadParam,
			"aes_key":             base64.StdEncoding.EncodeToString([]byte(u.aesKey)),
			"encrypt_type":        1,
		}
	}
	switch f.Kind {
	case media.KindAudio:
		return nil, channel.ErrUnsupportedKind
	case media.KindImage:
		u, err := b.upload(ctx, token, user, uploadImage, f.Data)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": itemImage, "image_item": map[string]any{"media": ref(u), "mid_size": u.cipherSize}}, nil
	case media.KindVideo:
		if media.Sniff(f.Data) != media.MIMEMP4 {
			return nil, channel.ErrUnsupportedKind
		}
		u, err := b.upload(ctx, token, user, uploadVideo, f.Data)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": itemVideo, "video_item": map[string]any{"media": ref(u), "video_size": u.cipherSize}}, nil
	}
	u, err := b.upload(ctx, token, user, uploadFile, f.Data)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": itemFile, "file_item": map[string]any{
		"media": ref(u), "file_name": f.Name, "len": strconv.Itoa(u.rawSize),
	}}, nil
}

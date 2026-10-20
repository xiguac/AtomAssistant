package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ASR 是离线语音识别服务的客户端：把一段语音转成文字。
//
// 服务是 asr-1.0 里的 rt.webdemo，和程序装在同一台机器上（见 deploy/asr.sh）。
// 它的接口：POST /api/asr，请求体是 WAV，返回 JSON，文字在 text 里，出错时在 error 里。
// 模型前端固定 16k，所以先在这边解成 16k 单声道再送过去：silk 自己解，其余的交给 ffmpeg。
type ASR struct {
	// URL 是服务地址，如 http://127.0.0.1:8765。
	URL string
	// Codec 为空用 Default。
	Codec *Codec
}

// asrRate 是识别模型要的采样率。
const asrRate = 16000

// Transcribe 把一段语音转成文字。听不出内容（静音、噪音）时返回空串。
func (a *ASR) Transcribe(ctx context.Context, audio []byte) (string, error) {
	c := a.Codec
	if c == nil {
		c = Default
	}
	pcm, err := c.PCM(ctx, audio, asrRate)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.URL, "/")+"/api/asr", bytes.NewReader(WAV(pcm, asrRate)))
	if err != nil {
		return "", fmt.Errorf("media: 语音识别请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", MIMEWAV)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("media: 连不上语音识别服务: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("media: 读语音识别结果失败: %w", err)
	}
	var out struct {
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("media: 语音识别服务返回的不是 JSON（HTTP %d）: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("media: 语音识别失败（HTTP %d）: %s", resp.StatusCode, strings.TrimSpace(out.Error))
	}
	return strings.TrimSpace(out.Text), nil
}

package llm

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// Image 是随消息一起发给模型的一段媒体内容：图片、语音或视频。只有支持对应输入的模型看得懂。
// 名字沿用最早只有图片时的叫法。
type Image struct {
	// MIME 是内容的类型，如 image/jpeg、audio/wav、video/mp4。
	MIME string
	Data []byte
}

// audioFormat 把语音的 MIME 类型换成模型服务要的 format 写法。
func audioFormat(mime string) string {
	switch mime {
	case "audio/mpeg":
		return "mp3"
	case "audio/amr":
		return "amr"
	}
	// 用户发来的语音交给模型之前都已转成 wav、mp3 或 amr，见 media.ForModel。
	return "wav"
}

// openAIMessages 把消息转成 OpenAI Chat Completions 的格式。
// 没带媒体的消息保持纯文本；带了的按多模态的写法，内容是文字和媒体组成的数组。
//
// 语音按 input_audio 给：阿里云百炼要求数据写成 data:;base64,… 的形式，OpenAI 要求直接是 Base64，
// 按服务的写法区分。视频按 video_url 给，数据写成 data:video/mp4;base64,…（阿里云百炼的写法；
// OpenAI 的接口不收视频）。
func openAIMessages(d Dialect, msgs []Message) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		if len(m.Images) == 0 && len(m.Audio) == 0 && len(m.Video) == 0 {
			out = append(out, m)
			continue
		}
		parts := []any{map[string]any{"type": "text", "text": m.Content}}
		for _, img := range m.Images {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{
				"url": "data:" + img.MIME + ";base64," + base64.StdEncoding.EncodeToString(img.Data),
			}})
		}
		for _, a := range m.Audio {
			data := base64.StdEncoding.EncodeToString(a.Data)
			if d == DialectBailian {
				data = "data:;base64," + data
			}
			parts = append(parts, map[string]any{"type": "input_audio", "input_audio": map[string]string{
				"data": data, "format": audioFormat(a.MIME),
			}})
		}
		for _, v := range m.Video {
			parts = append(parts, map[string]any{"type": "video_url", "video_url": map[string]string{
				"url": "data:" + v.MIME + ";base64," + base64.StdEncoding.EncodeToString(v.Data),
			}})
		}
		out = append(out, map[string]any{"role": m.Role, "content": parts})
	}
	return out
}

// anthropicMessage 把一条消息转成 Anthropic Messages 的格式，role 是已经换算好的角色。
// Anthropic 的接口不收语音和视频。
func anthropicMessage(role Role, m Message) (any, error) {
	if len(m.Audio) > 0 || len(m.Video) > 0 {
		var kinds []string
		if len(m.Audio) > 0 {
			kinds = append(kinds, "语音")
		}
		if len(m.Video) > 0 {
			kinds = append(kinds, "视频")
		}
		return nil, fmt.Errorf("llm: Anthropic 协议的接口不收%s，请在「模型」页取消这个模型的对应输入", strings.Join(kinds, "、"))
	}
	if len(m.Images) == 0 {
		return Message{Role: role, Content: m.Content}, nil
	}
	parts := make([]any, 0, len(m.Images)+1)
	for _, img := range m.Images {
		parts = append(parts, map[string]any{"type": "image", "source": map[string]string{
			"type": "base64", "media_type": img.MIME, "data": base64.StdEncoding.EncodeToString(img.Data),
		}})
	}
	parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	return map[string]any{"role": role, "content": parts}, nil
}

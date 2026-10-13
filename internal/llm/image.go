package llm

import "encoding/base64"

// Image 是随消息一起发给模型的一张图片。只有多模态模型看得懂。
type Image struct {
	// MIME 是图片的类型，如 image/jpeg。
	MIME string
	Data []byte
}

// openAIMessages 把消息转成 OpenAI Chat Completions 的格式。
// 没带图片的消息保持纯文本；带图片的按多模态的写法，内容是文字和图片组成的数组。
func openAIMessages(msgs []Message) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		if len(m.Images) == 0 {
			out = append(out, m)
			continue
		}
		parts := []any{map[string]any{"type": "text", "text": m.Content}}
		for _, img := range m.Images {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{
				"url": "data:" + img.MIME + ";base64," + base64.StdEncoding.EncodeToString(img.Data),
			}})
		}
		out = append(out, map[string]any{"role": m.Role, "content": parts})
	}
	return out
}

// anthropicMessage 把一条消息转成 Anthropic Messages 的格式，role 是已经换算好的角色。
func anthropicMessage(role Role, m Message) any {
	if len(m.Images) == 0 {
		return Message{Role: role, Content: m.Content}
	}
	parts := make([]any, 0, len(m.Images)+1)
	for _, img := range m.Images {
		parts = append(parts, map[string]any{"type": "image", "source": map[string]string{
			"type": "base64", "media_type": img.MIME, "data": base64.StdEncoding.EncodeToString(img.Data),
		}})
	}
	parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	return map[string]any{"role": role, "content": parts}
}

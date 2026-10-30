package channel

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"github.com/xiguac/atomassistant/internal/media"
)

// 用户在聊天软件里发来的语音消息：转成文字，和打的字一样处理。
//
// 不把语音交给模型：模型听语音按音频计 token，比转好的文字贵得多。
// 只有消息源原生的语音消息才转；用户当作文件发来的音频（mp3、wav 等）按文件收，见 files.go。
// 转出来的文字前面加一句说明（voiceNote），让模型知道这是语音转的、可能有错别字。

// Transcriber 把一段语音转成文字，由 *media.ASR 实现。听不出内容时返回空串。
type Transcriber interface {
	Transcribe(ctx context.Context, audio []byte) (string, error)
}

// WithASR 让用户可以在聊天里发语音。不接入时语音消息会被告知收不了。
func (h *Hub) WithASR(t Transcriber) *Hub {
	h.asr = t
	return h
}

// voiceNote 加在语音转出来的文字前面，写给模型看。
const voiceNote = "（用户发送的是语音消息，下面是转文字的内容，可能有错别字或同音字，请结合上下文多加思考）\n"

// transcribe 取回一条消息里的语音并转成文字，几段之间换行。没转成的告诉用户，一段都没转成时返回空串。
func (h *Hub) transcribe(ctx context.Context, channel string, m Message) string {
	if h.asr == nil {
		h.say(ctx, channel, m.Chat, "还没有配置语音识别，收不了语音，请打字。")
		return ""
	}
	voices := m.Voices
	if len(voices) > maxVoices {
		h.say(ctx, channel, m.Chat, fmt.Sprintf("一次最多收 %d 段语音，这次只收下了前 %d 段。", maxVoices, maxVoices))
		voices = voices[:maxVoices]
	}
	var texts, problems []string
	for _, a := range voices {
		text, data, err := h.transcribeOne(ctx, a)
		switch {
		case err != nil:
			h.log.Error("语音转文字失败", "渠道", channel, "err", err)
			problems = append(problems, "有一段语音没转成文字，请重发或打字。")
		case text == "":
			// 记下这段语音的格式、时长和响度，查得出是录音本来没声音、格式解错了，还是识别的问题。不记内容。
			h.log.Warn("语音没听出内容", append([]any{"渠道", channel}, audioStats(ctx, data)...)...)
			problems = append(problems, "有一段语音没听出内容，请重说或打字。")
		default:
			texts = append(texts, text)
		}
	}
	if len(problems) > 0 {
		h.say(ctx, channel, m.Chat, strings.Join(problems, "\n"))
	}
	return strings.Join(texts, "\n")
}

func (h *Hub) transcribeOne(ctx context.Context, a Attachment) (string, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, mediaTimeout)
	defer cancel()
	var p Picture
	var err error
	if a.Fetch != nil {
		p, err = a.Fetch(ctx)
	} else {
		p, err = h.fetch(ctx, a)
	}
	if err != nil {
		return "", nil, fmt.Errorf("取回语音失败: %w", err)
	}
	text, err := h.asr.Transcribe(ctx, p.Data)
	return text, p.Data, err
}

// audioStats 是一段语音的格式、大小、时长和响度，写进日志用。
func audioStats(ctx context.Context, data []byte) []any {
	out := []any{"格式", media.Sniff(data), "字节数", len(data)}
	const rate = 16000
	pcm, err := media.Default.PCM(ctx, data, rate)
	if err != nil {
		return append(out, "解码", err.Error())
	}
	var sum float64
	peak := 0
	n := len(pcm) / 2
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(pcm[i:])))
		if v < 0 {
			v = -v
		}
		peak = max(peak, v)
		sum += float64(v) * float64(v)
	}
	rms := 0.0
	if n > 0 {
		rms = math.Sqrt(sum / float64(n))
	}
	// 满幅是 32767；说话一般 RMS 在几百到几千，低于 50 基本是静音。
	return append(out, "时长秒", math.Round(float64(n)/rate*10)/10, "响度RMS", int(rms), "峰值", peak)
}

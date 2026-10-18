package media

import (
	"fmt"

	silk "github.com/wdvxdr1123/go-silk"
)

// silkRate 是微信、QQ 语音用的采样率（微信官方客户端解码时用的也是 24000）。
const silkRate = 24000

// silkBitRate 是编码 silk 的码率，语音够用。
const silkBitRate = 24000

// decodeSilk 把 silk 解成 rate 采样率的 16 位单声道原始采样。
func decodeSilk(data []byte, rate int) ([]byte, error) {
	pcm, err := silk.DecodeSilkBuffToPcm(data, rate)
	if err != nil {
		return nil, fmt.Errorf("media: 解 silk 语音失败: %w", err)
	}
	return pcm, nil
}

// encodeSilk 把 16 位单声道原始采样编成腾讯格式的 silk。
func encodeSilk(pcm []byte, rate int) ([]byte, error) {
	out, err := silk.EncodePcmBuffToSilk(pcm, rate, silkBitRate, true)
	if err != nil {
		return nil, fmt.Errorf("media: 编 silk 语音失败: %w", err)
	}
	return out, nil
}

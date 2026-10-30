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

// silkEncodeTries 是编 silk 最多试几次，见 encodeSilk。
const silkEncodeTries = 5

// encodeSilk 把 16 位单声道原始采样编成腾讯格式的 silk。
//
// 用的编码库是从 C 机器翻译过来的，同样的输入每次编出来的结果不完全一样，
// 偶尔（实测约 2%）编出一段解不开的数据——发出去对方就放不出来。所以编完先自己解一遍，
// 解不开就重编。
func encodeSilk(pcm []byte, rate int) ([]byte, error) {
	var last error
	for range silkEncodeTries {
		out, err := silk.EncodePcmBuffToSilk(pcm, rate, silkBitRate, true)
		if err != nil {
			return nil, fmt.Errorf("media: 编 silk 语音失败: %w", err)
		}
		if _, last = silk.DecodeSilkBuffToPcm(out, rate); last == nil {
			return out, nil
		}
	}
	return nil, fmt.Errorf("media: 编出的 silk 语音连续 %d 次解不开: %w", silkEncodeTries, last)
}

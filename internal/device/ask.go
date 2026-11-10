package device

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// ReplyChannelSetting 是手机提问的答复发到哪个消息渠道，存在后台的设置里。
	ReplyChannelSetting = "device.reply_channel"
	// DefaultReplyChannel 是没设过时用的渠道名。
	DefaultReplyChannel = "qq"

	// MaxAskText 是一次提问最多多少字。
	MaxAskText = 4000
	// MaxAskImage 是随提问带的一张截屏最大多少字节（解码后）。
	MaxAskImage = 3 << 20
)

// ErrUnavailable 表示服务暂时用不了（比如消息渠道还没就绪）。
var ErrUnavailable = errors.New("device: 服务暂时不可用")

// AskImage 是随提问带的一张图（一般是截屏）。
type AskImage struct {
	MIME string
	Data []byte
}

// Asker 把手机上的提问交给助手，由 atomd 里接着消息渠道中枢的实现提供。
type Asker interface {
	Ask(ctx context.Context, text string, images []AskImage) error
}

// Ask 把用户在手机上说的话（可带一张截屏）交给助手。答复走后台设置的消息渠道发回给用户。
func (s *Service) Ask(ctx context.Context, text string, img *AskImage) error {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > MaxAskText {
		return fmt.Errorf("%w: 提问要在 1 到 %d 个字之间", ErrInvalid, MaxAskText)
	}
	var images []AskImage
	if img != nil {
		if img.MIME != "image/jpeg" && img.MIME != "image/png" {
			return fmt.Errorf("%w: 截屏只支持 jpeg 或 png", ErrInvalid)
		}
		if len(img.Data) == 0 || len(img.Data) > MaxAskImage {
			return fmt.Errorf("%w: 截屏是空的，或超过 %d MB", ErrInvalid, MaxAskImage>>20)
		}
		images = append(images, *img)
	}
	if s.Asker == nil {
		return fmt.Errorf("%w: 没有接入消息渠道", ErrUnavailable)
	}
	return s.Asker.Ask(ctx, text, images)
}

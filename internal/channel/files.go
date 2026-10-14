package channel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 用户在聊天里发来的文件：取回来放进工作区，再告诉助手放在了哪里。
//
// 文件本身不进对话：助手用文件工具去读它，读出来的内容和别的文件一样按工具输出对待。
// 进对话的只有一句"我发来了文件 xxx"——这句话是用户的意思，按用户说的话处理，
// 归给哪件事和别的话一样由 route 决定。

// FileSink 把用户发来的文件存起来，返回它在工作区里的路径，由文件工作区实现。
type FileSink interface {
	Receive(name string, data []byte) (path string, err error)
}

// WithFiles 让用户可以在聊天里发文件。不接入时文件消息会被告知暂时收不了。
func (h *Hub) WithFiles(f FileSink) *Hub {
	h.files = f
	return h
}

const (
	// maxIncomingFiles 是一条消息里最多收几个文件。
	maxIncomingFiles = 5
	// fileTimeout 是取回一个文件的最长时间。
	fileTimeout = 2 * time.Minute
)

// receiveFiles 取回并存下一条消息里的文件，返回要交给任务的那句话。全都没收成时返回错误。
func (h *Hub) receiveFiles(ctx context.Context, channel string, m Message) (string, error) {
	if h.files == nil {
		return "", fmt.Errorf("文件处理没有启用，收不了文件。可在管理后台的「配置」里加上 files 一节后重启")
	}
	files := m.Files
	if len(files) > maxIncomingFiles {
		h.say(ctx, channel, m.Chat, fmt.Sprintf("一次最多收 %d 个文件，这次只收下了前 %d 个。", maxIncomingFiles, maxIncomingFiles))
		files = files[:maxIncomingFiles]
	}
	var saved, failed []string
	for _, a := range files {
		fetchCtx, cancel := context.WithTimeout(ctx, fileTimeout)
		var data []byte
		var err error
		if a.Fetch != nil {
			var p Picture
			if p, err = a.Fetch(fetchCtx); err == nil {
				data = p.Data
			}
		} else {
			data, err = fetchFile(fetchCtx, a.URL)
		}
		cancel()
		name := orElse(a.Name, "未命名文件")
		if err == nil {
			var where string
			if where, err = h.files.Receive(name, data); err == nil {
				saved = append(saved, fmt.Sprintf("%s（%s）", where, sizeText(len(data))))
				continue
			}
		}
		h.log.Error("收文件失败", "渠道", channel, "文件", name, "err", err)
		failed = append(failed, fmt.Sprintf("%s：%v", name, err))
	}
	if len(failed) > 0 {
		h.say(ctx, channel, m.Chat, "有文件没收成：\n"+strings.Join(failed, "\n"))
	}
	if len(saved) == 0 {
		return "", fmt.Errorf("文件一个都没收成")
	}
	return "我发来了文件，已放进工作区：" + strings.Join(saved, "、"), nil
}

func sizeText(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d 字节", n)
}

// fetchFile 按平台给的地址取回一个文件。
func fetchFile(ctx context.Context, rawURL string) ([]byte, error) {
	if !strings.Contains(rawURL, "://") {
		// 有的平台给的地址不带协议。
		rawURL = "https://" + rawURL
	}
	if !strings.HasPrefix(rawURL, "https://") {
		return nil, fmt.Errorf("文件地址不是 https")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("文件地址不合法: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("文件服务器返回 %d", resp.StatusCode)
	}
	return ReadLimited(resp.Body)
}

// ReadLimited 读出一个文件，超过 MaxFileBytes 的不收。各消息源取文件时都用它。
func ReadLimited(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取文件失败: %w", err)
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("文件超过 %d MB，收不了", MaxFileBytes>>20)
	}
	return data, nil
}

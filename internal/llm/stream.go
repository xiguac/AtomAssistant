package llm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultIdleTimeout 是默认的首字超时：发出请求后这么久都没收到任何数据就放弃。
// 收到数据之后，两段数据之间也按这个时长算。
//
// 强模型做长任务时，光是想清楚再开口就可能要好几分钟，所以给得宽。
// 不设整次请求的总时限：回答一段段流回来，只要还在来就一直等。
const defaultIdleTimeout = 10 * time.Minute

// errStalled 是看门狗掐断请求时留下的原因，用来和调用方自己取消区分开。
var errStalled = errors.New("等待模型返回超时")

// watchdog 在 idle 时长内没有收到任何数据时取消请求。
type watchdog struct {
	idle   time.Duration
	timer  *time.Timer
	cancel context.CancelCauseFunc
}

// startWatchdog 返回一个会被看门狗取消的 ctx。用完必须调 stop。
func startWatchdog(ctx context.Context, idle time.Duration) (context.Context, *watchdog) {
	ctx, cancel := context.WithCancelCause(ctx)
	w := &watchdog{idle: idle, cancel: cancel}
	w.timer = time.AfterFunc(idle, func() { cancel(errStalled) })
	return ctx, w
}

func (w *watchdog) stop() {
	w.timer.Stop()
	w.cancel(nil)
}

// body 包一层响应体：每读到数据就把期限往后推。
func (w *watchdog) body(r io.Reader) io.Reader { return &idleReader{r: r, w: w} }

type idleReader struct {
	r io.Reader
	w *watchdog
}

func (b *idleReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		b.w.timer.Reset(b.w.idle)
	}
	return n, err
}

// explain 把看门狗造成的错误换成说得清楚的话；别的错误原样返回。
func (w *watchdog) explain(ctx context.Context, name string, err error) error {
	if errors.Is(context.Cause(ctx), errStalled) {
		return fmt.Errorf("%w: %s 超过 %s 没有返回任何内容", ErrUpstream, name, w.idle)
	}
	return err
}

// isEventStream 报告响应是不是流式的。出错时上游通常直接回一段 JSON。
func isEventStream(resp *http.Response) bool {
	return resp.StatusCode == http.StatusOK &&
		strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
}

// readEvents 逐条读出 SSE 里的 data 内容交给 fn，读到 [DONE] 为止。
func readEvents(r io.Reader, fn func(data []byte) error) error {
	sc := bufio.NewScanner(r)
	// 一条事件就是一行，联网搜索的结果块可能很长。
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // 空行、event: 行、: 开头的注释（心跳）
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			return nil
		}
		if err := fn([]byte(data)); err != nil {
			return err
		}
	}
	return sc.Err()
}

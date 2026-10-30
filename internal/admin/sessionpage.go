package admin

import (
	"net/http"
	"time"

	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/core"
	chat "github.com/xiguac/atomassistant/internal/session"
)

// summaryKeepDays 是会话摘要与原话能查看的天数，更早的不再提供查看。
const summaryKeepDays = 30

// sessionData 是一天的会话：那天的摘要、原话，以及当天交代的事。
type sessionData struct {
	Task *core.Task
	Day  string
	// Expired 为真表示已超过 summaryKeepDays 天，摘要和原话都不再显示。
	Expired  bool
	Summary  *chat.Summary
	Messages []chat.Message
	Children []*core.Task
}

// sessionPage 显示一天的会话任务。
func (s *Server) sessionPage(w http.ResponseWriter, r *http.Request, v *view, t *core.Task, day string) {
	ctx := r.Context()
	d := sessionData{Task: t, Day: day}
	var err error
	if d.Children, err = s.opts.Store.ChildTasks(ctx, t.ID, false); err != nil {
		v.Error = err.Error()
	}
	cutoff := chat.Day(time.Now().AddDate(0, 0, -summaryKeepDays))
	if day < cutoff {
		d.Expired = true
	} else {
		sums, err := s.opts.Store.Sessions().Summaries(ctx, day, day)
		if err != nil {
			v.Error = err.Error()
		} else if len(sums) > 0 {
			d.Summary = &sums[0]
		}
		if d.Messages, err = s.opts.Store.Sessions().Messages(ctx, day, 1000); err != nil {
			v.Error = err.Error()
		}
	}
	v.Data = d
	s.render(w, "session", v)
}

// isSession 报告一个任务是不是每天的会话任务。
func isSession(t *core.Task) bool {
	_, ok := channel.SessionDay(t.Tags)
	return ok
}

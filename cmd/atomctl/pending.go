package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/store/pg"
)

// showPending 把正在等用户确认的动作完整地打出来。
//
// 批准之前必须看得到要做的到底是什么：一封要发出去的信，光看"助手的理由"是不够的，
// 得看到收件人和正文本身。
func showPending(ctx context.Context, st *pg.Store, id string) error {
	list, err := st.ListSteps(ctx, id)
	if err != nil {
		return err
	}
	for i := len(list) - 1; i >= 0; i-- {
		s := list[i]
		if s.Status != core.StepAwaiting {
			continue
		}
		fmt.Printf("\n等你确认的动作: %s（%s）\n", s.ToolName, s.SideEffect)
		var params map[string]any
		if err := json.Unmarshal(s.Input, &params); err != nil {
			fmt.Printf("  %s\n", s.Input)
		} else {
			keys := make([]string, 0, len(params))
			for k := range params {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				v, ok := params[k].(string)
				if !ok {
					b, err := json.Marshal(params[k])
					if err != nil {
						return fmt.Errorf("展示参数 %s 失败: %w", k, err)
					}
					v = string(b)
				}
				// 多行的值（邮件正文）另起一行并缩进，原样展示。
				if strings.Contains(v, "\n") {
					fmt.Printf("  %s:\n    %s\n", k, strings.ReplaceAll(v, "\n", "\n    "))
				} else {
					fmt.Printf("  %s: %s\n", k, v)
				}
			}
		}
		fmt.Printf("批准: atomctl approve %s    拒绝: atomctl reject %s [理由]\n", id, id)
		return nil
	}
	return nil
}

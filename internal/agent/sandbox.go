package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xiguac/atomassistant/internal/core"
	"github.com/xiguac/atomassistant/internal/sandbox"
)

// SandboxService 在隔离的环境里跑代码，由 *sandbox.Runner 提供。
type SandboxService interface {
	Run(ctx context.Context, code string) (sandbox.Result, error)
}

// ToolSandboxRun 在沙箱里跑一段 Python 代码。
const ToolSandboxRun = "sandbox.run"

var sandboxTool = builtin(ToolSandboxRun,
	`在一个隔离的沙箱里跑一段 Python 3 代码，用来处理工作区里的文件：批量改表格、合并拆分文档、统计分析、画图、转格式等`+
		`现有文件工具做不了或者做起来太繁琐的事。`+
		`沙箱里：工作区整个只读地挂在 /work（路径和工作区里的一样，如 /work/收到的文件/报价单.xlsx）；`+
		`当前目录是 /out，把要留下的文件写在这里，跑完会收进工作区的「沙箱输出/<时间>/」目录；`+
		`没有网络，装不了包；可用的库有 openpyxl、python-docx、python-pptx、pypdf、pandas、numpy、matplotlib、Pillow 和标准库；`+
		`有时间和内存上限。要告诉你的结果用 print 打出来（只保留前 32 KB）。`+
		`出错时看 stderr，改了再跑。不要用它做网络请求或者去读 /work 以外的东西，那些都做不到。`,
	`{"type":"object","properties":{"code":{"type":"string","description":"完整的 Python 3 代码"}},"required":["code"]}`,
	// 它只往工作区里一个新目录写文件，原来的文件动不了。
	core.SideEffectReversible)

// runSandbox 在沙箱里跑代码。
func (a *Actuator) runSandbox(ctx context.Context, s *core.Step) (json.RawMessage, error) {
	if a.sandbox == nil {
		return nil, fmt.Errorf("agent: 未启用沙箱")
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(s.Input, &in); err != nil {
		return nil, fmt.Errorf("agent: 解析沙箱参数失败: %w", err)
	}
	res, err := a.sandbox.Run(ctx, in.Code)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

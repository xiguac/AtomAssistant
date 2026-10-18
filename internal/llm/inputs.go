package llm

import "fmt"

// Input 是模型能接收的一类输入内容。
type Input string

const (
	InputText  Input = "text"
	InputImage Input = "image"
	InputAudio Input = "audio"
	InputVideo Input = "video"
)

// Inputs 列出全部输入类型，按后台里显示的先后。
var Inputs = []Input{InputText, InputImage, InputAudio, InputVideo}

// InputName 是输入类型给人看的名字。
func InputName(in Input) string {
	switch in {
	case InputText:
		return "文字"
	case InputImage:
		return "图片"
	case InputAudio:
		return "语音"
	case InputVideo:
		return "视频"
	}
	return string(in)
}

// DefaultInputs 是没有写明时认为模型支持的输入。
// 早先没有这项设置时，用户发来的图片一律原样交给模型，没写的照旧按支持图片算。
var DefaultInputs = []Input{InputText, InputImage}

// ParseInputs 校验配置里写的输入类型。文字是必须的：助手的指示和回答都是文字。
func ParseInputs(list []string) ([]Input, error) {
	if len(list) == 0 {
		return DefaultInputs, nil
	}
	out := make([]Input, 0, len(list))
	text := false
	for _, s := range list {
		in := Input(s)
		switch in {
		case InputText:
			text = true
		case InputImage, InputAudio, InputVideo:
		default:
			return nil, fmt.Errorf("llm: 输入类型只能是 text / image / audio / video，实际 %q", s)
		}
		out = append(out, in)
	}
	if !text {
		return nil, fmt.Errorf("llm: 输入类型必须包含 text")
	}
	return out, nil
}

// Accepts 报告一组输入类型里有没有 in。
func Accepts(inputs []Input, in Input) bool {
	for _, x := range inputs {
		if x == in {
			return true
		}
	}
	return false
}

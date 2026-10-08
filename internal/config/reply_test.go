package config

import "testing"

func TestLoadFile_回复风格与模型单价(t *testing.T) {
	f, err := LoadFile(writeConfig(t, validConfig+`
reply:
  style: balanced
  custom: 结论放第一句
fallback:
  provider: deepseek
  model: deepseek-chat
  input_yuan_per_mtok: 2
  output_yuan_per_mtok: 8
`))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if f.Reply.Style != "balanced" || f.Reply.Custom != "结论放第一句" {
		t.Fatalf("回复风格未读入: %+v", f.Reply)
	}
	if f.Fallback.InputYuanPerMTok != 2 || f.Fallback.OutputYuanPerMTok != 8 {
		t.Fatalf("单价未读入: %+v", f.Fallback)
	}

	// 不写 reply 是合法的，按简洁处理。
	if f, err := LoadFile(writeConfig(t, validConfig)); err != nil || f.Reply.Style != "" {
		t.Fatalf("不写 reply 应合法: %v", err)
	}
}

func TestLoadFile_非法的回复风格与负单价被拒(t *testing.T) {
	if _, err := LoadFile(writeConfig(t, validConfig+"\nreply:\n  style: 啰嗦\n")); err == nil {
		t.Fatal("未知的回复风格应报错，不能静默回退")
	}
	if _, err := LoadFile(writeConfig(t, validConfig+
		"\nfallback:\n  provider: deepseek\n  model: m\n  input_yuan_per_mtok: -1\n")); err == nil {
		t.Fatal("负单价应报错")
	}
}

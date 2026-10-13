package admin

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiguac/atomhelper/internal/agent"
	"github.com/xiguac/atomhelper/internal/skill"
)

// 「Skill」页：查看、安装、移除 Skill。
//
// 在这里安装和用 atomctl skill install 是同一回事：先过扫描，扫描不通过或没跑成一律不装。
// 装上的 Skill 不带来任何新权限——按它执行的任务只能调它声明过、且用户授权过的工具。

type skillsData struct {
	Skills []*skill.Skill
}

func (s *Server) skillsPage(w http.ResponseWriter, r *http.Request, v *view) {
	list, err := s.opts.Store.Skills().ListSkills(r.Context())
	if err != nil {
		v.Error = err.Error()
	}
	v.Data = skillsData{Skills: list}
	s.render(w, "skills", v)
}

// skillInstall 扫描并安装页面上贴进来的一份清单。
func (s *Server) skillInstall(w http.ResponseWriter, r *http.Request, _ *view) {
	text := strings.ReplaceAll(r.PostFormValue("manifest"), "\r\n", "\n")
	var installed *skill.Skill
	err := func() error {
		if strings.TrimSpace(text) == "" {
			return errors.New("请填写清单内容")
		}
		if len(text) > skill.MaxManifestBytes {
			return errors.New("清单内容超出长度限制")
		}
		// 扫描器按目录扫，所以先把清单落到一个临时目录里。
		dir, err := os.MkdirTemp("", "atom-skill-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		if err := os.WriteFile(filepath.Join(dir, skill.ManifestName), []byte(text), 0o600); err != nil {
			return err
		}
		// 扫描可能要几分钟。
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
		defer cancel()
		sk, err := skill.Load(ctx, dir, s.opts.Scanner)
		if err != nil {
			if sk != nil && sk.ScanReport != "" {
				return errors.New(err.Error() + "\n\n扫描报告：\n" + sk.ScanReport)
			}
			return err
		}
		installed = sk
		return skill.Install(r.Context(), s.opts.Store.Skills(), sk)
	}()
	notice := ""
	if err == nil {
		s.opts.Log.Info("Skill 已在后台安装", "skill", installed.ID, "来自", r.RemoteAddr)
		notice = "Skill「" + installed.Name + "」已通过扫描并安装。"
	}
	back(w, r, "/skills", notice, err)
}

func (s *Server) skillDelete(w http.ResponseWriter, r *http.Request, _ *view) {
	id := r.PostFormValue("id")
	err := s.opts.Store.Skills().RemoveSkill(r.Context(), id)
	if err == nil {
		s.opts.Log.Info("Skill 已在后台移除", "skill", id, "来自", r.RemoteAddr)
	}
	back(w, r, "/skills", "Skill「"+id+"」已移除。", err)
}

// 「系统提示词」页：查看、改写助手每一步决策都带着的两段系统提示。
//
// 改写的内容存在数据库的设置里，助手每次决策时现读，保存后立即生效；没有改写过的那一段用内置的。

// promptView 是页面上的一段系统提示。
type promptView struct {
	// Key 是它在设置里的名字，Title 与 Help 是给人看的说明。
	Key, Title, Help string
	Text             string
	// Custom 为真表示当前生效的是用户改写过的内容。
	Custom bool
}

// prompts 列出可以改写的各段系统提示与它们的内置内容。
func prompts() []struct {
	promptView
	def string
} {
	return []struct {
		promptView
		def string
	}{
		{promptView{Key: agent.PromptKeyBase, Title: "基础约束",
			Help: "位于系统提示最前面，适用于助手的每一步决策，优先级高于其余说明。"}, agent.DefaultBaseRules()},
		{promptView{Key: agent.PromptKeyPlanner, Title: "规划说明",
			Help: "规定助手每一步的输出格式与决策方法。其中的 JSON 输出格式由程序解析，改动字段名将导致助手无法运行。"}, agent.DefaultPlannerPrompt()},
	}
}

func (s *Server) promptsPage(w http.ResponseWriter, r *http.Request, v *view) {
	var list []promptView
	for _, p := range prompts() {
		saved, err := s.opts.Store.Setting(r.Context(), p.Key)
		if err != nil {
			v.Error = err.Error()
		}
		p.Text = p.def
		if strings.TrimSpace(saved) != "" {
			p.Text, p.Custom = saved, true
		}
		list = append(list, p.promptView)
	}
	v.Data = list
	s.render(w, "prompts", v)
}

// promptSave 保存一段系统提示。reset 非空表示恢复成内置的内容。
func (s *Server) promptSave(w http.ResponseWriter, r *http.Request, _ *view) {
	key := r.PostFormValue("key")
	text := strings.TrimSpace(strings.ReplaceAll(r.PostFormValue("text"), "\r\n", "\n"))
	reset := r.PostFormValue("reset") != ""
	title := ""
	err := func() error {
		for _, p := range prompts() {
			if p.Key != key {
				continue
			}
			title = p.Title
			if reset || text == strings.TrimSpace(p.def) {
				// 与内置内容一致就不存：以后内置内容更新时能跟着走。
				text = ""
			} else if text == "" {
				return errors.New("内容不能为空。如需使用内置内容，请点击「恢复默认」")
			}
			return s.opts.Store.PutSetting(r.Context(), key, text)
		}
		return errors.New("系统提示词类型无效")
	}()
	if err == nil {
		s.opts.Log.Info("系统提示词已在后台修改", "段落", title, "恢复默认", text == "", "来自", r.RemoteAddr)
	}
	notice := "「" + title + "」已保存，立即生效。"
	if text == "" {
		notice = "「" + title + "」已恢复为内置内容，立即生效。"
	}
	back(w, r, "/prompts", notice, err)
}

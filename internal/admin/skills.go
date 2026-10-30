package admin

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/skill"
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

// skillAsk 把用户一句话说的需求交给助手：建一个任务，由助手查找、扫描并安装合适的 Skill。
// 查找、扫描、高风险时请用户批准，都和助手在任务中自己缺能力时走同一套流程（见 agent 的 capability.*）。
func (s *Server) skillAsk(w http.ResponseWriter, r *http.Request, _ *view) {
	want := strings.TrimSpace(r.PostFormValue("want"))
	if want == "" {
		back(w, r, "/skills", "", errors.New("请说明需要什么样的 Skill"))
		return
	}
	if rs := []rune(want); len(rs) > 500 {
		back(w, r, "/skills", "", errors.New("需求描述请控制在 500 字以内"))
		return
	}
	title := "安装 Skill：" + want
	if rs := []rune(title); len(rs) > 24 {
		title = string(rs[:24]) + "…"
	}
	goal := "用户在管理后台要求安装 Skill，需求原话：" + want + "\n\n" +
		"请用 capability.search（kind 填 skill）查找合适的 Skill，选出最符合需求的一个，用 capability.install 安装。" +
		"安装前的安全扫描不通过就换一个或如实告诉用户；返回 needs_approval 时说明风险点，由用户决定。" +
		"装好后告诉用户装了哪个 Skill、它能做什么。找不到合适的就直说，不要自己编写。"
	id, err := s.opts.Tasks.Create(r.Context(), title, goal, nil)
	if err != nil {
		back(w, r, "/skills", "", err)
		return
	}
	http.Redirect(w, r, "/tasks/"+id, http.StatusSeeOther)
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

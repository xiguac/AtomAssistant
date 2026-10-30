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

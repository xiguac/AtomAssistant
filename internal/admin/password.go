package admin

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// 修改登录密码。
//
// 启动时的密码摘要来自环境变量（ATOM_ADMIN_PASSWORD_HASH），那个文件服务账号只能读、不能写。
// 所以在后台改的密码，摘要另存在配置文件旁边的一个文件里，有它就以它为准。
// 忘了密码时的退路仍然是环境变量：删掉这个文件（deploy.sh passwd 会顺带删），环境变量里的那个重新生效。

// passwordFileName 是保存密码摘要的文件名，与配置文件在同一个目录。
const passwordFileName = "admin-password.hash"

// passwordFile 返回保存密码摘要的文件路径；没有配置文件路径时返回空。
func (s *Server) passwordFile() string {
	if s.opts.ConfigPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.opts.ConfigPath), passwordFileName)
}

// loadPassword 定出当前生效的密码摘要：后台改过的优先，否则用环境变量里的。
func (s *Server) loadPassword() error {
	s.hash = s.opts.PasswordHash
	path := s.passwordFile()
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("admin: 读取密码文件失败: %w", err)
	}
	hash := strings.TrimSpace(string(raw))
	// 文件坏了就明说，不悄悄退回环境变量里的旧密码——那等于让一个已经作废的密码重新生效。
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		return fmt.Errorf("admin: %s 的内容不是有效的密码摘要。删除该文件后将使用 ATOM_ADMIN_PASSWORD_HASH", path)
	}
	s.hash = hash
	return nil
}

// passwordPage 是旧的地址：修改密码现在在「登录与安全」页里。
func (s *Server) passwordPage(w http.ResponseWriter, r *http.Request, _ *view) {
	http.Redirect(w, r, "/security", http.StatusSeeOther)
}

// passwordChange 修改登录密码。要先验证当前密码：会话被人拿到了，也不能凭它直接把密码换掉。
func (s *Server) passwordChange(w http.ResponseWriter, r *http.Request, _ *view) {
	current := r.PostFormValue("current")
	next := r.PostFormValue("new")
	err := func() error {
		s.mu.Lock()
		locked := s.failures >= maxFailures && time.Since(s.lockedAt) < lockFor
		hash := s.hash
		s.mu.Unlock()
		if locked {
			return errors.New("密码连续输入错误次数过多，请稍后重试")
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
			// 这里输错和登录时输错算在一起，免得被拿来无限次地试密码。
			s.mu.Lock()
			s.failures++
			s.lockedAt = time.Now()
			s.mu.Unlock()
			s.opts.Log.Warn("修改密码时当前密码错误", "来自", r.RemoteAddr)
			return errors.New("当前密码错误")
		}
		if next != r.PostFormValue("confirm") {
			return errors.New("两次输入的新密码不一致")
		}
		if next == current {
			return errors.New("新密码不能与当前密码相同")
		}
		fresh, err := HashPassword(next)
		if err != nil {
			return errors.New("新密码至少需要 10 个字符")
		}
		path := s.passwordFile()
		if path == "" {
			return errors.New("未设置 ATOM_CONFIG，无法保存新密码")
		}
		// 先写到旁边再改名：写到一半断电，原来的密码还在。
		tmp, err := os.CreateTemp(filepath.Dir(path), ".admin-password-*")
		if err != nil {
			return fmt.Errorf("无法写入配置目录: %w", err)
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.WriteString(fresh + "\n"); err != nil {
			_ = tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := os.Chmod(tmp.Name(), 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			return fmt.Errorf("保存新密码失败: %w", err)
		}
		// 新密码立即生效；除了正在用的这一个，别处已登录的会话全部作废。
		keep := ""
		if c, err := r.Cookie(cookieName); err == nil {
			keep = c.Value
		}
		s.mu.Lock()
		s.hash = fresh
		s.failures = 0
		for id := range s.sessions {
			if id != keep {
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
		s.opts.Log.Warn("后台登录密码已修改", "来自", r.RemoteAddr)
		return nil
	}()
	back(w, r, "/security", "登录密码已修改，立即生效。其他已登录的会话已全部退出。", err)
}

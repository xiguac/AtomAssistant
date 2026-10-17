package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/xiguac/atomhelper/internal/agent"
	"github.com/xiguac/atomhelper/internal/core"
)

// 「系统提示词」页：逐段查看、改写系统提示，并决定每一段是否加载。
//
// 系统提示不是一整篇，而是按场合拼起来的若干段（见 agent.PromptSpecs）。这一页把它们列出来：
// 每一段什么时候用、现在是内置的还是改写过的、是否加载。改动存在数据库的设置里，
// 助手每次决策时现读，保存后立即生效；没有改写过的那一段用内置的。
// 规定了输出格式的几段不能停用——程序要按那个格式解析模型的回答。
// 内置段落之外，用户还可以自己添加段落（agent.ExtraPrompt）：每次决策都加载，紧跟在基础约束后面。

// promptView 是页面上的一段系统提示。
type promptView struct {
	agent.PromptSpec
	// Text 是当前生效的内容；Custom 为真表示它是用户改写过的。
	Text   string
	Custom bool
	// On 为真表示这一段会被加载。
	On bool
}

func (s *Server) promptsPage(w http.ResponseWriter, r *http.Request, v *view) {
	saved, err := s.opts.Store.SettingsWithPrefix(r.Context(), "prompt.")
	if err != nil {
		v.Error = err.Error()
	}
	var list []promptView
	for _, spec := range agent.PromptSpecs() {
		p := promptView{PromptSpec: spec, Text: spec.Default,
			On: spec.Required || saved[agent.PromptOffKey(spec.Key)] == ""}
		if custom := strings.TrimSpace(saved[agent.PromptSettingKey(spec.Key)]); custom != "" {
			p.Text, p.Custom = custom, true
		}
		list = append(list, p)
	}
	extras, err := agent.ParseExtraPrompts(saved[agent.PromptExtraKey])
	if err != nil {
		v.Error = err.Error()
	}
	v.Data = promptsData{Specs: list, Extras: extras}
	s.render(w, "prompts", v)
}

// promptsData 是「系统提示词」页的内容：内置的各段，和用户自己添加的段落。
type promptsData struct {
	Specs  []promptView
	Extras []agent.ExtraPrompt
}

// promptSpec 按标识找出一段。
func promptSpec(key string) (agent.PromptSpec, bool) {
	for _, spec := range agent.PromptSpecs() {
		if spec.Key == key {
			return spec, true
		}
	}
	return agent.PromptSpec{}, false
}

// promptSave 保存一段系统提示。reset 非空表示恢复成内置的内容。
func (s *Server) promptSave(w http.ResponseWriter, r *http.Request, _ *view) {
	key := r.PostFormValue("key")
	text := strings.TrimSpace(strings.ReplaceAll(r.PostFormValue("text"), "\r\n", "\n"))
	reset := r.PostFormValue("reset") != ""
	spec, ok := promptSpec(key)
	err := func() error {
		if !ok {
			return errors.New("系统提示词类型无效")
		}
		if reset || text == strings.TrimSpace(spec.Default) {
			// 与内置内容一致就不存：以后内置内容更新时能跟着走。
			text = ""
		} else if text == "" {
			return errors.New("内容不能为空。如需使用内置内容，请点击「恢复默认」；如需停用，请关闭加载开关")
		}
		return s.opts.Store.PutSetting(r.Context(), agent.PromptSettingKey(key), text)
	}()
	if err == nil {
		s.opts.Log.Info("系统提示词已在后台修改", "段落", spec.Title, "恢复默认", text == "", "来自", r.RemoteAddr)
	}
	notice := "「" + spec.Title + "」已保存，立即生效。"
	if text == "" {
		notice = "「" + spec.Title + "」已恢复为内置内容，立即生效。"
	}
	back(w, r, "/prompts", notice, err)
}

// promptToggle 设置一段系统提示是否加载。
func (s *Server) promptToggle(w http.ResponseWriter, r *http.Request, _ *view) {
	key := r.PostFormValue("key")
	on := r.PostFormValue("on") != ""
	spec, ok := promptSpec(key)
	err := func() error {
		if !ok {
			return errors.New("系统提示词类型无效")
		}
		if spec.Required && !on {
			return errors.New("「" + spec.Title + "」规定了程序要解析的输出格式，不可停用")
		}
		off := "1"
		if on {
			off = ""
		}
		return s.opts.Store.PutSetting(r.Context(), agent.PromptOffKey(key), off)
	}()
	if err == nil {
		s.opts.Log.Info("系统提示词的加载设置已在后台修改", "段落", spec.Title, "加载", on, "来自", r.RemoteAddr)
	}
	notice := "「" + spec.Title + "」已停用，不再加载，立即生效。"
	if on {
		notice = "「" + spec.Title + "」已启用，立即生效。"
	}
	back(w, r, "/prompts", notice, err)
}

// loadExtras 读出用户自己添加的段落。
func (s *Server) loadExtras(ctx context.Context) ([]agent.ExtraPrompt, error) {
	raw, err := s.opts.Store.Setting(ctx, agent.PromptExtraKey)
	if err != nil {
		return nil, err
	}
	return agent.ParseExtraPrompts(raw)
}

// saveExtras 整份写回用户自己添加的段落。
func (s *Server) saveExtras(ctx context.Context, list []agent.ExtraPrompt) error {
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return s.opts.Store.PutSetting(ctx, agent.PromptExtraKey, string(raw))
}

// editExtras 读出自定义段落，交给 change 修改后写回。change 返回这次操作的结果说明。
func (s *Server) editExtras(w http.ResponseWriter, r *http.Request, change func([]agent.ExtraPrompt) ([]agent.ExtraPrompt, string, error)) {
	notice, err := func() (string, error) {
		list, err := s.loadExtras(r.Context())
		if err != nil {
			return "", err
		}
		list, notice, err := change(list)
		if err != nil {
			return "", err
		}
		return notice, s.saveExtras(r.Context(), list)
	}()
	if err == nil {
		s.opts.Log.Info("自定义系统提示段落已在后台修改", "结果", notice, "来自", r.RemoteAddr)
	}
	back(w, r, "/prompts", notice, err)
}

// findExtra 返回 id 对应的段落下标，没有则为 -1。
func findExtra(list []agent.ExtraPrompt, id string) int {
	for i, e := range list {
		if e.ID == id {
			return i
		}
	}
	return -1
}

// extraSave 添加一段（id 为空时）或修改已有的一段。
func (s *Server) extraSave(w http.ResponseWriter, r *http.Request, _ *view) {
	id := r.PostFormValue("id")
	title := strings.TrimSpace(r.PostFormValue("title"))
	text := strings.TrimSpace(strings.ReplaceAll(r.PostFormValue("text"), "\r\n", "\n"))
	s.editExtras(w, r, func(list []agent.ExtraPrompt) ([]agent.ExtraPrompt, string, error) {
		if title == "" || text == "" {
			return nil, "", errors.New("标题和内容都不能为空")
		}
		if id == "" {
			list = append(list, agent.ExtraPrompt{ID: core.NewID(), Title: title, Text: text})
			return list, "「" + title + "」已添加，立即生效。", nil
		}
		i := findExtra(list, id)
		if i < 0 {
			return nil, "", errors.New("要修改的段落不存在，可能已被删除")
		}
		list[i].Title, list[i].Text = title, text
		return list, "「" + title + "」已保存，立即生效。", nil
	})
}

// extraToggle 设置一段自定义段落是否加载。
func (s *Server) extraToggle(w http.ResponseWriter, r *http.Request, _ *view) {
	id, on := r.PostFormValue("id"), r.PostFormValue("on") != ""
	s.editExtras(w, r, func(list []agent.ExtraPrompt) ([]agent.ExtraPrompt, string, error) {
		i := findExtra(list, id)
		if i < 0 {
			return nil, "", errors.New("段落不存在，可能已被删除")
		}
		list[i].Off = !on
		if on {
			return list, "「" + list[i].Title + "」已启用，立即生效。", nil
		}
		return list, "「" + list[i].Title + "」已停用，不再加载，立即生效。", nil
	})
}

// extraDelete 删除一段自定义段落。
func (s *Server) extraDelete(w http.ResponseWriter, r *http.Request, _ *view) {
	id := r.PostFormValue("id")
	s.editExtras(w, r, func(list []agent.ExtraPrompt) ([]agent.ExtraPrompt, string, error) {
		i := findExtra(list, id)
		if i < 0 {
			return nil, "", errors.New("段落不存在，可能已被删除")
		}
		title := list[i].Title
		return append(list[:i], list[i+1:]...), "「" + title + "」已删除，立即生效。", nil
	})
}

package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/xiguac/atomassistant/internal/skillscan"
)

// ManifestName 是 Skill 目录里清单文件的名字。
const ManifestName = "skill.yaml"

// Checker 是安装前的扫描闸门，由 skillscan.Scanner 实现。
type Checker interface {
	Check(ctx context.Context, path string) (skillscan.Report, error)
}

// Load 扫描并解析 path 处的 Skill，但不安装。path 可以是清单文件，也可以是含 skill.yaml 的目录。
//
// 顺序是先扫描、后解析：扫描的对象是整个目录而不只是清单，
// 目录里夹带的其他文件同样要过扫描；而且在确认它不是恶意内容之前，
// 我们自己也不去解析它。
//
// 扫描不通过或没跑成时返回错误，同时把扫描器的输出放在返回的 Skill.ScanReport 里
// （此时 Skill 的其余字段为空），供调用方展示给用户。
func Load(ctx context.Context, path string, scan Checker) (*Skill, error) {
	if scan == nil {
		// 没有扫描器不等于可以跳过扫描。
		return nil, fmt.Errorf("%w: 没有配置扫描器", skillscan.ErrScanFailed)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("skill: 读取 %s 失败: %w", path, err)
	}
	manifest := path
	if info.IsDir() {
		manifest = filepath.Join(path, ManifestName)
	}

	rep, err := scan.Check(ctx, path)
	if err != nil {
		return &Skill{ScanReport: rep.Output}, err
	}

	raw, err := os.ReadFile(manifest)
	if err != nil {
		return nil, fmt.Errorf("skill: 读取清单 %s 失败: %w", manifest, err)
	}
	s, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	s.ScanReport = rep.Output
	return s, nil
}

// Install 把一个已经通过 Load 的 Skill 存入。同 ID 的旧版本被替换。
//
// Load 与 Install 分成两步，中间留给调用方做一件不能省的事：
// 把清单全文和它声明的工具展示给用户，由用户决定装不装。
func Install(ctx context.Context, store Store, s *Skill) error {
	if err := s.Validate(); err != nil {
		return err
	}
	s.InstalledAt = time.Now().UTC()
	return store.PutSkill(ctx, s)
}

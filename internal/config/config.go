// Package config 从环境变量读取运行配置。
//
// 自部署形态下用户要自己改配置，所以每一项都有合理默认值，
// 只有 DSN 是必填的。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xiguac/atomhelper/internal/seal"
)

// Config 是 atomd 的运行配置。
type Config struct {
	// DSN 是 PostgreSQL 连接串，必填。
	DSN string
	// Owner 写入执行租约，同一台机器重启后应保持稳定。
	Owner string

	Workers          int
	PollInterval     time.Duration
	LeaseDuration    time.Duration
	MaxStepsPerClaim int

	// LogLevel: debug / info / warn / error
	LogLevel string

	// ConfigPath 指向 YAML 配置文件。为空时 atomd 跑在 demo 模式下，
	// 不接任何真实模型与工具。
	ConfigPath string

	// KeyFile 是数据密钥文件的路径。数据库里的内容、工作区里的文件都靠它加密，
	// 它和这台机器的硬件绑在一起。不设置时与配置文件放在同一个目录。
	KeyFile string

	// VaultKey 是保险库密钥（base64 编码的 32 字节）。只从环境变量来，
	// 不进配置文件也不进数据库。为空时保险库处于锁定状态。
	VaultKey string

	// WebhookAddr 是 Webhook 入口的监听地址，如 127.0.0.1:8787。为空则不开。
	// WebhookToken 是调用方必须携带的令牌，开了入口就必须设置。
	WebhookAddr  string
	WebhookToken string

	// AdminAddr 是网页管理后台的监听地址，如 127.0.0.1:8780。为空则不开。
	// 不是本机地址时必须同时给出 TLS 证书，否则拒绝启动。
	AdminAddr string
	// AdminPasswordHash 是后台登录口令的 bcrypt 摘要，用 atomctl admin passwd 生成。
	AdminPasswordHash string
	// AdminTLSCert 与 AdminTLSKey 是后台的 TLS 证书和私钥文件。
	AdminTLSCert string
	AdminTLSKey  string

	// SkillScanner 是 Skill 安装前扫描器的可执行文件，空值按 skillspector。
	// SkillScannerArgs 是追加给它的参数，按空白分隔。
	// 这两项只能由用户在环境里配置：助手没有任何途径改它们，也就关不掉扫描。
	SkillScanner     string
	SkillScannerArgs []string
}

// Load 从环境变量读取配置。
//
// 非法值直接报错而不是静默回退到默认值：配置写错了却按默认值跑起来，
// 是最难排查的一类故障。
func Load() (*Config, error) {
	c := &Config{
		DSN:        os.Getenv("ATOM_DSN"),
		Owner:      os.Getenv("ATOM_OWNER"),
		LogLevel:   envString("ATOM_LOG_LEVEL", "info"),
		ConfigPath: os.Getenv("ATOM_CONFIG"),
		VaultKey:   os.Getenv("ATOM_VAULT_KEY"),
		KeyFile:    os.Getenv("ATOM_KEY_FILE"),

		WebhookAddr:  os.Getenv("ATOM_WEBHOOK_ADDR"),
		WebhookToken: os.Getenv("ATOM_WEBHOOK_TOKEN"),

		AdminAddr:         os.Getenv("ATOM_ADMIN_ADDR"),
		AdminPasswordHash: os.Getenv("ATOM_ADMIN_PASSWORD_HASH"),
		AdminTLSCert:      os.Getenv("ATOM_ADMIN_TLS_CERT"),
		AdminTLSKey:       os.Getenv("ATOM_ADMIN_TLS_KEY"),

		SkillScanner:     os.Getenv("ATOM_SKILL_SCANNER"),
		SkillScannerArgs: strings.Fields(os.Getenv("ATOM_SKILL_SCANNER_ARGS")),
	}
	if c.DSN == "" {
		return nil, fmt.Errorf("config: 必须设置 ATOM_DSN")
	}
	if c.KeyFile == "" {
		// 没有配置文件时落在当前目录：systemd 下那是数据目录，开发时是项目目录。
		c.KeyFile = filepath.Join(filepath.Dir(c.ConfigPath), seal.KeyFileName)
	}
	// 开了入口却没有令牌，等于把事件总线敞给所有能连上这个端口的人。
	if c.WebhookAddr != "" && c.WebhookToken == "" {
		return nil, fmt.Errorf("config: 设置了 ATOM_WEBHOOK_ADDR 就必须设置 ATOM_WEBHOOK_TOKEN")
	}

	var err error
	if c.Workers, err = envInt("ATOM_WORKERS", 4); err != nil {
		return nil, err
	}
	if c.MaxStepsPerClaim, err = envInt("ATOM_MAX_STEPS_PER_CLAIM", 16); err != nil {
		return nil, err
	}
	if c.PollInterval, err = envDuration("ATOM_POLL_INTERVAL", time.Second); err != nil {
		return nil, err
	}
	if c.LeaseDuration, err = envDuration("ATOM_LEASE", 2*time.Minute); err != nil {
		return nil, err
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("config: ATOM_LOG_LEVEL 取值非法: %q", c.LogLevel)
	}

	if c.Owner == "" {
		host, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("config: 无法确定主机名，请显式设置 ATOM_OWNER: %w", err)
		}
		c.Owner = "atomd@" + host
	}
	// 租约必须明显长于轮询间隔，否则任务会在正常执行中被误判为崩溃。
	if c.LeaseDuration <= c.PollInterval*2 {
		return nil, fmt.Errorf("config: ATOM_LEASE(%s) 必须显著大于 ATOM_POLL_INTERVAL(%s)",
			c.LeaseDuration, c.PollInterval)
	}
	return c, nil
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s 不是合法整数: %q", key, v)
	}
	if n <= 0 {
		return 0, fmt.Errorf("config: %s 必须为正数，实际 %d", key, n)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s 不是合法时长（如 30s / 2m）: %q", key, v)
	}
	if d <= 0 {
		return 0, fmt.Errorf("config: %s 必须为正数，实际 %s", key, d)
	}
	return d, nil
}

// Package config 从环境变量读取运行配置。
//
// 自部署形态下用户要自己改配置，所以每一项都有合理默认值，
// 只有 DSN 是必填的。
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
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

	// VaultKey 是保险库密钥（base64 编码的 32 字节）。只从环境变量来，
	// 不进配置文件也不进数据库。为空时保险库处于锁定状态。
	VaultKey string
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
	}
	if c.DSN == "" {
		return nil, fmt.Errorf("config: 必须设置 ATOM_DSN")
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

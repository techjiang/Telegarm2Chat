// Package config 负责从环境变量加载运行时配置。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 是进程级配置，启动后不再变更。
type Config struct {
	Addr            string
	DataDir         string
	DBPath          string
	Secret          []byte
	TokenTTL        time.Duration
	MaxBodyBytes    int64
	RateLimitPerMin int
	AllowedOrigins  []string
	ShutdownTimeout time.Duration

	TelegramToken   string
	TelegramAPIBase string
	TelegramPoll    time.Duration
	TelegramEnabled bool

	InviteCode string

	LogLevel string
}

// Load 读取环境变量并校验，缺省值面向单机部署。
func Load() (*Config, error) {
	c := &Config{
		Addr:            env("T2C_ADDR", ":8080"),
		DataDir:         env("T2C_DATA_DIR", "./data"),
		TokenTTL:        time.Duration(envInt("T2C_TOKEN_TTL_HOURS", 168)) * time.Hour,
		MaxBodyBytes:    int64(envInt("T2C_MAX_BODY_KB", 256)) * 1024,
		RateLimitPerMin: envInt("T2C_RATE_LIMIT_PER_MIN", 240),
		ShutdownTimeout: time.Duration(envInt("T2C_SHUTDOWN_SECONDS", 15)) * time.Second,
		TelegramToken:   os.Getenv("T2C_TELEGRAM_TOKEN"),
		TelegramAPIBase: env("T2C_TELEGRAM_API_BASE", "https://api.telegram.org"),
		TelegramPoll:    time.Duration(envInt("T2C_TELEGRAM_POLL_SECONDS", 25)) * time.Second,
		InviteCode:      os.Getenv("T2C_INVITE_CODE"),
		LogLevel:        strings.ToLower(env("T2C_LOG_LEVEL", "info")),
	}
	c.DBPath = env("T2C_DB_PATH", filepath.Join(c.DataDir, "t2c.db"))
	c.TelegramEnabled = c.TelegramToken != ""

	if origins := os.Getenv("T2C_ALLOWED_ORIGINS"); origins != "" {
		for _, o := range strings.Split(origins, ",") {
			if o = strings.TrimSpace(o); o != "" {
				c.AllowedOrigins = append(c.AllowedOrigins, o)
			}
		}
	}

	if c.MaxBodyBytes <= 0 || c.MaxBodyBytes > 8<<20 {
		return nil, errors.New("config: T2C_MAX_BODY_KB 需在 1..8192 之间")
	}
	if c.RateLimitPerMin < 0 {
		return nil, errors.New("config: T2C_RATE_LIMIT_PER_MIN 不能为负")
	}

	secret, err := loadSecret(c.DataDir)
	if err != nil {
		return nil, err
	}
	c.Secret = secret
	return c, nil
}

// loadSecret 优先使用 T2C_SECRET，否则持久化一份随机密钥，保证重启后 token 仍有效。
func loadSecret(dataDir string) ([]byte, error) {
	if s := os.Getenv("T2C_SECRET"); s != "" {
		if len(s) < 16 {
			return nil, errors.New("config: T2C_SECRET 至少 16 字节")
		}
		return []byte(s), nil
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("config: 创建数据目录: %w", err)
	}
	path := filepath.Join(dataDir, "secret.key")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("config: 生成密钥: %w", err)
	}
	enc := []byte(hex.EncodeToString(secret))
	if err := os.WriteFile(path, enc, 0o600); err != nil {
		return nil, fmt.Errorf("config: 写入密钥: %w", err)
	}
	return enc, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

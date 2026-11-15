package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("T2C_DATA_DIR", dir)
	for _, k := range []string{"T2C_ADDR", "T2C_MAX_BODY_KB", "T2C_RATE_LIMIT_PER_MIN", "T2C_SECRET", "T2C_INVITE_CODE"} {
		t.Setenv(k, "")
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8080" {
		t.Fatalf("默认地址错误: %s", c.Addr)
	}
	if c.MaxBodyBytes != 256<<10 {
		t.Fatalf("默认体积上限错误: %d", c.MaxBodyBytes)
	}
	if c.TokenTTL != 168*time.Hour {
		t.Fatalf("默认 TTL 错误: %v", c.TokenTTL)
	}
	if c.DBPath != filepath.Join(dir, "t2c.db") {
		t.Fatalf("默认 DB 路径错误: %s", c.DBPath)
	}
	if len(c.Secret) < 32 {
		t.Fatalf("密钥过短: %d", len(c.Secret))
	}
	if c.TelegramEnabled {
		t.Fatal("未配 token 时不应启用 Telegram")
	}
}

func TestSecretPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("T2C_DATA_DIR", dir)
	t.Setenv("T2C_SECRET", "")

	c1, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if string(c1.Secret) != string(c2.Secret) {
		t.Fatal("重启后密钥应保持一致，否则所有 token 失效")
	}
	// 权限应为 0600
	info, err := os.Stat(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("密钥文件权限应为 0600，得到 %o", perm)
	}
}

func TestExplicitSecret(t *testing.T) {
	t.Setenv("T2C_DATA_DIR", t.TempDir())
	t.Setenv("T2C_SECRET", "0123456789abcdef")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Secret) != "0123456789abcdef" {
		t.Fatal("显式密钥未被采用")
	}

	t.Setenv("T2C_SECRET", "short")
	if _, err := Load(); err == nil {
		t.Fatal("过短密钥应报错")
	}
}

func TestInvalidSizes(t *testing.T) {
	t.Setenv("T2C_DATA_DIR", t.TempDir())
	t.Setenv("T2C_MAX_BODY_KB", "0")
	if _, err := Load(); err == nil {
		t.Fatal("0 体积上限应报错")
	}
	t.Setenv("T2C_MAX_BODY_KB", "99999")
	if _, err := Load(); err == nil {
		t.Fatal("超大体积上限应报错")
	}
}

func TestAllowedOriginsParsing(t *testing.T) {
	t.Setenv("T2C_DATA_DIR", t.TempDir())
	t.Setenv("T2C_ALLOWED_ORIGINS", " https://a.com , https://b.com ,, ")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AllowedOrigins) != 2 || c.AllowedOrigins[0] != "https://a.com" {
		t.Fatalf("Origin 解析错误: %v", c.AllowedOrigins)
	}
}

func TestTelegramAutoEnable(t *testing.T) {
	t.Setenv("T2C_DATA_DIR", t.TempDir())
	t.Setenv("T2C_TELEGRAM_TOKEN", "123:abc")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.TelegramEnabled {
		t.Fatal("配置 token 后应自动启用")
	}
}

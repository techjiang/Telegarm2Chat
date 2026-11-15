// Package auth 提供密码哈希与无状态会话令牌。
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// 密码策略常量。
const (
	MinPasswordLen = 10
	MaxPasswordLen = 256
)

var ErrWeakPassword = fmt.Errorf("auth: 密码长度需在 %d..%d 之间", MinPasswordLen, MaxPasswordLen)

// argon2id 参数：OWASP 建议的中档配置，单次约 40ms / 64MB。
const (
	argonTime    = 2
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword 生成 PHC 格式的 argon2id 摘要。
func HashPassword(password string) (string, error) {
	if len(password) < MinPasswordLen || len(password) > MaxPasswordLen {
		return "", ErrWeakPassword
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: 生成盐: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

var b64 = base64.RawStdEncoding

// VerifyPassword 常量时间校验密码；参数不合法时返回 false 而非报错。
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var memory uint32
	var iter uint32
	var threads uint8
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return false
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return false
		}
		switch k {
		case "m":
			memory = uint32(n)
		case "t":
			iter = uint32(n)
		case "p":
			threads = uint8(n)
		}
	}
	if memory == 0 || iter == 0 || threads == 0 || memory > 1<<20 || iter > 10 {
		return false
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iter, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// DummyVerify 在用户不存在时消耗同等算力，避免用户名枚举的时间侧信道。
func DummyVerify(password string) {
	salt := make([]byte, argonSaltLen)
	binary.BigEndian.PutUint64(salt, uint64(len(password)))
	argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
}

var ErrInvalidToken = errors.New("auth: 令牌无效")

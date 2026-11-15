package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"strconv"
	"time"
)

// Token 是自包含的会话令牌（HMAC-SHA256 签名），无需服务端会话表。
type Token struct {
	UserID    int64
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// TokenIssuer 用进程密钥签发与校验令牌。
type TokenIssuer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewTokenIssuer 创建签发器。
func NewTokenIssuer(secret []byte, ttl time.Duration) *TokenIssuer {
	return &TokenIssuer{secret: secret, ttl: ttl, now: time.Now}
}

// Issue 为指定用户签发令牌，复用 sqlite 行号语义的纯数字 userID。
func (t *TokenIssuer) Issue(userID int64) string {
	iat := t.now().Unix()
	payload := strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(iat, 10)
	return payload + ":" + t.sign(payload)
}

// Parse 校验签名与有效期。
func (t *TokenIssuer) Parse(token string) (Token, error) {
	userIDStr, iatStr, sig, ok := cut3(token)
	if !ok {
		return Token{}, ErrInvalidToken
	}
	if !hmac.Equal([]byte(sig), []byte(t.sign(userIDStr+":"+iatStr))) {
		return Token{}, ErrInvalidToken
	}
	userID, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil || userID <= 0 {
		return Token{}, ErrInvalidToken
	}
	iat, err := strconv.ParseInt(iatStr, 10, 64)
	if err != nil {
		return Token{}, ErrInvalidToken
	}
	issued := time.Unix(iat, 0)
	expires := issued.Add(t.ttl)
	if t.now().After(expires) {
		return Token{}, ErrInvalidToken
	}
	return Token{UserID: userID, IssuedAt: issued, ExpiresAt: expires}, nil
}

// sign 产出 URL 安全的截断 HMAC，长度足够抵抗伪造。
func (t *TokenIssuer) sign(payload string) string {
	m := hmac.New(sha256.New, t.secret)
	m.Write([]byte(payload))
	sum := m.Sum(nil)[:16]
	return base64.RawURLEncoding.EncodeToString(sum)
}

func cut3(s string) (string, string, string, bool) {
	var parts [3]string
	n := 0
	start := 0
	for i := 0; i < len(s) && n < 2; i++ {
		if s[i] == ':' {
			parts[n] = s[start:i]
			n++
			start = i + 1
		}
	}
	if n != 2 {
		return "", "", "", false
	}
	parts[2] = s[start:]
	if parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// Fingerprint 生成稳定的非密摘要，用于审计日志里标记设备。
func Fingerprint(parts ...string) string {
	h := sha256.New()
	var buf [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(buf[:], uint64(len(p)))
		h.Write(buf[:])
		h.Write([]byte(p))
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:9])
}

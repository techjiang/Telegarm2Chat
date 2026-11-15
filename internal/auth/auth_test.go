package auth

import (
	"strings"
	"testing"
	"time"
)

func TestHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatalf("PHC 格式异常: %s", hash)
	}
	if !VerifyPassword(hash, "correct horse battery") {
		t.Fatal("正确密码校验失败")
	}
	if VerifyPassword(hash, "wrong horse battery") {
		t.Fatal("错误密码通过了校验")
	}
}

func TestHashSaltsAreUnique(t *testing.T) {
	a, _ := HashPassword("same-password-123")
	b, _ := HashPassword("same-password-123")
	if a == b {
		t.Fatal("相同密码产生了相同摘要，盐失效")
	}
}

func TestWeakPassword(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("短密码未被拒绝")
	}
	if _, err := HashPassword(strings.Repeat("x", MaxPasswordLen+1)); err == nil {
		t.Fatal("超长密码未被拒绝")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	cases := []string{
		"", "plaintext", "$argon2id$v=19$m=x,t=2,p=4$abc$def",
		"$argon2i$v=19$m=65536,t=2,p=4$YWJj$ZGVm",
		"$argon2id$v=19$m=65536,t=2,p=4$!!!$def",
	}
	for _, c := range cases {
		if VerifyPassword(c, "anything") {
			t.Fatalf("畸形摘要被误判为有效: %q", c)
		}
	}
}

func TestTokenRoundTrip(t *testing.T) {
	iss := NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	tok := iss.Issue(42)
	got, err := iss.Parse(tok)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.UserID != 42 {
		t.Fatalf("用户 ID 不匹配: %d", got.UserID)
	}
	if time.Until(got.ExpiresAt) <= 0 {
		t.Fatal("令牌已过期")
	}
}

func TestTokenTamperResistance(t *testing.T) {
	iss := NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	tok := iss.Issue(1)

	for _, mutate := range []string{
		"2" + tok[1:],                     // 改用户 ID
		tok + "x",                         // 追加字符
		strings.Replace(tok, ":", ";", 1), // 破坏分隔符
	} {
		if _, err := iss.Parse(mutate); err == nil {
			t.Fatalf("篡改后的令牌被接受: %q", mutate)
		}
	}
}

func TestTokenExpiry(t *testing.T) {
	iss := NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Second)
	now := time.Unix(1700000000, 0)
	iss.now = func() time.Time { return now }
	tok := iss.Issue(7)
	if _, err := iss.Parse(tok); err != nil {
		t.Fatalf("有效期内解析失败: %v", err)
	}
	iss.now = func() time.Time { return now.Add(2 * time.Second) }
	if _, err := iss.Parse(tok); err == nil {
		t.Fatal("过期令牌被接受")
	}
}

func TestTokenCrossKeyRejected(t *testing.T) {
	a := NewTokenIssuer([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), time.Hour)
	b := NewTokenIssuer([]byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), time.Hour)
	if _, err := b.Parse(a.Issue(1)); err == nil {
		t.Fatal("不同密钥签发的令牌被接受")
	}
}

func BenchmarkHashPassword(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := HashPassword("benchmarking-password"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVerifyPassword(b *testing.B) {
	hash, _ := HashPassword("benchmarking-password")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !VerifyPassword(hash, "benchmarking-password") {
			b.Fatal("校验失败")
		}
	}
}

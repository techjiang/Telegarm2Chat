package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/config"
)

// 并发注册时只能有一个管理员：判定与插入必须在同一事务内。
func TestConcurrentRegisterSingleAdmin(t *testing.T) {
	h := newHarness(t)

	const n = 6
	var wg sync.WaitGroup
	type result struct {
		code int
		body map[string]any
	}
	results := make([]result, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			code, out := h.do("POST", "/api/v1/auth/register", "", map[string]string{
				"username": userN(idx), "password": "password12345",
			})
			results[idx] = result{code, out}
		}(i)
	}
	wg.Wait()

	admins := 0
	for _, r := range results {
		if r.code != 200 {
			continue
		}
		d, ok := r.body["data"].(map[string]any)
		if !ok {
			continue
		}
		u := d["user"].(map[string]any)
		if u["isAdmin"] == true {
			admins++
		}
	}
	if admins != 1 {
		t.Fatalf("应恰好有 1 个管理员，实际 %d 个", admins)
	}
}

func userN(i int) string {
	return "racer" + string(rune('a'+i))
}

// 未开启信任代理时，伪造 X-Forwarded-For 不应改变识别到的 IP。
func TestForwardedForIgnoredByDefault(t *testing.T) {
	t.Setenv("T2C_TRUST_PROXY", "")
	h := newHarness(t)

	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/meta", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// 通过日志无法断言，这里直接验证 realIP 的行为
	req2, _ := http.NewRequest("GET", "/", nil)
	req2.RemoteAddr = "9.9.9.9:1234"
	req2.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := realIP(req2, false); got != "9.9.9.9" {
		t.Fatalf("不应采信 XFF，得到 %s", got)
	}
	if got := realIP(req2, true); got != "1.2.3.4" {
		t.Fatalf("显式信任时应采信 XFF，得到 %s", got)
	}
}

// 未知字段 / 超长字段的健壮性
func TestRegisterInputValidation(t *testing.T) {
	h := newHarness(t)
	cases := []map[string]string{
		{"username": "ab", "password": "password12345"},                    // 太短
		{"username": "with space", "password": "password12345"},            // 空格
		{"username": "okname", "password": "short"},                        // 密码短
		{"username": "okname2", "password": strings.Repeat("x", 300)},      // 密码超长
		{"username": strings.Repeat("a", 40), "password": "password12345"}, // 用户名超长
	}
	for i, c := range cases {
		if code, _ := h.do("POST", "/api/v1/auth/register", "", c); code != 400 {
			t.Fatalf("用例 %d 应 400，得到 %d (%v)", i, code, c)
		}
	}

	// 大写用户名会被归一化为小写，属于预期行为
	code, out := h.do("POST", "/api/v1/auth/register", "", map[string]string{
		"username": "MixedCase", "password": "password12345",
	})
	if code != 200 {
		t.Fatalf("大写用户名应被归一化而非拒绝，得到 %d %v", code, out)
	}
	u := dataMap(t, out)["user"].(map[string]any)
	if u["username"] != "mixedcase" {
		t.Fatalf("用户名应归一化为小写，得到 %v", u["username"])
	}
}

// SQL 注入样本不应导致 500
func TestSQLInjectionSamples(t *testing.T) {
	h := newHarness(t)
	tok, _ := h.register("sqluser", "password12345")
	payloads := []string{
		"' OR '1'='1", "'; DROP TABLE users;--", "1' UNION SELECT * FROM users--",
		"admin'--", "\" OR \"\"=\"", "%", "_%_",
	}
	for _, p := range payloads {
		code, _ := h.do("GET", "/api/v1/search?q="+urlq(p), tok, nil)
		if code >= 500 {
			t.Fatalf("注入样本 %q 触发了 %d", p, code)
		}
		code, _ = h.do("POST", "/api/v1/chats/join", tok, map[string]string{"slug": p})
		if code >= 500 {
			t.Fatalf("注入样本 %q 触发了 %d", p, code)
		}
	}
	// 用户表仍然存在
	if code, _ := h.do("GET", "/api/v1/me", tok, nil); code != 200 {
		t.Fatalf("注入后用户表不可用: %d", code)
	}
}

func urlq(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			b.WriteByte(c)
		default:
			b.WriteString("%")
			const hex = "0123456789ABCDEF"
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xF])
		}
	}
	return b.String()
}

// 伪造 X-Forwarded-For 不能绕过限流（默认不信任代理头）。
func TestRateLimitNotBypassedBySpoofedHeader(t *testing.T) {
	t.Setenv("T2C_TRUST_PROXY", "")
	h := newHarness(t)
	cfg := &config.Config{
		MaxBodyBytes:    64 << 10,
		RateLimitPerMin: 6,
		TokenTTL:        time.Hour,
	}
	s := New(Options{
		Config:  cfg,
		Service: h.svc,
		Issuer:  h.issuer,
		Hub:     h.hub,
	})
	srv := newTestServer(t, s)
	defer srv.Close()

	limited := false
	for i := 0; i < 40; i++ {
		req, _ := http.NewRequest("POST", srv.URL+"/api/v1/auth/login",
			strings.NewReader(`{"username":"nobody","password":"whatever12345"}`))
		req.Header.Set("Content-Type", "application/json")
		// 每次换一个伪造 IP
		req.Header.Set("X-Forwarded-For", "10.0.0."+itoa(i))
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("伪造 XFF 绕过了限流")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// 关闭不能让进程卡住：没有活跃连接时必须立刻完成。
func TestShutdownIsFastWithoutConnections(t *testing.T) {
	h := newHarness(t)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.hub.Shutdown(ctx)
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("关闭耗时 %v，疑似无条件等待超时", d)
	}
}

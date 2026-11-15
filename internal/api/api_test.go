package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/auth"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/chat"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/config"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/realtime"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/store"
)

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	st     *store.Store
	hub    *realtime.Hub
	svc    *chat.Service
	issuer *auth.TokenIssuer
	cfg    *config.Config
}

// newTestServer 用给定 Server 起一个测试用 HTTP 服务。
func newTestServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(s.Handler())
	return srv
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	hub := realtime.NewHub()
	svc := chat.New(st, hub)
	cfg := &config.Config{
		MaxBodyBytes:    64 << 10,
		RateLimitPerMin: 100000, // 测试里不触发限流
		TokenTTL:        time.Hour,
		AllowedOrigins:  nil,
	}
	issuer := auth.NewTokenIssuer([]byte("test-secret-key-0123456789abc"), cfg.TokenTTL)
	s := New(Options{Config: cfg, Service: svc, Issuer: issuer, Hub: hub})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		srv.Close()
		st.Close()
	})
	return &harness{t: t, srv: srv, st: st, hub: hub, svc: svc, issuer: issuer, cfg: cfg}
}

func (h *harness) do(method, path, token string, body any) (int, map[string]any) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (h *harness) register(username, password string) (string, int64) {
	h.t.Helper()
	code, out := h.do("POST", "/api/v1/auth/register", "", map[string]string{
		"username": username, "password": password,
	})
	if code != 200 {
		h.t.Fatalf("注册 %s 失败: %d %v", username, code, out)
	}
	data := out["data"].(map[string]any)
	user := data["user"].(map[string]any)
	return data["token"].(string), int64(user["id"].(float64))
}

func dataMap(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	d, ok := out["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 data: %v", out)
	}
	return d
}

func TestHealthAndMeta(t *testing.T) {
	h := newHarness(t)
	code, out := h.do("GET", "/healthz", "", nil)
	if code != 200 || dataMap(t, out)["status"] != "ok" {
		t.Fatalf("healthz 异常: %d %v", code, out)
	}
	code, out = h.do("GET", "/api/v1/meta", "", nil)
	if code != 200 || dataMap(t, out)["firstRun"] != true {
		t.Fatalf("meta 应标记首次运行: %d %v", code, out)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	resp, err := h.srv.Client().Get(h.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Fatalf("%s = %q，期望 %q", k, got, v)
		}
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatal("CSP 缺失")
	}
}

func TestAuthFlow(t *testing.T) {
	h := newHarness(t)
	adminTok, _ := h.register("admin", "adminpass1234")
	userTok, _ := h.register("user1", "userpass12345")

	code, out := h.do("GET", "/api/v1/me", adminTok, nil)
	if code != 200 || dataMap(t, out)["isAdmin"] != true {
		t.Fatalf("首用户应为管理员: %d %v", code, out)
	}
	code, out = h.do("GET", "/api/v1/me", userTok, nil)
	if code != 200 || dataMap(t, out)["isAdmin"] != false {
		t.Fatalf("第二用户不应是管理员: %d %v", code, out)
	}
	if code, _ := h.do("GET", "/api/v1/me", "", nil); code != 401 {
		t.Fatalf("无 token 应 401，得到 %d", code)
	}
	if code, _ := h.do("GET", "/api/v1/me", "bad.token.here", nil); code != 401 {
		t.Fatalf("坏 token 应 401，得到 %d", code)
	}
}

func TestMessageLeakPrevention(t *testing.T) {
	h := newHarness(t)
	aliceTok, _ := h.register("alice", "alicepass1234")
	bobTok, _ := h.register("bobbb", "bobpass12345")
	_, _ = aliceTok, bobTok
	eveTok, _ := h.register("eve", "evepass12345")

	code, out := h.do("POST", "/api/v1/chats", aliceTok, map[string]string{"title": "秘密频道"})
	if code != 201 {
		t.Fatalf("建频道失败: %d %v", code, out)
	}
	chatID := dataMap(t, out)["id"].(float64)

	code, out = h.do("POST", "/api/v1/chats/"+ftoa(chatID)+"/messages", aliceTok,
		map[string]string{"body": "机密内容"})
	if code != 201 {
		t.Fatalf("发消息失败: %d %v", code, out)
	}

	// eve 不是成员
	code, _ = h.do("GET", "/api/v1/chats/"+ftoa(chatID)+"/messages", eveTok, nil)
	if code != 403 {
		t.Fatalf("非成员读消息应 403，得到 %d", code)
	}
	code, _ = h.do("POST", "/api/v1/chats/"+ftoa(chatID)+"/messages", eveTok, map[string]string{"body": "闯入"})
	if code != 403 {
		t.Fatalf("非成员发消息应 403，得到 %d", code)
	}
	code, out = h.do("GET", "/api/v1/search?q=%E6%9C%BA%E5%AF%86", eveTok, nil)
	if hits, _ := dataMap(t, out)["hits"].([]any); len(hits) != 0 {
		t.Fatalf("非成员不应搜到内容，得到 %d 条", len(hits))
	}
}

func TestBodySizeLimit(t *testing.T) {
	h := newHarness(t)
	tok, _ := h.register("sizer", "sizerpass1234")
	code, out := h.do("POST", "/api/v1/chats", tok, map[string]string{"title": "x"})
	if code != 201 {
		t.Fatalf("建频道失败: %d", code)
	}
	id := ftoa(dataMap(t, out)["id"].(float64))

	// 超过 64KB 的请求体应被拒绝
	huge := strings.Repeat("啊", 40*1024)
	code, _ = h.do("POST", "/api/v1/chats/"+id+"/messages", tok, map[string]string{"body": huge})
	if code != 400 && code != 413 {
		t.Fatalf("超大请求体应被拒绝，得到 %d", code)
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	h := newHarness(t)
	tok, _ := h.register("strict", "strictpass1234")
	code, _ := h.do("POST", "/api/v1/chats", tok, map[string]any{"title": "x", "evil": "y"})
	if code != 400 {
		t.Fatalf("未知字段应被拒，得到 %d", code)
	}
}

func TestWebSocketRealtimeDelivery(t *testing.T) {
	h := newHarness(t)
	aliceTok, _ := h.register("wsalice", "alicepass1234")
	bobTok, bobID := h.register("wsbob", "bobpass12345")

	code, out := h.do("POST", "/api/v1/chats", aliceTok, map[string]string{"title": "实时频道"})
	if code != 201 {
		t.Fatalf("建频道失败: %d", code)
	}
	chatID := dataMap(t, out)["id"].(float64)
	slug := dataMap(t, out)["slug"].(string)
	if code, _ := h.do("POST", "/api/v1/chats/join", bobTok, map[string]string{"slug": slug}); code != 200 {
		t.Fatalf("bob 加入失败: %d", code)
	}

	wsURL := "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/api/v1/ws?token=" + bobTok
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket 连接失败: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// 首帧应是 ready
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读取 ready 失败: %v", err)
	}
	var ready map[string]any
	_ = json.Unmarshal(data, &ready)
	if ready["type"] != "ready" {
		t.Fatalf("首帧应为 ready，得到 %v", ready)
	}

	// alice 发消息，bob 应实时收到
	if code, _ := h.do("POST", "/api/v1/chats/"+ftoa(chatID)+"/messages", aliceTok,
		map[string]string{"body": "实时推送测试"}); code != 201 {
		t.Fatalf("发消息失败: %d", code)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("读取消息事件失败: %v", err)
		}
		var ev map[string]any
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		if ev["type"] == "message" {
			payload := ev["data"].(map[string]any)
			if payload["body"] != "实时推送测试" {
				t.Fatalf("消息内容不符: %v", payload)
			}
			if int64(payload["senderId"].(float64)) == bobID {
				t.Fatal("发送者 ID 异常")
			}
			return
		}
	}
	t.Fatal("未在超时内收到实时消息")
}

func TestWebSocketCrossOriginRejected(t *testing.T) {
	h := newHarness(t)
	tok, _ := h.register("origin", "originpass1234")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/api/v1/ws?token=" + tok
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}},
	})
	if err == nil {
		t.Fatal("跨站 WebSocket 应被拒绝")
	}
	if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("期望 403，得到 %d", resp.StatusCode)
	}
}

func TestWebSocketRequiresAuth(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/api/v1/ws"
	if _, _, err := websocket.Dial(ctx, wsURL, nil); err == nil {
		t.Fatal("无 token 的 WebSocket 连接应失败")
	}
}

func ftoa(f float64) string {
	n := int64(f)
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

var _ = context.Background

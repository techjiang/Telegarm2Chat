package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/store"
)

// fakeTG 模拟 Bot API，记录调用并返回预设响应。
type fakeTG struct {
	mu       sync.Mutex
	sent     []string
	failNext int
	updates  [][]Update
	updIdx   int
}

func (f *fakeTG) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/bot", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		method := parts[len(parts)-1]
		f.mu.Lock()
		defer f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getMe":
			fmt.Fprint(w, `{"ok":true,"result":{"username":"testbot"}}`)
		case "getUpdates":
			var out []Update
			if f.updIdx < len(f.updates) {
				out = f.updates[f.updIdx]
				f.updIdx++
			}
			b, _ := json.Marshal(out)
			fmt.Fprintf(w, `{"ok":true,"result":%s}`, b)
		case "sendMessage":
			if f.failNext > 0 {
				f.failNext--
				w.WriteHeader(500)
				fmt.Fprint(w, `{"ok":false,"error_code":500,"description":"internal"}`)
				return
			}
			var payload struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			f.sent = append(f.sent, payload.Text)
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
		default:
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	})
	return mux
}

func setup(t *testing.T) (*store.Store, *store.Chat, *fakeTG, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "owner1", "", "h", true)
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.CreateChannel(ctx, u.ID, "频道", "")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeTG{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return st, &c, fake, srv
}

func TestSendMessageRedactsToken(t *testing.T) {
	_, _, _, srv := setup(t)
	// 用一个必然失败的地址，检查错误里不含 token
	c := NewClient("SECRET_TOKEN_123", "http://127.0.0.1:1")
	_, err := c.SendMessage(context.Background(), "1", "hi")
	if err == nil {
		t.Fatal("应失败")
	}
	if strings.Contains(err.Error(), "SECRET_TOKEN_123") {
		t.Fatalf("错误信息泄露了 token: %v", err)
	}
	_ = srv
}

func TestClientGetMe(t *testing.T) {
	_, _, _, srv := setup(t)
	c := NewClient("tok", srv.URL)
	name, err := c.GetMe(context.Background())
	if err != nil || name != "testbot" {
		t.Fatalf("GetMe 失败: %q %v", name, err)
	}
}

func TestAPIErrorRetryable(t *testing.T) {
	cases := map[int]bool{400: false, 403: false, 429: true, 500: true, 502: true}
	for status, want := range cases {
		e := &APIError{Status: status}
		if e.Retryable() != want {
			t.Fatalf("status %d Retryable 应为 %v", status, want)
		}
	}
}

func TestInboundMessagePropagation(t *testing.T) {
	st, chat, fake, srv := setup(t)
	ctx := context.Background()
	if err := st.LinkTelegram(ctx, chat.ID, "-100777"); err != nil {
		t.Fatal(err)
	}

	fake.updates = [][]Update{{
		{UpdateID: 100, Message: &TGMessage{
			MessageID: 5, Text: "来自 TG 的问候",
			Chat: struct {
				ID    int64  `json:"id"`
				Type  string `json:"type"`
				Title string `json:"title"`
			}{ID: -100777, Type: "supergroup", Title: "群"},
		}},
	}}
	// 补上 From 字段（匿名结构体无法直接字面量赋值）
	fake.updates[0][0].Message.From = &struct {
		ID        int64  `json:"id"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Username  string `json:"username"`
		IsBot     bool   `json:"is_bot"`
	}{ID: 42, FirstName: "张三", Username: "zhangsan"}

	var posted []string
	var mu sync.Mutex
	b := NewBridge(NewClient("tok", srv.URL), st, 200*time.Millisecond,
		func(ctx context.Context, chatID int64, sender, body string) error {
			mu.Lock()
			posted = append(posted, fmt.Sprintf("%d|%s|%s", chatID, sender, body))
			mu.Unlock()
			return nil
		})

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b.Start(ctx)
	defer b.Stop(context.Background())

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(posted)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) == 0 {
		t.Fatal("入站消息未传播")
	}
	if !strings.Contains(posted[0], "来自 TG 的问候") || !strings.Contains(posted[0], "张三") {
		t.Fatalf("消息内容不符: %s", posted[0])
	}
	if !strings.Contains(posted[0], fmt.Sprint(chat.ID)) {
		t.Fatalf("会话映射错误: %s", posted[0])
	}
}

func TestInboundIgnoresUnlinkedAndBots(t *testing.T) {
	st, chat, fake, srv := setup(t)
	ctx := context.Background()
	_ = st.LinkTelegram(ctx, chat.ID, "-100777")

	botMsg := &TGMessage{MessageID: 1, Text: "我是机器人", Chat: struct {
		ID    int64  `json:"id"`
		Type  string `json:"type"`
		Title string `json:"title"`
	}{ID: -100777}}
	botMsg.From = &struct {
		ID        int64  `json:"id"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Username  string `json:"username"`
		IsBot     bool   `json:"is_bot"`
	}{ID: 9, FirstName: "bot", IsBot: true}

	unlinked := &TGMessage{MessageID: 2, Text: "别的群", Chat: struct {
		ID    int64  `json:"id"`
		Type  string `json:"type"`
		Title string `json:"title"`
	}{ID: -999}}
	unlinked.From = &struct {
		ID        int64  `json:"id"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Username  string `json:"username"`
		IsBot     bool   `json:"is_bot"`
	}{ID: 8, FirstName: "路人"}

	fake.updates = [][]Update{{{UpdateID: 1, Message: botMsg}, {UpdateID: 2, Message: unlinked}}}

	var count int
	var mu sync.Mutex
	b := NewBridge(NewClient("tok", srv.URL), st, 200*time.Millisecond,
		func(context.Context, int64, string, string) error {
			mu.Lock()
			count++
			mu.Unlock()
			return nil
		})
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	b.Start(ctx)
	time.Sleep(900 * time.Millisecond)
	b.Stop(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Fatalf("机器人与未绑定群消息不应入库，得到 %d", count)
	}
}

func TestOutboundDeliveryAndRetry(t *testing.T) {
	st, chat, fake, srv := setup(t)
	ctx := context.Background()
	_ = st.LinkTelegram(ctx, chat.ID, "-100777")

	u, _ := st.UserByUsername(ctx, "owner1")
	m, _, err := st.AppendMessage(ctx, store.Message{
		ChatID: chat.ID, SenderID: u.ID, SenderName: "owner1", Body: "要同步到 TG",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueTelegram(ctx, chat.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	fake.failNext = 1 // 第一次失败，验证重试

	b := NewBridge(NewClient("tok", srv.URL), st, time.Second, nil)
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	b.Start(cctx)
	defer b.Stop(context.Background())

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		n := len(fake.sent)
		fake.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sent) != 1 {
		t.Fatalf("应成功投递 1 条，得到 %d", len(fake.sent))
	}
	if !strings.Contains(fake.sent[0], "要同步到 TG") || !strings.Contains(fake.sent[0], "owner1") {
		t.Fatalf("投递内容不符: %s", fake.sent[0])
	}
}

func TestOutboundSkipsUnlinkedChat(t *testing.T) {
	st, chat, fake, srv := setup(t)
	ctx := context.Background()
	// 未绑定
	b := NewBridge(NewClient("tok", srv.URL), st, time.Second, nil)
	u, _ := st.UserByUsername(ctx, "owner1")
	m, _, _ := st.AppendMessage(ctx, store.Message{ChatID: chat.ID, SenderID: u.ID, SenderName: "o", Body: "不该发"})
	b.Enqueue(ctx, chat.ID, m.ID)

	items, err := st.ClaimTelegramBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("未绑定会话不应入队，得到 %d", len(items))
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sent) != 0 {
		t.Fatal("不应有发送")
	}
}

func TestLongMessageTruncated(t *testing.T) {
	st, chat, fake, srv := setup(t)
	ctx := context.Background()
	_ = st.LinkTelegram(ctx, chat.ID, "-100777")
	u, _ := st.UserByUsername(ctx, "owner1")
	m, _, err := st.AppendMessage(ctx, store.Message{
		ChatID: chat.ID, SenderID: u.ID, SenderName: "owner1",
		Body: strings.Repeat("长", 2000),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.EnqueueTelegram(ctx, chat.ID, m.ID)

	b := NewBridge(NewClient("tok", srv.URL), st, time.Second, nil)
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	b.Start(cctx)
	defer b.Stop(context.Background())

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		n := len(fake.sent)
		fake.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sent) == 0 {
		t.Fatal("未投递")
	}
	if len([]rune(fake.sent[0])) > 4100 {
		t.Fatalf("超长消息未截断: %d 字符", len([]rune(fake.sent[0])))
	}
}

func TestRedact(t *testing.T) {
	if got := redact("error for token abc123", "abc123"); strings.Contains(got, "abc123") {
		t.Fatalf("脱敏失败: %s", got)
	}
	if got := redact("no secret here", ""); got != "no secret here" {
		t.Fatal("空密钥不该改动文本")
	}
}

package chat

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/realtime"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/store"
)

func newSvc(t *testing.T) (*Service, *store.Store, *realtime.Hub) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "svc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hub := realtime.NewHub()
	return New(st, hub), st, hub
}

func mkUser(t *testing.T, st *store.Store, name string) store.User {
	t.Helper()
	u, err := st.CreateUser(context.Background(), name, name, "h", false)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSendRejectsNonMember(t *testing.T) {
	svc, st, _ := newSvc(t)
	ctx := context.Background()
	owner := mkUser(t, st, "owner")
	outsider := mkUser(t, st, "outsider")
	c, err := svc.CreateChannel(ctx, owner, "频道", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Send(ctx, outsider, SendInput{ChatID: c.ID, Body: "闯入"})
	if !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("非成员发送应 403，得到 %v", err)
	}
}

func TestMentionOnlyMembersOfChat(t *testing.T) {
	svc, st, _ := newSvc(t)
	ctx := context.Background()
	a := mkUser(t, st, "alice")
	b := mkUser(t, st, "bob")
	_ = mkUser(t, st, "carol") // 非成员
	c, err := svc.CreateChannel(ctx, a, "频道", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(ctx, c.ID, b.ID, store.RoleMember); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Send(ctx, a, SendInput{ChatID: c.ID, Body: "@bob @carol @nobody 看这里 @bob"})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, m := range res.Mentions {
		names[m.Username] = true
	}
	if !names["bob"] {
		t.Fatal("应解析出 bob")
	}
	if names["carol"] {
		t.Fatal("carol 未入群，不应被解析")
	}
	if names["nobody"] {
		t.Fatal("不存在的用户不应被解析")
	}
	// 去重：@bob 出现两次只应有一条
	cnt := 0
	for _, m := range res.Mentions {
		if m.Username == "bob" {
			cnt++
		}
	}
	if cnt != 1 {
		t.Fatalf("提及应去重，bob 出现 %d 次", cnt)
	}
}

func TestSendPushesToMembersOnly(t *testing.T) {
	svc, st, hub := newSvc(t)
	ctx := context.Background()
	a := mkUser(t, st, "sender")
	b := mkUser(t, st, "listener")
	c := mkUser(t, st, "stranger")
	c_, _ := svc.CreateChannel(ctx, a, "频道", "")
	if err := st.AddMember(ctx, c_.ID, b.ID, store.RoleMember); err != nil {
		t.Fatal(err)
	}

	cb, ok := hub.Register(b.ID)
	if !ok {
		t.Fatal("注册失败")
	}
	defer cb.Close()
	cc, _ := hub.Register(c.ID)
	defer cc.Close()

	if _, err := svc.Send(ctx, a, SendInput{ChatID: c_.ID, Body: "只发给成员"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cb.Out():
	case <-time.After(time.Second):
		t.Fatal("成员未收到推送")
	}
	select {
	case <-cc.Out():
		t.Fatal("非成员收到了推送——严重越权")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestSendSanitizesControlChars(t *testing.T) {
	svc, st, _ := newSvc(t)
	ctx := context.Background()
	u := mkUser(t, st, "sender2")
	c, _ := svc.CreateChannel(ctx, u, "频道", "")
	res, err := svc.Send(ctx, u, SendInput{ChatID: c.ID, Body: "好\x00的\x07\u001b[31m红\n第二行"})
	if err != nil {
		t.Fatal(err)
	}
	body := res.Message.Body
	for _, bad := range []string{"\x00", "\x07", "\u001b"} {
		if strings.Contains(body, bad) {
			t.Fatalf("控制字符未清除: %q", body)
		}
	}
	if !strings.Contains(body, "\n") {
		t.Fatal("换行不应被清除")
	}
}

func TestJoinLeaveChannel(t *testing.T) {
	svc, st, _ := newSvc(t)
	ctx := context.Background()
	owner := mkUser(t, st, "owner3")
	joiner := mkUser(t, st, "joiner")
	c, _ := svc.CreateChannel(ctx, owner, "频道", "")

	if _, err := svc.JoinChannel(ctx, joiner, c.Slug); err != nil {
		t.Fatal(err)
	}
	if err := svc.LeaveChannel(ctx, owner, c.ID); err == nil {
		t.Fatal("owner 不应能退出")
	}
	if err := svc.LeaveChannel(ctx, joiner, c.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := st.IsMember(ctx, c.ID, joiner.ID); ok {
		t.Fatal("退出后仍是成员")
	}
}

func TestSearchValidation(t *testing.T) {
	svc, st, _ := newSvc(t)
	ctx := context.Background()
	u := mkUser(t, st, "searcher")
	if _, err := svc.Search(ctx, u, "a", 10); err == nil {
		t.Fatal("单字符搜索应被拒绝")
	}
	if _, err := svc.Search(ctx, u, "有效关键词", 10); err != nil {
		t.Fatalf("正常搜索应成功: %v", err)
	}
}

type fakeBridge struct {
	enabled bool
	calls   []string
}

func (f *fakeBridge) Enabled() bool { return f.enabled }
func (f *fakeBridge) Enqueue(_ context.Context, chatID, msgID int64) {
	f.calls = append(f.calls, "enqueue")
}

func TestBridgeHookCalledOnSend(t *testing.T) {
	svc, st, _ := newSvc(t)
	ctx := context.Background()
	u := mkUser(t, st, "bridger")
	c, _ := svc.CreateChannel(ctx, u, "频道", "")

	fb := &fakeBridge{enabled: true}
	svc.SetBridge(fb)
	if _, err := svc.Send(ctx, u, SendInput{ChatID: c.ID, Body: "触发桥接"}); err != nil {
		t.Fatal(err)
	}
	if len(fb.calls) != 1 {
		t.Fatalf("桥接应被调用一次，得到 %d", len(fb.calls))
	}

	fb.calls = nil
	fb.enabled = false
	if _, err := svc.Send(ctx, u, SendInput{ChatID: c.ID, Body: "不再触发"}); err != nil {
		t.Fatal(err)
	}
	if len(fb.calls) != 0 {
		t.Fatal("禁用时不应调用桥接")
	}
}

func TestDeleteMessagePermission(t *testing.T) {
	svc, st, _ := newSvc(t)
	ctx := context.Background()
	a := mkUser(t, st, "author2")
	b := mkUser(t, st, "member2")
	c, _ := svc.CreateChannel(ctx, a, "频道", "")
	_ = st.AddMember(ctx, c.ID, b.ID, store.RoleMember)

	res, err := svc.Send(ctx, a, SendInput{ChatID: c.ID, Body: "待撤回"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteMessage(ctx, b, c.ID, res.Message.ID); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("他人删除应 403，得到 %v", err)
	}
	if err := svc.DeleteMessage(ctx, a, c.ID, res.Message.ID); err != nil {
		t.Fatalf("作者删除应成功: %v", err)
	}
}

func BenchmarkSendFanout(b *testing.B) {
	st, err := store.Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	hub := realtime.NewHub()
	svc := New(st, hub)
	ctx := context.Background()
	owner, _ := st.CreateUser(ctx, "benchowner", "", "h", false)
	c, _ := svc.CreateChannel(ctx, owner, "bench", "")
	for i := 0; i < 50; i++ {
		u, _ := st.CreateUser(ctx, "m"+string(rune('a'+i%26))+string(rune('a'+i/26)), "", "h", false)
		_ = st.AddMember(ctx, c.ID, u.ID, store.RoleMember)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Send(ctx, owner, SendInput{ChatID: c.ID, Body: "基准消息"}); err != nil {
			b.Fatal(err)
		}
	}
}

package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mkUser(t *testing.T, s *Store, name string) User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), name, name, "$argon2id$fake", false)
	if err != nil {
		t.Fatalf("创建用户 %s 失败: %v", name, err)
	}
	return u
}

func TestCreateUserUniqueness(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "alice", "", "h", false); err != nil {
		t.Fatal(err)
	}
	// 大小写应归一化后冲突。
	_, err := s.CreateUser(ctx, "ALICE", "", "h", false)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("期望 ErrConflict，得到 %v", err)
	}
}

func TestValidateUsername(t *testing.T) {
	valid := []string{"abc", "a_b-c", "user123", strings.Repeat("a", 32)}
	for _, u := range valid {
		if err := ValidateUsername(u); err != nil {
			t.Fatalf("%q 应合法: %v", u, err)
		}
	}
	invalid := []string{"ab", "", strings.Repeat("a", 33), "Alice", "a b", "a@b", "用户名"}
	for _, u := range invalid {
		if err := ValidateUsername(u); err == nil {
			t.Fatalf("%q 应非法", u)
		}
	}
}

func TestChannelAndMembership(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	alice := mkUser(t, s, "alice")
	bob := mkUser(t, s, "bob")

	c, err := s.CreateChannel(ctx, alice.ID, "产品", "讨论")
	if err != nil {
		t.Fatal(err)
	}
	ok, role, err := s.IsMember(ctx, c.ID, alice.ID)
	if err != nil || !ok || role != RoleOwner {
		t.Fatalf("创建者应为 owner，得到 ok=%v role=%s err=%v", ok, role, err)
	}
	if err := s.AddMember(ctx, c.ID, bob.ID, RoleMember); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, c.ID, bob.ID, RoleMember); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复加入应返回 ErrConflict，得到 %v", err)
	}
	ids, err := s.MemberIDs(ctx, c.ID)
	if err != nil || len(ids) != 2 {
		t.Fatalf("成员数应为 2，得到 %d (%v)", len(ids), err)
	}
	// slug 不可预测且不重复
	if len(c.Slug) < 8 || strings.Contains(c.Slug, " ") {
		t.Fatalf("slug 形态异常: %q", c.Slug)
	}
}

func TestEnsureDirectIsIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := mkUser(t, s, "aa")
	b := mkUser(t, s, "bb")

	c1, err := s.EnsureDirect(ctx, a.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := s.EnsureDirect(ctx, b.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c1.ID != c2.ID {
		t.Fatalf("私聊应复用同一会话: %d vs %d", c1.ID, c2.ID)
	}
	if _, err := s.EnsureDirect(ctx, a.ID, a.ID); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("自聊应被拒绝，得到 %v", err)
	}
}

func TestMessagePagination(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "pager")
	c, _ := s.CreateChannel(ctx, u.ID, "频道", "")

	var ids []int64
	for i := 0; i < 12; i++ {
		m, _, err := s.AppendMessage(ctx, Message{
			ChatID: c.ID, SenderID: u.ID, SenderName: "pager",
			Source: SourceWeb, Body: "消息" + strings.Repeat("x", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}

	page, err := s.ListMessages(ctx, ListParams{ChatID: c.ID, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 5 {
		t.Fatalf("期望 5 条，得到 %d", len(page))
	}
	// 正序返回，最新 5 条
	if page[4].ID != ids[11] || page[0].ID != ids[7] {
		t.Fatalf("分页边界不对: first=%d last=%d", page[0].ID, page[4].ID)
	}

	older, err := s.ListMessages(ctx, ListParams{ChatID: c.ID, Limit: 5, BeforeID: page[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 5 || older[4].ID != ids[6] {
		t.Fatalf("向前翻页边界不对: len=%d last=%d", len(older), older[len(older)-1].ID)
	}

	newer, err := s.ListMessages(ctx, ListParams{ChatID: c.ID, Limit: 10, AfterID: ids[9]})
	if err != nil {
		t.Fatal(err)
	}
	if len(newer) != 2 || newer[0].ID != ids[10] {
		t.Fatalf("增量拉取不对: %+v", newer)
	}
}

func TestAppendMessageIdempotentByClientID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "dupper")
	c, _ := s.CreateChannel(ctx, u.ID, "频道", "")

	m1, created, err := s.AppendMessage(ctx, Message{ChatID: c.ID, SenderID: u.ID, SenderName: "x", Body: "hi", ClientID: "k1"})
	if err != nil || !created {
		t.Fatalf("首次写入应成功: created=%v err=%v", created, err)
	}
	m2, created, err := s.AppendMessage(ctx, Message{ChatID: c.ID, SenderID: u.ID, SenderName: "x", Body: "hi", ClientID: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if created || m2.ID != m1.ID {
		t.Fatalf("重复 clientID 应返回同一条: created=%v id=%d/%d", created, m2.ID, m1.ID)
	}
}

func TestMessageValidation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "valider")
	c, _ := s.CreateChannel(ctx, u.ID, "频道", "")

	if _, _, err := s.AppendMessage(ctx, Message{ChatID: c.ID, Body: "   "}); err == nil {
		t.Fatal("空白消息应被拒绝")
	}
	long := strings.Repeat("字", MaxMessageLen+1)
	if _, _, err := s.AppendMessage(ctx, Message{ChatID: c.ID, Body: long}); err == nil {
		t.Fatal("超长消息应被拒绝")
	}
	if _, _, err := s.AppendMessage(ctx, Message{ChatID: c.ID, Body: strings.Repeat("字", MaxMessageLen)}); err != nil {
		t.Fatalf("边界长度应允许: %v", err)
	}
}

func TestSoftDeletePermissions(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	author := mkUser(t, s, "author")
	other := mkUser(t, s, "other")
	c, _ := s.CreateChannel(ctx, author.ID, "频道", "")
	if err := s.AddMember(ctx, c.ID, other.ID, RoleMember); err != nil {
		t.Fatal(err)
	}
	m, _, _ := s.AppendMessage(ctx, Message{ChatID: c.ID, SenderID: author.ID, SenderName: "author", Body: "hi"})

	if err := s.SoftDeleteMessage(ctx, m.ID, other.ID, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("非作者删除应 403，得到 %v", err)
	}
	if err := s.SoftDeleteMessage(ctx, m.ID, other.ID, true); err != nil {
		t.Fatalf("管理员删除应成功: %v", err)
	}
	if _, err := s.MessageByID(ctx, m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应查不到，得到 %v", err)
	}
}

func TestSearchRespectsMembership(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	alice := mkUser(t, s, "alice")
	bob := mkUser(t, s, "bob")
	out := mkUser(t, s, "outsider")

	c, _ := s.CreateChannel(ctx, alice.ID, "私密频道", "")
	_, _, _ = s.AppendMessage(ctx, Message{ChatID: c.ID, SenderID: alice.ID, SenderName: "alice", Body: "绝密关键词在这里"})

	hits, err := s.SearchMessages(ctx, alice.ID, "绝密关键词", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("成员应搜到 1 条，得到 %d", len(hits))
	}
	hits, err = s.SearchMessages(ctx, out.ID, "绝密关键词", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("非成员不应搜到，得到 %d", len(hits))
	}
	_ = bob
}

func TestSearchWildcardsAreEscaped(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "searcher")
	c, _ := s.CreateChannel(ctx, u.ID, "频道", "")
	_, _, _ = s.AppendMessage(ctx, Message{ChatID: c.ID, SenderID: u.ID, SenderName: "s", Body: "100% 完成"})

	// LIKE 通配符若不转义，'%' 会匹配一切。
	hits, err := s.SearchMessages(ctx, u.ID, "100%", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("应精确命中 1 条，得到 %d", len(hits))
	}
	hits, _ = s.SearchMessages(ctx, u.ID, "_", 10)
	if len(hits) != 0 {
		t.Fatalf("下划线不应作为通配符匹配全部，得到 %d", len(hits))
	}
}

func TestUnreadCounts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := mkUser(t, s, "sender")
	b := mkUser(t, s, "reader")
	c, _ := s.CreateChannel(ctx, a.ID, "频道", "")
	_ = s.AddMember(ctx, c.ID, b.ID, RoleMember)

	for i := 0; i < 3; i++ {
		if _, _, err := s.AppendMessage(ctx, Message{ChatID: c.ID, SenderID: a.ID, SenderName: "a", Body: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	chats, err := s.ListChats(ctx, b.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 || chats[0].Unread != 3 {
		t.Fatalf("未读应为 3，得到 %+v", chats)
	}

	if err := s.MarkRead(ctx, c.ID, b.ID, 1<<40); err != nil {
		t.Fatal(err)
	}
	chats, _ = s.ListChats(ctx, b.ID, 10)
	if chats[0].Unread != 0 {
		t.Fatalf("已读后未读应为 0，得到 %d", chats[0].Unread)
	}
	// 已读位置只前进
	if err := s.MarkRead(ctx, c.ID, b.ID, 1); err != nil {
		t.Fatal(err)
	}
	chats, _ = s.ListChats(ctx, b.ID, 10)
	if chats[0].LastReadID != 1<<40 {
		t.Fatalf("已读位置不应回退，得到 %d", chats[0].LastReadID)
	}
}

func TestConcurrentAppends(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "racer")
	c, _ := s.CreateChannel(ctx, u.ID, "并发", "")

	const workers, per = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*per)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < per; j++ {
				if _, _, err := s.AppendMessage(ctx, Message{
					ChatID: c.ID, SenderID: u.ID, SenderName: "racer", Body: "并发写入",
				}); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发写入出错: %v", err)
	}
	msgs, err := s.ListMessages(ctx, ListParams{ChatID: c.ID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != workers*per {
		t.Fatalf("消息数应为 %d，得到 %d", workers*per, len(msgs))
	}
}

func TestTelegramOutboxRetryBackoff(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "tg")
	c, _ := s.CreateChannel(ctx, u.ID, "频道", "")
	if err := s.LinkTelegram(ctx, c.ID, "-100123"); err != nil {
		t.Fatal(err)
	}
	m, _, _ := s.AppendMessage(ctx, Message{ChatID: c.ID, SenderID: u.ID, SenderName: "a", Body: "hi"})
	if err := s.EnqueueTelegram(ctx, c.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	// 同一消息重复入队不应产生第二条
	if err := s.EnqueueTelegram(ctx, c.ID, m.ID); err != nil {
		t.Fatal(err)
	}

	batch, err := s.ClaimTelegramBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 1 {
		t.Fatalf("队列应只有 1 条，得到 %d", len(batch))
	}
	// 失败后立即不可再取（退避生效）
	if err := s.RetryTelegram(ctx, batch[0].ID, "boom", 60, 8); err != nil {
		t.Fatal(err)
	}
	batch, _ = s.ClaimTelegramBatch(ctx, 10)
	if len(batch) != 0 {
		t.Fatalf("退避期内不应立即可取，得到 %d", len(batch))
	}
	// 达到上限后标记完成
	if err := s.RetryTelegram(ctx, batch2ID(s, ctx, c.ID, m.ID), "boom", 60, 1); err != nil {
		t.Fatal(err)
	}
}

func batch2ID(s *Store, ctx context.Context, chatID, msgID int64) int64 {
	var id int64
	_ = s.db.QueryRowContext(ctx, `SELECT id FROM tg_outbox WHERE message_id=?`, msgID).Scan(&id)
	return id
}

func TestTelegramLinkUnique(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "linker")
	c1, _ := s.CreateChannel(ctx, u.ID, "频道1", "")
	c2, _ := s.CreateChannel(ctx, u.ID, "频道2", "")

	if err := s.LinkTelegram(ctx, c1.ID, "-100"); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkTelegram(ctx, c2.ID, "-100"); !errors.Is(err, ErrConflict) {
		t.Fatalf("同群绑定两会话应冲突，得到 %v", err)
	}
	// 重新绑定覆盖
	if err := s.LinkTelegram(ctx, c1.ID, "-200"); err != nil {
		t.Fatal(err)
	}
	tg, ok, err := s.TGChatIDFor(ctx, c1.ID)
	if err != nil || !ok || tg != "-200" {
		t.Fatalf("重新绑定失败: tg=%s ok=%v err=%v", tg, ok, err)
	}
	id, ok, err := s.ChatIDForTG(ctx, "-100")
	if err != nil || ok {
		t.Fatalf("旧群应已解绑: id=%d ok=%v err=%v", id, ok, err)
	}
}

func BenchmarkAppendMessage(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "bench", "", "h", false)
	c, _ := s.CreateChannel(ctx, u.ID, "bench", "")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := s.AppendMessage(ctx, Message{
			ChatID: c.ID, SenderID: u.ID, SenderName: "bench", Body: "基准测试消息内容",
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkListMessages(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "bench2", "", "h", false)
	c, _ := s.CreateChannel(ctx, u.ID, "bench", "")
	for i := 0; i < 1000; i++ {
		_, _, _ = s.AppendMessage(ctx, Message{ChatID: c.ID, SenderID: u.ID, SenderName: "b", Body: "历史消息"})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.ListMessages(ctx, ListParams{ChatID: c.ID, Limit: 40}); err != nil {
			b.Fatal(err)
		}
	}
}

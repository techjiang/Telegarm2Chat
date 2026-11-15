// Package chat 编排会话、消息与推送，不含 HTTP 细节。
package chat

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/realtime"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/store"
)

// TelegramBridge 是桥接层的能力接口，便于测试替换。
type TelegramBridge interface {
	Enabled() bool
	Enqueue(ctx context.Context, chatID, messageID int64)
}

// Mention 描述一次 @ 提及。
type Mention struct {
	UserID   int64  `json:"userId"`
	Username string `json:"username"`
}

var mentionRe = regexp.MustCompile(`@([a-z0-9_-]{3,32})`)

// Service 是聊天业务入口。
type Service struct {
	st     *store.Store
	hub    *realtime.Hub
	bridge TelegramBridge
	now    func() time.Time

	// onMessage 在消息成功落库后回调，用于对接指标。
	onMessage func(source string)
}

// New 构造服务。
func New(st *store.Store, hub *realtime.Hub) *Service {
	return &Service{st: st, hub: hub, now: time.Now}
}

// OnMessage 注册落库回调。
func (s *Service) OnMessage(fn func(source string)) { s.onMessage = fn }

func (s *Service) countMessage(source string) {
	if s.onMessage != nil {
		s.onMessage(source)
	}
}

// SetBridge 注入 Telegram 桥，允许 nil。
func (s *Service) SetBridge(b TelegramBridge) { s.bridge = b }

// Store 暴露存储层给需要直查的 handler。
func (s *Service) Store() *store.Store { return s.st }

// Hub 暴露推送中枢。
func (s *Service) Hub() *realtime.Hub { return s.hub }

// CreateChannel 建频道。
func (s *Service) CreateChannel(ctx context.Context, owner store.User, title, topic string) (store.Chat, error) {
	c, err := s.st.CreateChannel(ctx, owner.ID, title, topic)
	if err != nil {
		return store.Chat{}, err
	}
	s.publishChatUpdate(ctx, c.ID)
	s.audit(ctx, owner.ID, "chat.create", c.Slug)
	return c, nil
}

// OpenDirect 打开与目标用户的私聊。
func (s *Service) OpenDirect(ctx context.Context, actor store.User, targetUsername string) (store.Chat, error) {
	target, err := s.st.UserByUsername(ctx, targetUsername)
	if err != nil {
		return store.Chat{}, err
	}
	c, err := s.st.EnsureDirect(ctx, actor.ID, target.ID)
	if err != nil {
		return store.Chat{}, err
	}
	s.publishChatUpdate(ctx, c.ID)
	return c, nil
}

// ChannelBySlug 取频道并校验可见性。
func (s *Service) ChannelBySlug(ctx context.Context, actor store.User, slug string) (store.Chat, error) {
	c, err := s.st.ChatBySlug(ctx, slug)
	if err != nil {
		return store.Chat{}, err
	}
	ok, _, err := s.st.IsMember(ctx, c.ID, actor.ID)
	if err != nil {
		return store.Chat{}, err
	}
	if !ok {
		return store.Chat{}, store.ErrForbidden
	}
	return c, nil
}

// JoinChannel 加入频道。
func (s *Service) JoinChannel(ctx context.Context, actor store.User, slug string) (store.Chat, error) {
	c, err := s.st.ChatBySlug(ctx, slug)
	if err != nil {
		return store.Chat{}, err
	}
	if c.Kind != store.KindChannel {
		return store.Chat{}, store.ErrInvalidTarget
	}
	// 已经是成员就静默返回：否则每次点「用频道号加入」都会多一条系统消息。
	if err := s.st.AddMember(ctx, c.ID, actor.ID, store.RoleMember); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return c, nil
		}
		return store.Chat{}, err
	}
	s.postSystem(ctx, c.ID, actor.DisplayName+" 加入了频道")
	s.publishChatUpdate(ctx, c.ID)
	return c, nil
}

// InviteMember 把目标用户拉进频道。只有频道成员能邀请，且只对频道生效：
// 私聊的成员集合由两个用户 ID 决定，让人往里塞第三个不合语义。
func (s *Service) InviteMember(ctx context.Context, actor store.User, chatID int64, username string) (store.User, error) {
	c, err := s.st.ChatByID(ctx, chatID)
	if err != nil {
		return store.User{}, err
	}
	if c.Kind != store.KindChannel {
		return store.User{}, store.ErrInvalidTarget
	}
	if ok, _, err := s.st.IsMember(ctx, chatID, actor.ID); err != nil {
		return store.User{}, err
	} else if !ok {
		return store.User{}, store.ErrForbidden
	}
	target, err := s.st.UserByUsername(ctx, strings.TrimPrefix(strings.TrimSpace(username), "@"))
	if err != nil {
		return store.User{}, err
	}
	if target.ID == actor.ID {
		return store.User{}, errors.New("不用邀请自己，你已经在频道里")
	}
	err = s.st.AddMember(ctx, chatID, target.ID, store.RoleMember)
	if errors.Is(err, store.ErrConflict) {
		return target, nil // 已经是成员，幂等返回
	}
	if err != nil {
		return store.User{}, err
	}
	s.postSystem(ctx, chatID, actor.DisplayName+" 邀请 "+target.DisplayName+" 加入了频道")
	s.publishChatUpdate(ctx, chatID)
	return target, nil
}

// LeaveChannel 退出频道，owner 不可退。
func (s *Service) LeaveChannel(ctx context.Context, actor store.User, chatID int64) error {
	ok, role, err := s.st.IsMember(ctx, chatID, actor.ID)
	if err != nil {
		return err
	}
	if !ok {
		return store.ErrNotFound
	}
	if role == store.RoleOwner {
		return errors.New("创建者不能退出频道，请先移交或删除")
	}
	if err := s.st.RemoveMember(ctx, chatID, actor.ID); err != nil {
		return err
	}
	s.postSystem(ctx, chatID, actor.DisplayName+" 退出了频道")
	s.publishChatUpdate(ctx, chatID)
	return nil
}

// ListChats 返回用户会话列表。
func (s *Service) ListChats(ctx context.Context, actor store.User) ([]store.Chat, error) {
	chats, err := s.st.ListChats(ctx, actor.ID, 100)
	if err != nil {
		return nil, err
	}
	return chats, nil
}

// Messages 游标分页读取消息。
func (s *Service) Messages(ctx context.Context, actor store.User, chatID int64, before, after int64, limit int) ([]store.Message, error) {
	if ok, _, err := s.st.IsMember(ctx, chatID, actor.ID); err != nil {
		return nil, err
	} else if !ok {
		return nil, store.ErrForbidden
	}
	return s.st.ListMessages(ctx, store.ListParams{ChatID: chatID, Limit: limit, BeforeID: before, AfterID: after})
}

// SendInput 是发消息的入参。
type SendInput struct {
	ChatID   int64
	Body     string
	ClientID string
}

// SendResult 返回落库消息与解析出的提及。
type SendResult struct {
	Message   store.Message
	Mentions  []Mention
	Duplicate bool
}

// Send 写入用户消息，推送给在线成员，并交给桥接层外发。
func (s *Service) Send(ctx context.Context, actor store.User, in SendInput) (SendResult, error) {
	ok, _, err := s.st.IsMember(ctx, in.ChatID, actor.ID)
	if err != nil {
		return SendResult{}, err
	}
	if !ok {
		return SendResult{}, store.ErrForbidden
	}

	m, created, err := s.st.AppendMessage(ctx, store.Message{
		ChatID:       in.ChatID,
		SenderID:     actor.ID,
		SenderName:   actor.DisplayName,
		SenderAvatar: actor.AvatarVersion,
		Source:       store.SourceWeb,
		Body:         sanitizeBody(in.Body),
		ClientID:     in.ClientID,
	})
	if err != nil {
		return SendResult{}, err
	}
	mentions := s.resolveMentions(ctx, in.ChatID, m.Body)
	res := SendResult{Message: m, Mentions: mentions, Duplicate: !created}
	if !created {
		return res, nil
	}

	s.countMessage(store.SourceWeb)
	members, err := s.st.MemberIDs(ctx, in.ChatID)
	if err != nil {
		slog.Warn("chat: 读取成员失败，降级为广播", "chat_id", in.ChatID, "err", err)
	}
	s.hub.ToUsers(members, realtime.Event{
		Type:   realtime.EventMessage,
		ChatID: in.ChatID,
		Data:   m,
		TS:     realtime.Now(),
	})
	_ = s.st.MarkRead(ctx, in.ChatID, actor.ID, m.ID)

	if s.bridge != nil && s.bridge.Enabled() {
		s.bridge.Enqueue(ctx, in.ChatID, m.ID)
	}
	return res, nil
}

// PostBridgeMessage 由 Telegram 桥调用，把外部消息写入并推送。
func (s *Service) PostBridgeMessage(ctx context.Context, chatID int64, senderName, body string) (store.Message, error) {
	m, created, err := s.st.AppendMessage(ctx, store.Message{
		ChatID:     chatID,
		SenderName: truncate(senderName, 64),
		Source:     store.SourceTelegram,
		Body:       sanitizeBody(body),
	})
	if err != nil || !created {
		return m, err
	}
	s.countMessage(store.SourceTelegram)
	members, err := s.st.MemberIDs(ctx, chatID)
	if err != nil {
		return m, nil
	}
	s.hub.ToUsers(members, realtime.Event{
		Type:   realtime.EventMessage,
		ChatID: chatID,
		Data:   m,
		TS:     realtime.Now(),
	})
	return m, nil
}

// DeleteMessage 软删除并广播撤回事件。
func (s *Service) DeleteMessage(ctx context.Context, actor store.User, chatID, messageID int64) error {
	if ok, _, err := s.st.IsMember(ctx, chatID, actor.ID); err != nil {
		return err
	} else if !ok {
		return store.ErrForbidden
	}
	if err := s.st.SoftDeleteMessage(ctx, messageID, actor.ID, actor.IsAdmin); err != nil {
		return err
	}
	members, err := s.st.MemberIDs(ctx, chatID)
	if err == nil {
		s.hub.ToUsers(members, realtime.Event{
			Type:   realtime.EventMessageDeleted,
			ChatID: chatID,
			Data:   map[string]int64{"id": messageID},
			TS:     realtime.Now(),
		})
	}
	s.audit(ctx, actor.ID, "message.delete", "")
	return nil
}

// MarkRead 更新已读位置并广播，用于多端同步未读。
func (s *Service) MarkRead(ctx context.Context, actor store.User, chatID, messageID int64) error {
	if ok, _, err := s.st.IsMember(ctx, chatID, actor.ID); err != nil {
		return err
	} else if !ok {
		return store.ErrForbidden
	}
	if err := s.st.MarkRead(ctx, chatID, actor.ID, messageID); err != nil {
		return err
	}
	s.hub.ToUsers([]int64{actor.ID}, realtime.Event{
		Type:   realtime.EventRead,
		ChatID: chatID,
		Data:   map[string]int64{"messageId": messageID},
		TS:     realtime.Now(),
	})
	return nil
}

// Search 全文检索用户可见消息。
func (s *Service) Search(ctx context.Context, actor store.User, q string, limit int) ([]store.SearchHit, error) {
	q = strings.TrimSpace(q)
	if len([]rune(q)) < 2 {
		return nil, errors.New("搜索关键词至少 2 个字符")
	}
	return s.st.SearchMessages(ctx, actor.ID, q, limit)
}

// Members 会话成员列表。
func (s *Service) Members(ctx context.Context, actor store.User, chatID int64) ([]store.User, error) {
	if ok, _, err := s.st.IsMember(ctx, chatID, actor.ID); err != nil {
		return nil, err
	} else if !ok {
		return nil, store.ErrForbidden
	}
	return s.st.Members(ctx, chatID)
}

// resolveMentions 把 @name 解析成用户，只保留会话成员：
// 否则 @ 会成为探测任意用户名是否存在的通道。
func (s *Service) resolveMentions(ctx context.Context, chatID int64, body string) []Mention {
	matches := mentionRe.FindAllStringSubmatch(body, 16)
	if len(matches) == 0 {
		return nil
	}
	memberIDs, err := s.st.MemberIDs(ctx, chatID)
	if err != nil {
		return nil
	}
	members := make(map[int64]struct{}, len(memberIDs))
	for _, id := range memberIDs {
		members[id] = struct{}{}
	}

	seen := make(map[string]struct{}, len(matches))
	var out []Mention
	for _, m := range matches {
		name := m[1]
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		u, err := s.st.UserByUsername(ctx, name)
		if err != nil {
			continue
		}
		if _, ok := members[u.ID]; !ok {
			continue
		}
		out = append(out, Mention{UserID: u.ID, Username: u.Username})
	}
	return out
}

func (s *Service) postSystem(ctx context.Context, chatID int64, body string) {
	m, created, err := s.st.AppendMessage(ctx, store.Message{
		ChatID:     chatID,
		SenderName: "系统",
		Source:     store.SourceSystem,
		Body:       body,
	})
	if err != nil || !created {
		return
	}
	if members, err := s.st.MemberIDs(ctx, chatID); err == nil {
		s.hub.ToUsers(members, realtime.Event{
			Type:   realtime.EventMessage,
			ChatID: chatID,
			Data:   m,
			TS:     realtime.Now(),
		})
	}
}

func (s *Service) publishChatUpdate(ctx context.Context, chatID int64) {
	c, err := s.st.ChatByID(ctx, chatID)
	if err != nil {
		return
	}
	members, err := s.st.MemberIDs(ctx, chatID)
	if err != nil {
		return
	}
	s.hub.ToUsers(members, realtime.Event{
		Type:   realtime.EventChat,
		ChatID: chatID,
		Data:   c,
		TS:     realtime.Now(),
	})
}

func (s *Service) audit(ctx context.Context, actorID int64, action, target string) {
	s.st.Audit(ctx, actorID, action, target, "")
}

// sanitizeBody 去掉控制字符（保留换行与制表），防御终端转义注入。
func sanitizeBody(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			continue
		case r == '\u2028' || r == '\u2029':
			b.WriteRune('\n')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

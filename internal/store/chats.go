package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Chat 是频道或私聊会话。
type Chat struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	Slug         string `json:"slug"`
	Title        string `json:"title"`
	Topic        string `json:"topic"`
	CreatedAt    int64  `json:"createdAt"`
	Archived     bool   `json:"archived"`
	Unread       int64  `json:"unread,omitempty"`
	LastReadID   int64  `json:"lastReadId,omitempty"`
	LastMessage  string `json:"lastMessage,omitempty"`
	LastActiveAt int64  `json:"lastActiveAt,omitempty"`
	// Peer 只在私聊里有值：对方的 id / 昵称 / 头像版本，前端据此显示对方头像。
	Peer *Peer `json:"peer,omitempty"`
}

// Peer 是私聊对面的那个人。
type Peer struct {
	ID            int64  `json:"id"`
	Username      string `json:"username"`
	DisplayName   string `json:"displayName"`
	AvatarVersion int64  `json:"avatarVersion,omitempty"`
}

const (
	KindChannel = "channel"
	KindDirect  = "direct"

	RoleOwner  = "owner"
	RoleMember = "member"

	// DirectSlug 生成私聊稳定 slug：d:<小ID>:<大ID>，保证双方唯一。
	directSlugPrefix = "d:"
)

// DirectSlug 返回两人私聊的稳定标识。
func DirectSlug(a, b int64) string {
	if a > b {
		a, b = b, a
	}
	return fmt.Sprintf("%s%d:%d", directSlugPrefix, a, b)
}

// CreateChannel 创建频道并把创建者设为 owner。
func (s *Store) CreateChannel(ctx context.Context, ownerID int64, title, topic string) (Chat, error) {
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 80 {
		return Chat{}, errors.New("频道名需为 1..80 字符")
	}
	if len(topic) > 240 {
		topic = topic[:240]
	}
	// slug 冲突概率极低，仍留三次重试兜底。
	for attempt := 0; ; attempt++ {
		c, err := s.createChat(ctx, KindChannel, "c-"+randomSlug(), title, topic, ownerID)
		if err == nil || !errors.Is(err, ErrConflict) || attempt >= 2 {
			return c, err
		}
	}
}

// EnsureDirect 幂等创建两人私聊。
func (s *Store) EnsureDirect(ctx context.Context, a, b int64) (Chat, error) {
	if a == b {
		return Chat{}, ErrInvalidTarget
	}
	slug := DirectSlug(a, b)
	if c, err := s.ChatBySlug(ctx, slug); err == nil {
		return c, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Chat{}, err
	}
	c, err := s.createChat(ctx, KindDirect, slug, "私聊", "", a)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return s.ChatBySlug(ctx, slug)
		}
		return Chat{}, err
	}
	if err := s.AddMember(ctx, c.ID, b, RoleMember); err != nil && !errors.Is(err, ErrConflict) {
		return Chat{}, err
	}
	return c, nil
}

func (s *Store) createChat(ctx context.Context, kind, slug, title, topic string, creator int64) (Chat, error) {
	now := unixNow()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Chat{}, fmt.Errorf("store: 开启事务: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO chats(kind, slug, title, topic, created_by, created_at) VALUES(?,?,?,?,?,?)`,
		kind, slug, title, topic, creator, now)
	if err != nil {
		if isUniqueViolation(err) {
			return Chat{}, ErrConflict
		}
		return Chat{}, fmt.Errorf("store: 创建会话: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Chat{}, fmt.Errorf("store: 读取会话 ID: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO chat_members(chat_id, user_id, role, joined_at) VALUES(?,?,?,?)`,
		id, creator, RoleOwner, now); err != nil {
		return Chat{}, fmt.Errorf("store: 添加成员: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Chat{}, fmt.Errorf("store: 提交事务: %w", err)
	}
	return Chat{ID: id, Kind: kind, Slug: slug, Title: title, Topic: topic, CreatedAt: now}, nil
}

// ChatBySlug 按 slug 取会话。
func (s *Store) ChatBySlug(ctx context.Context, slug string) (Chat, error) {
	return scanChat(s.db.QueryRowContext(ctx,
		`SELECT id, kind, slug, title, topic, created_at, archived FROM chats WHERE slug = ?`, slug))
}

// ChatByID 按 ID 取会话。
func (s *Store) ChatByID(ctx context.Context, id int64) (Chat, error) {
	return scanChat(s.db.QueryRowContext(ctx,
		`SELECT id, kind, slug, title, topic, created_at, archived FROM chats WHERE id = ?`, id))
}

// AddMember 添加成员，重复添加返回 ErrConflict。
func (s *Store) AddMember(ctx context.Context, chatID, userID int64, role string) error {
	if role != RoleOwner && role != RoleMember {
		return errors.New("store: 非法角色")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO chat_members(chat_id, user_id, role, joined_at) VALUES(?,?,?,?)`,
		chatID, userID, role, unixNow())
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: 添加成员: %w", err)
	}
	return nil
}

// RemoveMember 移除成员（owner 由上层限制）。
func (s *Store) RemoveMember(ctx context.Context, chatID, userID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM chat_members WHERE chat_id=? AND user_id=?`, chatID, userID)
	if err != nil {
		return fmt.Errorf("store: 移除成员: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// IsMember 返回用户是否在会话中及其角色。
func (s *Store) IsMember(ctx context.Context, chatID, userID int64) (bool, string, error) {
	var role string
	err := s.db.QueryRowContext(ctx,
		`SELECT role FROM chat_members WHERE chat_id=? AND user_id=?`, chatID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("store: 查询成员: %w", err)
	}
	return true, role, nil
}

// MemberIDs 返回会话全部成员 ID，用于定向推送。
func (s *Store) MemberIDs(ctx context.Context, chatID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id FROM chat_members WHERE chat_id=?`, chatID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询成员列表: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: 扫描成员: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListChats 返回用户加入的会话，带未读数与最后一条消息预览。
func (s *Store) ListChats(ctx context.Context, userID int64, limit int) ([]Chat, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.kind, c.slug, c.title, c.topic, c.created_at, c.archived,
		       m.last_read_id,
		       COALESCE((SELECT COUNT(*) FROM messages msg
		                 WHERE msg.chat_id=c.id AND msg.deleted_at=0 AND msg.id > m.last_read_id
		                   AND (msg.sender_id IS NULL OR msg.sender_id <> ?1)), 0) AS unread,
		       COALESCE((SELECT msg.body FROM messages msg
		                 WHERE msg.chat_id=c.id AND msg.deleted_at=0
		                 ORDER BY msg.id DESC LIMIT 1), '') AS last_body,
		       COALESCE((SELECT msg.created_at FROM messages msg
		                 WHERE msg.chat_id=c.id AND msg.deleted_at=0
		                 ORDER BY msg.id DESC LIMIT 1), 0) AS last_at,
		       peer.id, peer.username, peer.display_name,
		       COALESCE((SELECT av.updated_at FROM user_avatars av WHERE av.user_id = peer.id), 0)
		FROM chats c
		JOIN chat_members m ON m.chat_id=c.id AND m.user_id=?1
		LEFT JOIN chat_members pm
		       ON pm.chat_id = c.id AND c.kind = 'direct' AND pm.user_id <> ?1
		LEFT JOIN users peer ON peer.id = pm.user_id
		WHERE c.archived=0
		ORDER BY last_at DESC, c.id DESC
		LIMIT ?2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询会话列表: %w", err)
	}
	defer rows.Close()
	out := make([]Chat, 0, limit)
	for rows.Next() {
		var (
			c        Chat
			arch     int
			lastAt   int64
			peerID   sql.NullInt64
			peerName sql.NullString
			peerDisp sql.NullString
			peerAv   int64
		)
		if err := rows.Scan(&c.ID, &c.Kind, &c.Slug, &c.Title, &c.Topic, &c.CreatedAt, &arch,
			&c.LastReadID, &c.Unread, &c.LastMessage, &lastAt,
			&peerID, &peerName, &peerDisp, &peerAv); err != nil {
			return nil, fmt.Errorf("store: 扫描会话: %w", err)
		}
		if peerID.Valid {
			c.Peer = &Peer{ID: peerID.Int64, Username: peerName.String,
				DisplayName: peerDisp.String, AvatarVersion: peerAv}
		}
		c.Archived = arch == 1
		c.LastActiveAt = lastAt
		if len(c.LastMessage) > 120 {
			c.LastMessage = truncateRunes(c.LastMessage, 120)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkRead 更新已读位置，只允许前进。
func (s *Store) MarkRead(ctx context.Context, chatID, userID, messageID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_members SET last_read_id = MAX(last_read_id, ?)
		 WHERE chat_id=? AND user_id=?`, messageID, chatID, userID)
	if err != nil {
		return fmt.Errorf("store: 更新已读: %w", err)
	}
	return nil
}

// Members 返回成员详情（含用户名），用于会话详情展示。
func (s *Store) Members(ctx context.Context, chatID int64) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.username, u.display_name, u.is_admin, u.created_at,
		       COALESCE((SELECT av.updated_at FROM user_avatars av WHERE av.user_id = u.id), 0)
		FROM chat_members m JOIN users u ON u.id = m.user_id
		WHERE m.chat_id=? ORDER BY m.role='owner' DESC, u.username`, chatID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询成员: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var (
			u   User
			adm int
		)
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &adm, &u.CreatedAt, &u.AvatarVersion); err != nil {
			return nil, fmt.Errorf("store: 扫描成员: %w", err)
		}
		u.IsAdmin = adm == 1
		out = append(out, u)
	}
	return out, rows.Err()
}

func scanChat(row *sql.Row) (Chat, error) {
	var (
		c    Chat
		arch int
	)
	err := row.Scan(&c.ID, &c.Kind, &c.Slug, &c.Title, &c.Topic, &c.CreatedAt, &arch)
	if errors.Is(err, sql.ErrNoRows) {
		return Chat{}, ErrNotFound
	}
	if err != nil {
		return Chat{}, fmt.Errorf("store: 扫描会话: %w", err)
	}
	c.Archived = arch == 1
	return c, nil
}

const slugAlphabet = "abcdefghijkmnopqrstuvwxyz23456789"

// randomSlug 用 crypto/rand 生成不可预测的不重名后缀。
func randomSlug() string {
	const n = 10
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("store: 熵源不可用: " + err.Error())
	}
	for i := range buf {
		buf[i] = slugAlphabet[int(buf[i])%len(slugAlphabet)]
	}
	return string(buf)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

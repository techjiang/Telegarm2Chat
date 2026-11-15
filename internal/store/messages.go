package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// 消息来源。
const (
	SourceWeb      = "web"
	SourceTelegram = "telegram"
	SourceSystem   = "system"

	MaxMessageLen = 4000
)

// Message 是一条聊天消息。
type Message struct {
	ID         int64  `json:"id"`
	ChatID     int64  `json:"chatId"`
	SenderID   int64  `json:"senderId,omitempty"`
	SenderName string `json:"senderName"`
	Source     string `json:"source"`
	Body       string `json:"body"`
	ClientID   string `json:"clientId,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	// SenderAvatar 是发送者头像版本，0 表示没设过头像（前端回落到字母头像）。
	SenderAvatar int64 `json:"senderAvatar,omitempty"`
}

// senderAvatarExpr 作为 SELECT 里的附加列，拿到消息发送者的头像版本。
// 用相关子查询而不是 LEFT JOIN：messages 是热表，多一个 join 会更贵。
const senderAvatarExpr = `COALESCE((SELECT av.updated_at FROM user_avatars av
	WHERE av.user_id = messages.sender_id), 0)`

// AppendMessage 写入消息；同一 chat 内 client_id 重复时返回已存在的那条（幂等重试）。
func (s *Store) AppendMessage(ctx context.Context, m Message) (Message, bool, error) {
	m.Body = strings.TrimSpace(m.Body)
	if m.Body == "" {
		return Message{}, false, errors.New("消息内容不能为空")
	}
	if n := len([]rune(m.Body)); n > MaxMessageLen {
		return Message{}, false, fmt.Errorf("消息过长（%d > %d）", n, MaxMessageLen)
	}
	if len(m.ClientID) > 64 {
		m.ClientID = m.ClientID[:64]
	}
	if m.Source == "" {
		m.Source = SourceWeb
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = unixNow()
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages(chat_id, sender_id, sender_name, source, body, client_id, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		m.ChatID, nullableID(m.SenderID), m.SenderName, m.Source, m.Body, m.ClientID, m.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			existing, getErr := s.messageByClientID(ctx, m.ChatID, m.ClientID)
			if getErr != nil {
				return Message{}, false, getErr
			}
			return existing, false, nil
		}
		return Message{}, false, fmt.Errorf("store: 写入消息: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, false, fmt.Errorf("store: 读取消息 ID: %w", err)
	}
	m.ID = id
	return m, true, nil
}

func (s *Store) messageByClientID(ctx context.Context, chatID int64, clientID string) (Message, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, chat_id, COALESCE(sender_id,0), sender_name, source, body, client_id, created_at,
		        `+senderAvatarExpr+`
		 FROM messages WHERE chat_id=? AND client_id=?`, chatID, clientID)
	var m Message
	err := row.Scan(&m.ID, &m.ChatID, &m.SenderID, &m.SenderName, &m.Source, &m.Body, &m.ClientID,
		&m.CreatedAt, &m.SenderAvatar)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("store: 查询消息: %w", err)
	}
	return m, nil
}

// MessageByID 按 ID 取未删除消息。
func (s *Store) MessageByID(ctx context.Context, id int64) (Message, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, chat_id, COALESCE(sender_id,0), sender_name, source, body, client_id, created_at,
		        `+senderAvatarExpr+`
		 FROM messages WHERE id=? AND deleted_at=0`, id)
	var m Message
	err := row.Scan(&m.ID, &m.ChatID, &m.SenderID, &m.SenderName, &m.Source, &m.Body, &m.ClientID,
		&m.CreatedAt, &m.SenderAvatar)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("store: 查询消息: %w", err)
	}
	return m, nil
}

// ListParams 控制游标分页：BeforeID>0 向前翻页，AfterID>0 增量拉取。
type ListParams struct {
	ChatID   int64
	Limit    int
	BeforeID int64
	AfterID  int64
}

// ListMessages 返回按时间正序排列的消息页。
func (s *Store) ListMessages(ctx context.Context, p ListParams) ([]Message, error) {
	if p.Limit <= 0 {
		p.Limit = 40
	}
	if p.Limit > 200 {
		p.Limit = 200
	}
	var (
		rows *sql.Rows
		err  error
	)
	base := `SELECT id, chat_id, COALESCE(sender_id,0), sender_name, source, body, client_id, created_at,
	                ` + senderAvatarExpr + `
	        FROM messages WHERE chat_id=? AND deleted_at=0`
	switch {
	case p.AfterID > 0:
		rows, err = s.db.QueryContext(ctx, base+` AND id > ? ORDER BY id ASC LIMIT ?`,
			p.ChatID, p.AfterID, p.Limit)
	case p.BeforeID > 0:
		rows, err = s.db.QueryContext(ctx, base+` AND id < ? ORDER BY id DESC LIMIT ?`,
			p.ChatID, p.BeforeID, p.Limit)
	default:
		rows, err = s.db.QueryContext(ctx, base+` ORDER BY id DESC LIMIT ?`, p.ChatID, p.Limit)
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询消息列表: %w", err)
	}
	defer rows.Close()

	out := make([]Message, 0, p.Limit)
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChatID, &m.SenderID, &m.SenderName, &m.Source, &m.Body,
			&m.ClientID, &m.CreatedAt, &m.SenderAvatar); err != nil {
			return nil, fmt.Errorf("store: 扫描消息: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历消息: %w", err)
	}
	if p.AfterID == 0 {
		// 倒序取回，翻转成正序返回。
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

// SoftDeleteMessage 软删除消息，只有作者或管理员可删。
func (s *Store) SoftDeleteMessage(ctx context.Context, id, actorID int64, isAdmin bool) error {
	m, err := s.MessageByID(ctx, id)
	if err != nil {
		return err
	}
	if !isAdmin && m.SenderID != actorID {
		return ErrForbidden
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE messages SET deleted_at=? WHERE id=?`, unixNow(), id); err != nil {
		return fmt.Errorf("store: 删除消息: %w", err)
	}
	return nil
}

// SearchHit 是搜索结果，附带会话信息便于跳转。
type SearchHit struct {
	Message
	ChatTitle string `json:"chatTitle"`
	ChatKind  string `json:"chatKind"`
}

// SearchMessages 在用户可见会话内做全文检索。
//
// 策略：先用 FTS5（快，按词切分）拿结果；不足时用 LIKE 子串补足，
// 因为 FTS 对连续中文/无分隔符文本只能前缀命中，子串搜索会漏。
func (s *Store) SearchMessages(ctx context.Context, userID int64, query string, limit int) ([]SearchHit, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	hits, err := s.searchFTS(ctx, userID, query, limit)
	if err != nil || len(hits) >= limit {
		return hits, err
	}
	seen := make(map[int64]struct{}, len(hits))
	for _, h := range hits {
		seen[h.ID] = struct{}{}
	}
	extra, err := s.searchLike(ctx, userID, query, limit-len(hits), seen)
	if err != nil {
		return hits, nil // 回退失败不影响 FTS 结果
	}
	return append(hits, extra...), nil
}

func (s *Store) searchFTS(ctx context.Context, userID int64, query string, limit int) ([]SearchHit, error) {
	match := sanitizeFTSQuery(query)
	if match == "" {
		return []SearchHit{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.chat_id, COALESCE(m.sender_id,0), m.sender_name, m.source, m.body,
		       m.client_id, m.created_at, c.title, c.kind
		FROM messages_fts f
		JOIN messages m ON m.id = f.rowid
		JOIN chats c ON c.id = m.chat_id
		JOIN chat_members cm ON cm.chat_id = m.chat_id AND cm.user_id = ?1
		WHERE messages_fts MATCH ?2 AND m.deleted_at = 0
		ORDER BY m.id DESC
		LIMIT ?3`, userID, match, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 全文检索: %w", err)
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.ID, &h.ChatID, &h.SenderID, &h.SenderName, &h.Source, &h.Body,
			&h.ClientID, &h.CreatedAt, &h.ChatTitle, &h.ChatKind); err != nil {
			return nil, fmt.Errorf("store: 扫描检索结果: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历检索结果: %w", err)
	}
	if out == nil {
		out = []SearchHit{}
	}
	return out, nil
}

// searchLike 子串回退：只在用户可见会话内匹配，扫描量由窗口上限兜住。
func (s *Store) searchLike(ctx context.Context, userID int64, query string, limit int, seen map[int64]struct{}) ([]SearchHit, error) {
	if limit <= 0 {
		return []SearchHit{}, nil
	}
	// 转义 LIKE 通配符，避免用户输入变成模式匹配。
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.chat_id, COALESCE(m.sender_id,0), m.sender_name, m.source, m.body,
		       m.client_id, m.created_at, c.title, c.kind
		FROM messages m
		JOIN chats c ON c.id = m.chat_id
		JOIN chat_members cm ON cm.chat_id = m.chat_id AND cm.user_id = ?1
		WHERE m.deleted_at = 0 AND m.body LIKE '%' || ?2 || '%' ESCAPE '\'
		ORDER BY m.id DESC
		LIMIT ?3`, userID, esc, limit*3)
	if err != nil {
		return nil, fmt.Errorf("store: 子串检索: %w", err)
	}
	defer rows.Close()
	out := make([]SearchHit, 0, limit)
	for rows.Next() && len(out) < limit {
		var h SearchHit
		if err := rows.Scan(&h.ID, &h.ChatID, &h.SenderID, &h.SenderName, &h.Source, &h.Body,
			&h.ClientID, &h.CreatedAt, &h.ChatTitle, &h.ChatKind); err != nil {
			return nil, fmt.Errorf("store: 扫描检索结果: %w", err)
		}
		if _, dup := seen[h.ID]; dup {
			continue
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// sanitizeFTSQuery 把用户输入拆成前缀词并加引号，规避 FTS5 语法注入。
func sanitizeFTSQuery(q string) string {
	fields := strings.Fields(q)
	if len(fields) > 8 {
		fields = fields[:8]
	}
	var parts []string
	for _, f := range fields {
		clean := strings.Map(func(r rune) rune {
			switch r {
			case '"', '\'', '*', '(', ')', ':', '^', '-', '+', '|', ',', '\\':
				return -1
			}
			return r
		}, f)
		if clean == "" {
			continue
		}
		parts = append(parts, `"`+clean+`"*`)
	}
	return strings.Join(parts, " ")
}

func nullableID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

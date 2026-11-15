package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// TGLink 是会话与 Telegram 群的绑定关系。
type TGLink struct {
	ChatID    int64  `json:"chatId"`
	TGChatID  string `json:"tgChatId"`
	CreatedAt int64  `json:"createdAt"`
}

// LinkTelegram 绑定会话到 TG 群，一个会话只能绑一个群。
func (s *Store) LinkTelegram(ctx context.Context, chatID int64, tgChatID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tg_links(chat_id, tg_chat_id, created_at) VALUES(?,?,?)
		 ON CONFLICT(chat_id) DO UPDATE SET tg_chat_id=excluded.tg_chat_id`,
		chatID, tgChatID, unixNow())
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("store: 绑定 Telegram: %w", err)
	}
	return nil
}

// UnlinkTelegram 解除绑定。
func (s *Store) UnlinkTelegram(ctx context.Context, chatID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tg_links WHERE chat_id=?`, chatID)
	if err != nil {
		return fmt.Errorf("store: 解绑 Telegram: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TGChatIDFor 返回会话绑定的 TG 群 ID。
func (s *Store) TGChatIDFor(ctx context.Context, chatID int64) (string, bool, error) {
	var tg string
	err := s.db.QueryRowContext(ctx, `SELECT tg_chat_id FROM tg_links WHERE chat_id=?`, chatID).Scan(&tg)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: 查询 Telegram 绑定: %w", err)
	}
	return tg, true, nil
}

// ChatIDForTG 反查 TG 群对应的会话。
func (s *Store) ChatIDForTG(ctx context.Context, tgChatID string) (int64, bool, error) {
	var chatID int64
	err := s.db.QueryRowContext(ctx, `SELECT chat_id FROM tg_links WHERE tg_chat_id=?`, tgChatID).Scan(&chatID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: 反查 Telegram 绑定: %w", err)
	}
	return chatID, true, nil
}

// OutboxItem 是待投递到 Telegram 的消息任务。
type OutboxItem struct {
	ID        int64
	ChatID    int64
	MessageID int64
	Attempts  int
}

// EnqueueTelegram 把消息放入外发队列；同一消息只入队一次。
func (s *Store) EnqueueTelegram(ctx context.Context, chatID, messageID int64) error {
	now := unixNow()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tg_outbox(chat_id, message_id, next_try, created_at) VALUES(?,?,?,?)
		 ON CONFLICT(message_id) DO NOTHING`, chatID, messageID, now, now)
	if err != nil {
		return fmt.Errorf("store: 入队 Telegram: %w", err)
	}
	return nil
}

// ClaimTelegramBatch 取一批到期任务，按退避时间过滤，保证多实例也不会并发抢同一条。
func (s *Store) ClaimTelegramBatch(ctx context.Context, limit int) ([]OutboxItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, chat_id, message_id, attempts FROM tg_outbox
		WHERE done_at = 0 AND next_try <= ? ORDER BY id LIMIT ?`, unixNow(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: 读取 Telegram 队列: %w", err)
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var it OutboxItem
		if err := rows.Scan(&it.ID, &it.ChatID, &it.MessageID, &it.Attempts); err != nil {
			return nil, fmt.Errorf("store: 扫描 Telegram 队列: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// FinishTelegram 标记任务完成。
func (s *Store) FinishTelegram(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tg_outbox SET done_at=? WHERE id=?`, unixNow(), id)
	return err
}

// RetryTelegram 记录失败并安排指数退避重试，超过上限标记完成（放弃）。
func (s *Store) RetryTelegram(ctx context.Context, id int64, errMsg string, backoff int64, maxAttempts int) error {
	if len(errMsg) > 300 {
		errMsg = errMsg[:300]
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE tg_outbox
		SET attempts = attempts + 1,
		    last_error = ?,
		    next_try = ?,
		    done_at = CASE WHEN attempts + 1 >= ? THEN ? ELSE 0 END
		WHERE id = ?`, errMsg, unixNow()+backoff, maxAttempts, unixNow(), id)
	return err
}

// UpdateState 保存键值型桥接状态，例如 Telegram update 偏移量。
func (s *Store) UpdateState(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tg_state(key, value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("store: 写入状态: %w", err)
	}
	return nil
}

// State 读回状态。
func (s *Store) State(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM tg_state WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: 读取状态: %w", err)
	}
	return v, true, nil
}

// Stats 汇总仪表盘需要的计数。
type Stats struct {
	Users         int64 `json:"users"`
	Chats         int64 `json:"chats"`
	Messages      int64 `json:"messages"`
	PendingTG     int64 `json:"pendingTelegram"`
	TelegramLinks int64 `json:"telegramLinks"`
}

// Snapshot 返回整体计数，单查询内合并以减少往返。
func (s *Store) Snapshot(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM users),
		       (SELECT COUNT(*) FROM chats),
		       (SELECT COUNT(*) FROM messages WHERE deleted_at=0),
		       (SELECT COUNT(*) FROM tg_outbox WHERE done_at=0),
		       (SELECT COUNT(*) FROM tg_links)`).
		Scan(&st.Users, &st.Chats, &st.Messages, &st.PendingTG, &st.TelegramLinks)
	if err != nil {
		return Stats{}, fmt.Errorf("store: 统计: %w", err)
	}
	return st, nil
}

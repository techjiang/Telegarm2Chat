// Package store 封装 SQLite 持久化，启用 WAL、外键与忙等超时。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"time"

	_ "modernc.org/sqlite"
)

// 领域错误，供上层映射成 HTTP 状态码。
var (
	ErrNotFound      = errors.New("store: 记录不存在")
	ErrConflict      = errors.New("store: 唯一约束冲突")
	ErrForbidden     = errors.New("store: 无权访问")
	ErrUnauthorized  = errors.New("store: 凭据无效")
	ErrInvalidTarget = errors.New("store: 非法目标")
)

// Store 是并发安全的数据库句柄。
type Store struct {
	db *sql.DB
}

// Open 打开数据库并执行迁移。
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: 解析路径: %w", err)
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "temp_store(MEMORY)")
	dsn := "file:" + abs + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: 打开数据库: %w", err)
	}
	// WAL 下单写多读：限制并发连接，避免写锁抖动。
	db.SetMaxOpenConns(max(4, runtime.NumCPU()))
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: 连接数据库: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭底层连接。
func (s *Store) Close() error { return s.db.Close() }

// DB 暴露只读句柄，供健康检查等场景使用。
func (s *Store) DB() *sql.DB { return s.db }

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  username      TEXT    NOT NULL UNIQUE,
  display_name  TEXT    NOT NULL DEFAULT '',
  password_hash TEXT    NOT NULL,
  is_admin      INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  last_seen_at  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS chats (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  kind        TEXT    NOT NULL CHECK (kind IN ('channel','direct')),
  slug        TEXT    NOT NULL UNIQUE,
  title       TEXT    NOT NULL,
  topic       TEXT    NOT NULL DEFAULT '',
  created_by  INTEGER,
  created_at  INTEGER NOT NULL,
  archived    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS chat_members (
  chat_id    INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       TEXT    NOT NULL DEFAULT 'member' CHECK (role IN ('owner','member')),
  joined_at  INTEGER NOT NULL,
  last_read_id INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (chat_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_members_user ON chat_members(user_id);

CREATE TABLE IF NOT EXISTS messages (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  chat_id     INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  sender_id   INTEGER REFERENCES users(id) ON DELETE SET NULL,
  sender_name TEXT    NOT NULL,
  source      TEXT    NOT NULL CHECK (source IN ('web','telegram','system')),
  body        TEXT    NOT NULL,
  client_id   TEXT    NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  deleted_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_messages_chat ON messages(chat_id, id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_client
  ON messages(chat_id, client_id) WHERE client_id <> '';

CREATE TABLE IF NOT EXISTS tg_links (
  chat_id    INTEGER PRIMARY KEY REFERENCES chats(id) ON DELETE CASCADE,
  tg_chat_id TEXT    NOT NULL UNIQUE,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS tg_outbox (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  chat_id    INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  message_id INTEGER NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  next_try   INTEGER NOT NULL,
  last_error TEXT    NOT NULL DEFAULT '',
  done_at    INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_outbox_pending ON tg_outbox(done_at, next_try);
CREATE UNIQUE INDEX IF NOT EXISTS idx_outbox_msg ON tg_outbox(message_id);

CREATE TABLE IF NOT EXISTS tg_state (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- 头像单独存表：老库无需 ALTER，CREATE IF NOT EXISTS 即可升级。
-- 图片由前端缩放到 256px 内再上传，这里只做魔数与尺寸校验。
CREATE TABLE IF NOT EXISTS user_avatars (
  user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  mime       TEXT    NOT NULL,
  bytes      BLOB    NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_log (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  actor_id  INTEGER NOT NULL DEFAULT 0,
  action    TEXT    NOT NULL,
  target    TEXT    NOT NULL DEFAULT '',
  ip        TEXT    NOT NULL DEFAULT '',
  at        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_log(at DESC);

-- 全文检索：独立 FTS5 表，由触发器同步，避免外部内容表的一致性坑。
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
  body,
  content='messages',
  content_rowid='id',
  tokenize="unicode61 remove_diacritics 2"
);

CREATE TRIGGER IF NOT EXISTS messages_fts_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER IF NOT EXISTS messages_fts_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, body) VALUES('delete', old.id, old.body);
END;
CREATE TRIGGER IF NOT EXISTS messages_fts_au AFTER UPDATE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, body) VALUES('delete', old.id, old.body);
  INSERT INTO messages_fts(rowid, body) VALUES (new.id, new.body);
END;
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("store: 执行迁移: %w", err)
	}
	return nil
}

func unixNow() int64 { return time.Now().Unix() }

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

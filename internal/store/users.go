package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/auth"
)

// User 是可登录的账号。
type User struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	IsAdmin     bool   `json:"isAdmin"`
	CreatedAt   int64  `json:"createdAt"`
}

// ValidateUsername 只允许小写字母数字下划线与短横线，长度 3..32。
func ValidateUsername(name string) error {
	if len(name) < 3 || len(name) > 32 {
		return errors.New("用户名长度需在 3..32 之间")
	}
	for _, r := range name {
		if r > unicode.MaxASCII {
			return errors.New("用户名只允许字母、数字、下划线、短横线")
		}
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return errors.New("用户名只允许小写字母、数字、下划线、短横线")
		}
	}
	return nil
}

// NormalizeUsername 统一小写并去空格，避免大小写重复注册。
func NormalizeUsername(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// CountUsers 用于首个注册者自动成为管理员。
func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser 创建账号，用户名冲突返回 ErrConflict。
func (s *Store) CreateUser(ctx context.Context, username, displayName, passwordHash string, isAdmin bool) (User, error) {
	username = NormalizeUsername(username)
	if displayName == "" {
		displayName = username
	}
	if len(displayName) > 64 {
		displayName = displayName[:64]
	}
	admin := 0
	if isAdmin {
		admin = 1
	}
	now := unixNow()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users(username, display_name, password_hash, is_admin, created_at, last_seen_at)
		 VALUES(?,?,?,?,?,?)`, username, displayName, passwordHash, admin, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrConflict
		}
		return User{}, fmt.Errorf("store: 创建用户: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("store: 读取用户 ID: %w", err)
	}
	return User{ID: id, Username: username, DisplayName: displayName, IsAdmin: isAdmin, CreatedAt: now}, nil
}

// Authenticate 校验用户名口令，用户不存在时也消耗等量 CPU。
func (s *Store) Authenticate(ctx context.Context, username, password string) (User, error) {
	username = NormalizeUsername(username)
	var (
		u        User
		hash     string
		isAdmin  int
		lastSeen int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, display_name, password_hash, is_admin, created_at
		 FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &u.DisplayName, &hash, &isAdmin, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		auth.DummyVerify(password)
		return User{}, ErrUnauthorized
	}
	if err != nil {
		return User{}, fmt.Errorf("store: 查询用户: %w", err)
	}
	if !auth.VerifyPassword(hash, password) {
		return User{}, ErrUnauthorized
	}
	u.IsAdmin = isAdmin == 1
	_ = lastSeen
	_, _ = s.db.ExecContext(ctx, `UPDATE users SET last_seen_at = ? WHERE id = ?`, unixNow(), u.ID)
	return u, nil
}

// UserByID 按主键取用户。
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	var (
		u       User
		isAdmin int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, display_name, is_admin, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Username, &u.DisplayName, &isAdmin, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: 查询用户: %w", err)
	}
	u.IsAdmin = isAdmin == 1
	return u, nil
}

// UserByUsername 按用户名取用户。
func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	var (
		u       User
		isAdmin int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, display_name, is_admin, created_at FROM users WHERE username = ?`,
		NormalizeUsername(username)).
		Scan(&u.ID, &u.Username, &u.DisplayName, &isAdmin, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: 查询用户: %w", err)
	}
	u.IsAdmin = isAdmin == 1
	return u, nil
}

// SearchUsers 按用户名/昵称前缀检索，供 @ 补全。
func (s *Store) SearchUsers(ctx context.Context, q string, limit int) ([]User, error) {
	q = NormalizeUsername(q)
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, display_name, is_admin, created_at FROM users
		 WHERE username LIKE ?1 OR display_name LIKE ?1 ORDER BY username LIMIT ?2`,
		q+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("store: 检索用户: %w", err)
	}
	defer rows.Close()
	out := make([]User, 0, limit)
	for rows.Next() {
		var (
			u       User
			isAdmin int
		)
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &isAdmin, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: 扫描用户: %w", err)
		}
		u.IsAdmin = isAdmin == 1
		out = append(out, u)
	}
	return out, rows.Err()
}

// Audit 写入一条审计记录，失败不影响主流程。
func (s *Store) Audit(ctx context.Context, actorID int64, action, target, ip string) {
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO audit_log(actor_id, action, target, ip, at) VALUES(?,?,?,?,?)`,
		actorID, action, target, ip, unixNow())
}

// CreateUserAtomic 在同一事务内判定"是否首个用户"再插入，
// 避免并发注册时两个请求都看到 0 而双双成为管理员。
// makeFirstAdmin 为 true 时，只有表中确实没有用户才授予管理员。
func (s *Store) CreateUserAtomic(ctx context.Context, username, displayName, passwordHash string, makeFirstAdmin bool) (User, bool, error) {
	username = NormalizeUsername(username)
	if displayName == "" {
		displayName = username
	}
	if len([]rune(displayName)) > 64 {
		displayName = string([]rune(displayName)[:64])
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, false, fmt.Errorf("store: 开启事务: %w", err)
	}
	defer tx.Rollback()

	// IMMEDIATE 事务已持有写锁，这里的计数与插入不会被其他写者插入中间。
	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return User{}, false, fmt.Errorf("store: 统计用户: %w", err)
	}
	isAdmin := makeFirstAdmin && count == 0

	admin := 0
	if isAdmin {
		admin = 1
	}
	now := unixNow()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO users(username, display_name, password_hash, is_admin, created_at, last_seen_at)
		 VALUES(?,?,?,?,?,?)`, username, displayName, passwordHash, admin, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, false, ErrConflict
		}
		return User{}, false, fmt.Errorf("store: 创建用户: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, false, fmt.Errorf("store: 读取用户 ID: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if isUniqueViolation(err) {
			return User{}, false, ErrConflict
		}
		return User{}, false, fmt.Errorf("store: 提交事务: %w", err)
	}
	return User{ID: id, Username: username, DisplayName: displayName, IsAdmin: isAdmin, CreatedAt: now}, isAdmin, nil
}

// DeleteUser 删除账号，用于注册流程回滚等场景。
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: 删除用户: %w", err)
	}
	return nil
}

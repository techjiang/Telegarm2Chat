package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // 只取配置做尺寸校验
	_ "image/jpeg" // 同上
	_ "image/png"  // 同上
	"strings"
	"unicode/utf8"
)

// 头像约束：前端会先缩放到 256px 内，这里留出余量给没走前端的客户端。
const (
	MaxAvatarBytes = 256 << 10 // 256 KiB
	MaxAvatarSide  = 512       // 单边像素上限
	MaxDisplayLen  = 32        // 昵称最大字符数
)

// ErrAvatarTooLarge 区分「体积过大」与「格式不对」，便于前端给准确提示。
var ErrAvatarTooLarge = errors.New("store: 头像过大")

// Avatar 是头像的原始字节与 MIME。
type Avatar struct {
	MIME      string
	Bytes     []byte
	UpdatedAt int64
}

// SetDisplayName 修改昵称，返回更新后的用户。
func (s *Store) SetDisplayName(ctx context.Context, userID int64, name string) (User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return User{}, errors.New("昵称不能为空")
	}
	if utf8.RuneCountInString(name) > MaxDisplayLen {
		return User{}, fmt.Errorf("昵称最多 %d 个字符", MaxDisplayLen)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE users SET display_name=? WHERE id=?`, name, userID)
	if err != nil {
		return User{}, fmt.Errorf("store: 更新昵称: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrNotFound
	}
	return s.UserByID(ctx, userID)
}

// SetAvatar 校验并保存头像。用魔数判定真实类型，不信任客户端声明的 MIME，
// 否则一个声明 image/png 的 HTML 文件就能变成同源存储型 XSS 的载体。
func (s *Store) SetAvatar(ctx context.Context, userID int64, raw []byte) (Avatar, error) {
	if len(raw) == 0 {
		return Avatar{}, errors.New("头像内容为空")
	}
	if len(raw) > MaxAvatarBytes {
		return Avatar{}, ErrAvatarTooLarge
	}
	mime := detectImageMIME(raw)
	if mime == "" {
		return Avatar{}, errors.New("只支持 PNG、JPEG、GIF、WebP 图片")
	}
	// 解析宽高，顺手挡掉「像素炸弹」：几 KB 的文件解压后可能是上亿像素。
	w, h, err := imageDimensions(raw, mime)
	if err != nil {
		return Avatar{}, errors.New("图片无法解析，请换一张")
	}
	if w <= 0 || h <= 0 || w > MaxAvatarSide || h > MaxAvatarSide {
		return Avatar{}, fmt.Errorf("图片尺寸需在 %d×%d 以内", MaxAvatarSide, MaxAvatarSide)
	}

	// 版本号严格递增：同一秒内连传两张图时，秒级时间戳不变会让浏览器
	// 命中旧缓存（?v= 没变），看起来像"上传没生效"。
	now := unixNow()
	var prev int64
	_ = s.db.QueryRowContext(ctx, `SELECT updated_at FROM user_avatars WHERE user_id=?`, userID).Scan(&prev)
	if now <= prev {
		now = prev + 1
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO user_avatars(user_id, mime, bytes, updated_at) VALUES(?,?,?,?)
		 ON CONFLICT(user_id) DO UPDATE SET mime=excluded.mime, bytes=excluded.bytes,
		   updated_at=excluded.updated_at`,
		userID, mime, raw, now); err != nil {
		return Avatar{}, fmt.Errorf("store: 保存头像: %w", err)
	}
	return Avatar{MIME: mime, Bytes: raw, UpdatedAt: now}, nil
}

// AvatarByUserID 取头像；没有时返回 ErrNotFound。
func (s *Store) AvatarByUserID(ctx context.Context, userID int64) (Avatar, error) {
	var a Avatar
	err := s.db.QueryRowContext(ctx,
		`SELECT mime, bytes, updated_at FROM user_avatars WHERE user_id=?`, userID).
		Scan(&a.MIME, &a.Bytes, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Avatar{}, ErrNotFound
	}
	if err != nil {
		return Avatar{}, fmt.Errorf("store: 查询头像: %w", err)
	}
	return a, nil
}

// DeleteAvatar 删除头像，用于「恢复默认字母头像」。
func (s *Store) DeleteAvatar(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM user_avatars WHERE user_id=?`, userID); err != nil {
		return fmt.Errorf("store: 删除头像: %w", err)
	}
	return nil
}

// AvatarVersions 批量返回各用户头像的更新时间，供消息列表附带 ?v= 破坏缓存。
func (s *Store) AvatarVersions(ctx context.Context, userIDs []int64) (map[int64]int64, error) {
	out := make(map[int64]int64, len(userIDs))
	for _, id := range userIDs {
		var at int64
		err := s.db.QueryRowContext(ctx,
			`SELECT updated_at FROM user_avatars WHERE user_id=?`, id).Scan(&at)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("store: 查询头像版本: %w", err)
		}
		out[id] = at
	}
	return out, nil
}

// detectImageMIME 用魔数识别图片类型，只放行这四种。
func detectImageMIME(b []byte) string {
	switch {
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// imageDimensions 取图片宽高。PNG/JPEG/GIF 交给标准库；WebP 标准库没有解码器，
// 这里按 RIFF 头手工解析，避免为了一个尺寸校验引第三方依赖。
func imageDimensions(raw []byte, mime string) (int, int, error) {
	if mime != "image/webp" {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			return 0, 0, err
		}
		return cfg.Width, cfg.Height, nil
	}
	return webpDimensions(raw)
}

func webpDimensions(b []byte) (int, int, error) {
	if len(b) < 30 {
		return 0, 0, errors.New("webp: 头部不完整")
	}
	switch string(b[12:16]) {
	case "VP8X": // 扩展格式：画布宽高为 24 位小端「实际值 - 1」
		w := int(uint32(b[24])|uint32(b[25])<<8|uint32(b[26])<<16) + 1
		h := int(uint32(b[27])|uint32(b[28])<<8|uint32(b[29])<<16) + 1
		return w, h, nil
	case "VP8 ": // 有损：13 位宽高，位于帧头之后
		if len(b) < 30 || b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0, errors.New("webp: 帧头损坏")
		}
		w := int(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff)
		h := int(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff)
		return w, h, nil
	case "VP8L": // 无损：14 位宽高交错存放
		if b[20] != 0x2f {
			return 0, 0, errors.New("webp: 无损头损坏")
		}
		bits := binary.LittleEndian.Uint32(b[21:25])
		return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1, nil
	}
	return 0, 0, errors.New("webp: 未知子格式")
}

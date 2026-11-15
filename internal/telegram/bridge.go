package telegram

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/store"
)

// PostFunc 由业务层注入，把 TG 消息写进会话并推送。
type PostFunc func(ctx context.Context, chatID int64, senderName, body string) error

// Bridge 承接 Telegram 双向同步：入站长轮询 + 出站队列投递。
type Bridge struct {
	client *Client
	st     *store.Store
	post   PostFunc
	poll   time.Duration

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

const (
	stateKeyOffset = "tg.offset"
	maxAttempts    = 8
	maxTextLen     = 4000
)

// NewBridge 创建桥。
func NewBridge(client *Client, st *store.Store, poll time.Duration, post PostFunc) *Bridge {
	if poll <= 0 || poll > 50*time.Second {
		poll = 25 * time.Second
	}
	return &Bridge{client: client, st: st, post: post, poll: poll}
}

// Enabled 汇报是否配置了 token。
func (b *Bridge) Enabled() bool { return b != nil && b.client != nil }

// Start 启动入站与出站两个循环。
func (b *Bridge) Start(ctx context.Context) {
	ctx, b.cancel = context.WithCancel(ctx)
	b.wg.Add(2)
	go b.pollLoop(ctx)
	go b.outboxLoop(ctx)
}

// Stop 等待循环退出。
func (b *Bridge) Stop(ctx context.Context) {
	if b.cancel != nil {
		b.cancel()
	}
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("telegram: 关闭超时")
	}
}

// Enqueue 把消息排入外发队列（未绑定 TG 群的会话直接跳过）。
func (b *Bridge) Enqueue(ctx context.Context, chatID, messageID int64) {
	if !b.Enabled() {
		return
	}
	if _, ok, err := b.st.TGChatIDFor(ctx, chatID); err != nil || !ok {
		return
	}
	if err := b.st.EnqueueTelegram(ctx, chatID, messageID); err != nil {
		slog.Warn("telegram: 入队失败", "chat_id", chatID, "message_id", messageID, "err", err)
	}
}

// pollLoop 处理入站更新；网络错误时指数退避，避免打爆 API。
func (b *Bridge) pollLoop(ctx context.Context) {
	defer b.wg.Done()
	offset, _, _ := b.st.State(ctx, stateKeyOffset)
	var off int64
	if v, err := strconv.ParseInt(offset, 10, 64); err == nil {
		off = v
	}
	backoff := time.Second

	for {
		if ctx.Err() != nil {
			return
		}
		updates, err := b.client.GetUpdates(ctx, off, b.poll)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var apiErr *APIError
			wait := backoff
			if errors.As(err, &apiErr) {
				if apiErr.RetryAfter > 0 {
					wait = apiErr.RetryAfter
				}
				if !apiErr.Retryable() && apiErr.Status != 409 && apiErr.Status != 401 {
					slog.Error("telegram: 轮询被拒绝，停止入站", "err", err)
					return
				}
			}
			slog.Warn("telegram: 轮询失败，稍后重试", "err", err, "wait", wait)
			sleepCtx(ctx, wait)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second

		for _, u := range updates {
			if u.UpdateID >= off {
				off = u.UpdateID + 1
			}
			b.handleUpdate(ctx, u)
		}
		if len(updates) > 0 {
			if err := b.st.UpdateState(ctx, stateKeyOffset, strconv.FormatInt(off, 10)); err != nil {
				slog.Warn("telegram: 保存 offset 失败", "err", err)
			}
		}
	}
}

func (b *Bridge) handleUpdate(ctx context.Context, u Update) {
	m := u.Message
	if m == nil || m.From == nil || m.From.IsBot {
		return
	}
	text := m.Text_()
	if text == "" {
		return
	}
	tgChat := strconv.FormatInt(m.Chat.ID, 10)
	chatID, ok, err := b.st.ChatIDForTG(ctx, tgChat)
	if err != nil {
		slog.Warn("telegram: 反查会话失败", "tg_chat", tgChat, "err", err)
		return
	}
	if !ok {
		return // 未绑定的群，静默忽略
	}
	if err := b.post(ctx, chatID, m.DisplayName(), text); err != nil {
		slog.Warn("telegram: 写入消息失败", "chat_id", chatID, "err", err)
	}
}

// outboxLoop 定期投递外发队列。
func (b *Bridge) outboxLoop(ctx context.Context) {
	defer b.wg.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.flush(ctx)
		}
	}
}

func (b *Bridge) flush(ctx context.Context) {
	items, err := b.st.ClaimTelegramBatch(ctx, 20)
	if err != nil {
		slog.Warn("telegram: 读取外发队列失败", "err", err)
		return
	}
	for _, it := range items {
		if ctx.Err() != nil {
			return
		}
		b.deliver(ctx, it)
	}
}

func (b *Bridge) deliver(ctx context.Context, it store.OutboxItem) {
	tgChat, ok, err := b.st.TGChatIDFor(ctx, it.ChatID)
	if err != nil || !ok {
		_ = b.st.FinishTelegram(ctx, it.ID)
		return
	}
	msg, err := b.st.MessageByID(ctx, it.MessageID)
	if err != nil {
		// 消息已被删除，放弃投递。
		_ = b.st.FinishTelegram(ctx, it.ID)
		return
	}
	text := msg.SenderName + ": " + msg.Body
	if r := []rune(text); len(r) > maxTextLen {
		text = string(r[:maxTextLen]) + "…"
	}

	_, err = b.client.SendMessage(ctx, tgChat, text)
	if err == nil {
		_ = b.st.FinishTelegram(ctx, it.ID)
		return
	}
	var apiErr *APIError
	backoff := int64(1 << min(it.Attempts, 6)) // 1s,2s,4s,...,64s
	if errors.As(err, &apiErr) {
		if apiErr.RetryAfter > 0 {
			backoff = int64(apiErr.RetryAfter / time.Second)
			if backoff < 1 {
				backoff = 1
			}
		}
		if !apiErr.Retryable() && apiErr.Status != 429 {
			slog.Warn("telegram: 不可重试错误，放弃投递", "message_id", it.MessageID, "err", err)
			_ = b.st.FinishTelegram(ctx, it.ID)
			return
		}
	}
	slog.Warn("telegram: 投递失败，安排重试", "message_id", it.MessageID, "attempt", it.Attempts+1, "err", err)
	if err := b.st.RetryTelegram(ctx, it.ID, err.Error(), backoff, maxAttempts); err != nil {
		slog.Warn("telegram: 记录重试失败", "err", err)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

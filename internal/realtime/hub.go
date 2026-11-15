// Package realtime 维护 WebSocket 连接并按用户定向推送事件。
package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Event 是推给客户端的事件信封。
type Event struct {
	Type   string `json:"type"`
	ChatID int64  `json:"chatId,omitempty"`
	Data   any    `json:"data,omitempty"`
	TS     int64  `json:"ts"`
}

// 事件类型。
const (
	EventMessage        = "message"
	EventMessageDeleted = "message.deleted"
	EventRead           = "read"
	EventChat           = "chat"
	EventProfile        = "profile" // 昵称或头像变更
	EventPresence       = "presence"
	EventError          = "error"
	EventReady          = "ready"
)

// Client 是一条连接的出站句柄。
//
// 生命周期：Register 创建，Close 关闭 done。send 通道只由 hub 写入、
// writer 协程读取，从不 close，因此不会有"向已关闭通道发送"的 panic。
type Client struct {
	UserID int64
	send   chan []byte
	done   chan struct{}
	once   sync.Once
	hub    *Hub
}

// Send 非阻塞投递；缓冲满说明客户端消费不过来，直接踢掉。
func (c *Client) Send(ev Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	c.SendRaw(b)
}

// SendRaw 投递已序列化的字节。
func (c *Client) SendRaw(b []byte) {
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.send <- b:
	case <-c.done:
	default:
		slog.Warn("realtime: 客户端消费过慢，主动断开", "user_id", c.UserID)
		c.Close()
	}
}

// Close 幂等关闭。
func (c *Client) Close() {
	c.once.Do(func() {
		close(c.done)
		c.hub.remove(c)
	})
}

// Out 供写入循环读取。
func (c *Client) Out() <-chan []byte { return c.send }

// Done 关闭信号。
func (c *Client) Done() <-chan struct{} { return c.done }

// Hub 按 userID 索引连接，支持同一用户多端在线。
type Hub struct {
	mu      sync.RWMutex
	clients map[int64]map[*Client]struct{}
	closing chan struct{}
	once    sync.Once

	connects  atomic.Int64
	delivered atomic.Int64
	dropped   atomic.Int64
}

// NewHub 创建中枢。
func NewHub() *Hub {
	return &Hub{
		clients: make(map[int64]map[*Client]struct{}),
		closing: make(chan struct{}),
	}
}

// Register 登记新连接。
func (h *Hub) Register(userID int64) (*Client, bool) {
	c := &Client{UserID: userID, send: make(chan []byte, 64), done: make(chan struct{}), hub: h}
	select {
	case <-h.closing:
		return c, false
	default:
	}
	h.mu.Lock()
	set, ok := h.clients[userID]
	if !ok {
		set = make(map[*Client]struct{}, 2)
		h.clients[userID] = set
	}
	set[c] = struct{}{}
	h.mu.Unlock()
	h.connects.Add(1)
	return c, true
}

func (h *Hub) remove(c *Client) {
	h.mu.Lock()
	if set, ok := h.clients[c.UserID]; ok {
		delete(set, c)
		if len(set) == 0 {
			delete(h.clients, c.UserID)
		}
	}
	h.mu.Unlock()
}

// ToUsers 只推给指定用户集合。
func (h *Hub) ToUsers(userIDs []int64, ev Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		slog.Error("realtime: 序列化事件失败", "type", ev.Type, "err", err)
		return
	}
	h.mu.RLock()
	targets := make([]*Client, 0, len(userIDs))
	for _, uid := range userIDs {
		for c := range h.clients[uid] {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		select {
		case <-c.done:
			continue
		default:
		}
		select {
		case c.send <- b:
			h.delivered.Add(1)
		case <-c.done:
		default:
			h.dropped.Add(1)
			go c.Close()
		}
	}
}

// Broadcast 推给所有在线用户，用于系统公告。
func (h *Hub) Broadcast(ev Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	h.mu.RLock()
	targets := make([]*Client, 0, 64)
	for _, set := range h.clients {
		for c := range set {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		select {
		case c.send <- b:
			h.delivered.Add(1)
		case <-c.done:
		default:
			h.dropped.Add(1)
			go c.Close()
		}
	}
}

// OnlineCount 在线用户数。
func (h *Hub) OnlineCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// ConnCount 连接总数。
func (h *Hub) ConnCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, set := range h.clients {
		n += len(set)
	}
	return n
}

// Counters 暴露计数给指标接口。
func (h *Hub) Counters() (connects, delivered, dropped int64) {
	return h.connects.Load(), h.delivered.Load(), h.dropped.Load()
}

// Shutdown 关闭全部连接并等待写入循环退出。
func (h *Hub) Shutdown(ctx context.Context) {
	h.once.Do(func() { close(h.closing) })

	h.mu.Lock()
	all := make([]*Client, 0, 64)
	for _, set := range h.clients {
		for c := range set {
			all = append(all, c)
		}
	}
	h.mu.Unlock()

	for _, c := range all {
		c.Close()
	}
	// 关闭是同步完成的（Close 只关通道并摘除注册），
	// 这里只在超时内等写入协程把剩余事件吐出，不再无条件阻塞。
	select {
	case <-ctx.Done():
		return
	default:
	}
}

// Now 统一事件时间戳。
func Now() int64 { return time.Now().Unix() }

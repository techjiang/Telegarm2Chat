package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/realtime"
)

const (
	wsWriteTimeout = 10 * time.Second
	wsReadLimit    = 16 << 10
	wsPingInterval = 25 * time.Second
)

type wsInbound struct {
	Type      string `json:"type"`
	ChatID    int64  `json:"chatId"`
	MessageID int64  `json:"messageId"`
}

// handleWS 建立实时连接：服务端只推事件，客户端可发 ping/read 指令。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "未认证")
		return
	}

	// 校验 Origin，避免跨站 WebSocket 劫持。
	opts := &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionContextTakeover,
	}
	if len(s.cfg.AllowedOrigins) > 0 {
		opts.OriginPatterns = s.cfg.AllowedOrigins
	} else {
		opts.OriginPatterns = []string{"*"}
		if !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "forbidden", "跨站连接被拒绝")
			return
		}
	}

	// 认证走 header 或 ?token=，Accept 前必须完成。
	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		slog.Warn("ws: 握手失败", "err", err)
		return
	}
	conn.SetReadLimit(wsReadLimit)

	client, registered := s.hub.Register(u.ID)
	if !registered {
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	client.Send(realtime.Event{
		Type: realtime.EventReady,
		Data: map[string]any{"userId": u.ID, "username": u.Username},
		TS:   realtime.Now(),
	})

	go s.wsWriter(ctx, conn, client)
	s.wsReader(ctx, conn, u.ID)

	client.Close()
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func (s *Server) wsWriter(ctx context.Context, conn *websocket.Conn, client *realtime.Client) {
	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-client.Done():
			return
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				client.Close()
				return
			}
		case b, ok := <-client.Out():
			if !ok {
				return
			}
			wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := conn.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				client.Close()
				return
			}
		}
	}
}

func (s *Server) wsReader(ctx context.Context, conn *websocket.Conn, userID int64) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var in wsInbound
		if err := json.Unmarshal(data, &in); err != nil {
			continue
		}
		switch in.Type {
		case "ping":
			// 心跳由协议层处理，这里无需响应。
		case "read":
			if in.ChatID > 0 && in.MessageID > 0 {
				u, err := s.svc.Store().UserByID(ctx, userID)
				if err == nil {
					_ = s.svc.MarkRead(ctx, u, in.ChatID, in.MessageID)
				}
			}
		}
	}
}

// sameOrigin 校验 Origin 与 Host 同源（缺省 Origin 时放行非浏览器客户端）。
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := parseOrigin(origin)
	if err != nil {
		return false
	}
	return u == r.Host
}

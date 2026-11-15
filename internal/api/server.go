// Package api 暴露 HTTP/WebSocket 接口。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/metrics"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/auth"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/chat"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/config"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/realtime"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/store"
)

// Server 组装路由与依赖。
type Server struct {
	cfg       *config.Config
	svc       *chat.Service
	issuer    *auth.TokenIssuer
	hub       *realtime.Hub
	rl        *rateLimiter
	authRL    *rateLimiter
	assets    http.Handler
	mux       *http.ServeMux
	startedAt time.Time

	reqTotal *atomic.Int64
	err5xx   *atomic.Int64
}

// Options 构造参数。
type Options struct {
	Config  *config.Config
	Service *chat.Service
	Issuer  *auth.TokenIssuer
	Hub     *realtime.Hub
	Assets  http.Handler
	// Metrics 可选；非 nil 时记录请求计数。
	Metrics *metrics.Registry
}

// New 构造 Server。
func New(o Options) *Server {
	s := &Server{
		cfg:       o.Config,
		svc:       o.Service,
		issuer:    o.Issuer,
		hub:       o.Hub,
		rl:        newRateLimiter(o.Config.RateLimitPerMin),
		authRL:    newRateLimiter(max(o.Config.RateLimitPerMin/4, 10)),
		assets:    o.Assets,
		mux:       http.NewServeMux(),
		startedAt: time.Now(),
	}
	if o.Metrics != nil {
		s.reqTotal = o.Metrics.Counter("t2c_http_requests_total", "HTTP 请求总数")
		s.err5xx = o.Metrics.Counter("t2c_http_5xx_total", "5xx 响应数")
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	m := s.mux

	// 公共
	m.HandleFunc("GET /healthz", s.handleHealth)
	m.HandleFunc("GET /readyz", s.handleReady)
	m.HandleFunc("GET /api/v1/meta", s.handleMeta)
	m.Handle("POST /api/v1/auth/register", s.authRL.middleware(http.HandlerFunc(s.handleRegister)))
	m.Handle("POST /api/v1/auth/login", s.authRL.middleware(http.HandlerFunc(s.handleLogin)))

	// 需登录
	authed := func(h http.HandlerFunc) http.Handler { return s.requireAuth(h) }
	m.Handle("GET /api/v1/me", authed(s.handleMe))
	m.Handle("POST /api/v1/auth/logout", authed(s.handleLogout))

	m.Handle("GET /api/v1/chats", authed(s.handleListChats))
	m.Handle("POST /api/v1/chats", authed(s.handleCreateChat))
	m.Handle("POST /api/v1/chats/join", authed(s.handleJoinChat))
	m.Handle("GET /api/v1/chats/{id}/messages", authed(s.handleListMessages))
	m.Handle("POST /api/v1/chats/{id}/messages", authed(s.handlePostMessage))
	m.Handle("DELETE /api/v1/chats/{id}/messages/{mid}", authed(s.handleDeleteMessage))
	m.Handle("POST /api/v1/chats/{id}/read", authed(s.handleMarkRead))
	m.Handle("GET /api/v1/chats/{id}/members", authed(s.handleMembers))
	m.Handle("POST /api/v1/chats/{id}/leave", authed(s.handleLeaveChat))
	m.Handle("POST /api/v1/chats/{id}/telegram", authed(s.handleLinkTelegram))
	m.Handle("DELETE /api/v1/chats/{id}/telegram", authed(s.handleUnlinkTelegram))

	m.Handle("POST /api/v1/direct", authed(s.handleDirect))
	m.Handle("GET /api/v1/users/search", authed(s.handleUserSearch))
	m.Handle("GET /api/v1/search", authed(s.handleSearch))
	m.Handle("GET /api/v1/stats", authed(s.handleStats))

	// WebSocket
	m.Handle("GET /api/v1/ws", authed(http.HandlerFunc(s.handleWS)))

	// 静态资源（前端 SPA）
	if s.assets != nil {
		m.Handle("/", s.assets)
	}
}

// Handler 返回带全局中间件的根 handler。
func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	h = maxBody(s.cfg.MaxBodyBytes)(h)
	h = s.rl.middleware(h)
	h = securityHeaders(h)
	h = recoverer(h)
	h = s.requestLog(h)
	h = s.withClientIP(h)
	return h
}

// withClientIP 在最外层解析客户端 IP，供限流与审计使用。
//
// 默认**不信任** X-Forwarded-For：否则任何客户端都能自带该头伪造 IP，
// 一个请求换一个头就绕过了按 IP 的限流。部署在反向代理后请显式设
// T2C_TRUST_PROXY=1，由代理负责覆盖写入该头。
func (s *Server) withClientIP(next http.Handler) http.Handler {
	trust := false
	switch strings.ToLower(strings.TrimSpace(getenv("T2C_TRUST_PROXY"))) {
	case "1", "true", "yes":
		trust = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxClientIP, realIP(r, trust))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ---- 响应工具 ----

type envelope struct {
	OK    bool    `json:"ok"`
	Data  any     `json:"data,omitempty"`
	Error *errObj `json:"error,omitempty"`
}

type errObj struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("api: 写出响应失败", "err", err)
	}
}

func writeData(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, envelope{OK: true, Data: data})
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, envelope{OK: false, Error: &errObj{Code: code, Message: msg}})
}

// mapErr 把领域错误映射成 HTTP 响应。
func mapErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "资源不存在")
	case errors.Is(err, store.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "无权访问该资源")
	case errors.Is(err, store.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized", "用户名或密码错误")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "资源已存在")
	case errors.Is(err, store.ErrInvalidTarget):
		writeError(w, http.StatusBadRequest, "invalid_target", "非法的操作目标")
	case errors.Is(err, auth.ErrWeakPassword):
		writeError(w, http.StatusBadRequest, "weak_password", err.Error())
	case errors.Is(err, auth.ErrInvalidToken):
		writeError(w, http.StatusUnauthorized, "unauthorized", "令牌无效")
	default:
		slog.Error("api: 未预期错误", "err", err)
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	}
}

// decodeJSON 严格解析：拒绝未知字段、限制体积、禁止尾随数据。
func decodeJSON[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid_request", "请求体为空")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_request", "请求体格式错误")
		}
		return v, false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "invalid_request", "请求体包含多余数据")
		return v, false
	}
	return v, true
}

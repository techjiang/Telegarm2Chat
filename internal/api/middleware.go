package api

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/store"
)

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxClientIP
)

// withUser 把已认证用户放进 context。
func withUser(ctx context.Context, u store.User) context.Context {
	return context.WithValue(ctx, ctxUser, u)
}

// userFrom 读取已认证用户。
func userFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(ctxUser).(store.User)
	return u, ok
}

// clientIP 从 context 取已解析的客户端 IP。
func clientIP(ctx context.Context) string {
	if v, ok := ctx.Value(ctxClientIP).(string); ok {
		return v
	}
	return ""
}

// realIP 解析真实客户端 IP。默认信任 X-Forwarded-For 第一跳，
// 因为本服务通常部署在反向代理之后；如需严格模式可设 T2C_TRUST_PROXY=0。
func realIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
				return ip.String()
			}
		}
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			if ip := net.ParseIP(strings.TrimSpace(xr)); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// securityHeaders 加一组防御性响应头。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; connect-src 'self' ws: wss:; base-uri 'none'; "+
				"form-action 'self'; frame-ancestors 'none'; object-src 'none'")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// maxBody 限制请求体大小。
func maxBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// recoverer 兜住 panic，避免单请求打挂进程。
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic 已被捕获", "path", r.URL.Path, "panic", rec)
				writeError(w, http.StatusInternalServerError, "internal_error", "服务内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requestLog 记录访问日志（不含敏感体）与请求计数。
func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if s.reqTotal != nil {
			s.reqTotal.Add(1)
		}
		if sw.status >= 500 && s.err5xx != nil {
			s.err5xx.Add(1)
		}
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"bytes", sw.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", clientIP(r.Context()))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status   int
	bytes    int
	hijacked bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.hijacked {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.hijacked {
		return 0, http.ErrHijacked
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap 让 http.ResponseController 能拿到底层 writer。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack 必须显式转发：coder/websocket 直接用类型断言取 http.Hijacker，
// 不会走 Unwrap。缺了它，WebSocket 握手会 501。
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	w.hijacked = true
	return hj.Hijack()
}

// Flush 转发，兼容 SSE 等流式响应。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// rateLimiter 是基于令牌桶的按 IP 限流器，无第三方依赖。
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // 每秒补充令牌
	burst   float64
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMinute int) *rateLimiter {
	if perMinute <= 0 {
		return nil
	}
	// burst 给到每分钟额度的 1/3（30..120），既容得下 SPA 首屏并发，
	// 又不至于让单 IP 长时间打满。
	burst := perMinute / 3
	if burst < 30 {
		burst = 30
	}
	if burst > 120 {
		burst = 120
	}
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    float64(perMinute) / 60,
		burst:   float64(burst),
		lastGC:  time.Now(),
	}
}

func (l *rateLimiter) allow(key string) bool {
	if l == nil {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens = min(l.burst, b.tokens+elapsed*l.rate)
	if b.tokens < 1 {
		l.gcLocked(now)
		return false
	}
	b.tokens--
	l.gcLocked(now)
	return true
}

// gcLocked 定期清理闲置桶，防止内存随 IP 数增长。
func (l *rateLimiter) gcLocked(now time.Time) {
	if now.Sub(l.lastGC) < time.Minute {
		return
	}
	l.lastGC = now
	for k, b := range l.buckets {
		if now.Sub(b.last) > 5*time.Minute {
			delete(l.buckets, k)
		}
	}
}

func (l *rateLimiter) middleware(next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r.Context())) {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "请求过于频繁，请稍后再试")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAuth 校验 Bearer token，并把用户放入 context。
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "缺少访问令牌")
			return
		}
		parsed, err := s.issuer.Parse(tok)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "令牌无效或已过期")
			return
		}
		u, err := s.svc.Store().UserByID(r.Context(), parsed.UserID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "用户不存在")
			return
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), u)))
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		// WebSocket 无法自定义头，退化为查询参数。
		return r.URL.Query().Get("token")
	}
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

package api

import (
	"context"
	"net/http"
	"time"
)

// contextWithTimeout 从请求上下文派生超时上下文。
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

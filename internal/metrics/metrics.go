// Package metrics 提供轻量 Prometheus 文本格式导出，不引第三方库。
package metrics

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry 收集计数器与直方图摘要。
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*atomic.Int64
	gauges   map[string]func() int64
	help     map[string]string
}

// New 创建注册表。
func New() *Registry {
	return &Registry{
		counters: make(map[string]*atomic.Int64),
		gauges:   make(map[string]func() int64),
		help:     make(map[string]string),
	}
}

// Counter 返回可自增的计数器。
func (r *Registry) Counter(name, help string) *atomic.Int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	var c atomic.Int64
	r.counters[name] = &c
	r.help[name] = help
	return &c
}

// Gauge 注册按需计算的瞬时值。
func (r *Registry) Gauge(name, help string, fn func() int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[name] = fn
	r.help[name] = help
}

// Handler 输出 Prometheus 文本格式。
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var b strings.Builder
		r.mu.RLock()
		defer r.mu.RUnlock()
		for name, c := range r.counters {
			if h := r.help[name]; h != "" {
				fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, h, name)
			}
			fmt.Fprintf(&b, "%s %d\n", name, c.Load())
		}
		for name, fn := range r.gauges {
			if h := r.help[name]; h != "" {
				fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, h, name)
			}
			fmt.Fprintf(&b, "%s %d\n", name, fn())
		}
		_, _ = w.Write([]byte(b.String()))
	})
}

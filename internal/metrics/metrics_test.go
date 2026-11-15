package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCounterAndGauge(t *testing.T) {
	r := New()
	c := r.Counter("t2c_ops_total", "操作数")
	c.Add(3)
	c.Add(2)
	var gaugeVal int64 = 7
	r.Gauge("t2c_queue", "队列长度", func() int64 { return gaugeVal })

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		"t2c_ops_total 5",
		"t2c_queue 7",
		"# TYPE t2c_ops_total counter",
		"# TYPE t2c_queue gauge",
		"# HELP t2c_ops_total 操作数",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("指标输出缺少 %q:\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type 错误: %s", ct)
	}
}

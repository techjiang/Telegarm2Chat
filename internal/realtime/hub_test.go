package realtime

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestRegisterAndDeliver(t *testing.T) {
	h := NewHub()
	c, ok := h.Register(1)
	if !ok {
		t.Fatal("注册失败")
	}
	defer c.Close()

	h.ToUsers([]int64{1}, Event{Type: EventMessage, ChatID: 9, Data: map[string]int{"id": 3}})

	select {
	case b := <-c.Out():
		var ev Event
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type != EventMessage || ev.ChatID != 9 {
			t.Fatalf("事件内容不符: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到事件")
	}
}

func TestNoCrossUserLeak(t *testing.T) {
	h := NewHub()
	a, _ := h.Register(1)
	b, _ := h.Register(2)
	defer a.Close()
	defer b.Close()

	h.ToUsers([]int64{1}, Event{Type: EventMessage})

	select {
	case <-a.Out():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("目标用户未收到")
	}
	select {
	case <-b.Out():
		t.Fatal("非目标用户收到了事件（越权推送）")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestMultiDeviceDelivery(t *testing.T) {
	h := NewHub()
	d1, _ := h.Register(7)
	d2, _ := h.Register(7)
	defer d1.Close()
	defer d2.Close()

	h.ToUsers([]int64{7}, Event{Type: EventRead})
	for i, d := range []*Client{d1, d2} {
		select {
		case <-d.Out():
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("设备 %d 未收到", i)
		}
	}
	if h.ConnCount() != 2 {
		t.Fatalf("连接数应为 2，得到 %d", h.ConnCount())
	}
	if h.OnlineCount() != 1 {
		t.Fatalf("在线用户应为 1，得到 %d", h.OnlineCount())
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	h := NewHub()
	c, _ := h.Register(1)
	c.Close()
	c.Close()
	c.Close()
	if h.ConnCount() != 0 {
		t.Fatalf("关闭后连接数应为 0，得到 %d", h.ConnCount())
	}
	// 关闭后再次推送不应 panic
	h.ToUsers([]int64{1}, Event{Type: EventMessage})
}

func TestSlowConsumerIsDisconnected(t *testing.T) {
	h := NewHub()
	c, _ := h.Register(1)
	defer c.Close()

	// 缓冲 64，写入 200 条必然触发踢下线
	for i := 0; i < 200; i++ {
		h.ToUsers([]int64{1}, Event{Type: EventMessage, ChatID: int64(i)})
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-c.Done():
			_, _, dropped := h.Counters()
			if dropped == 0 {
				t.Fatal("应记录丢弃计数")
			}
			return
		case <-deadline:
			t.Fatal("慢消费者未被断开")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestConcurrentSendAndClose(t *testing.T) {
	h := NewHub()
	for i := 0; i < 20; i++ {
		c, _ := h.Register(int64(i))
		var wg sync.WaitGroup
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func(id int64) {
				defer wg.Done()
				for k := 0; k < 50; k++ {
					h.ToUsers([]int64{id}, Event{Type: EventMessage})
				}
			}(int64(i))
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Close()
		}()
		wg.Wait()
	}
}

func TestShutdownClosesAll(t *testing.T) {
	h := NewHub()
	clients := make([]*Client, 5)
	for i := range clients {
		clients[i], _ = h.Register(int64(i))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	h.Shutdown(ctx)

	for i, c := range clients {
		select {
		case <-c.Done():
		case <-time.After(time.Second):
			t.Fatalf("客户端 %d 未关闭", i)
		}
	}
	if _, ok := h.Register(99); ok {
		t.Fatal("关闭后不应再接受注册")
	}
}

func BenchmarkToUsers(b *testing.B) {
	h := NewHub()
	for i := 0; i < 100; i++ {
		c, _ := h.Register(int64(i))
		defer c.Close()
		// 持续消费，避免被踢下线
		go func() {
			for range c.Out() {
			}
		}()
	}
	ids := make([]int64, 100)
	for i := range ids {
		ids[i] = int64(i)
	}
	ev := Event{Type: EventMessage, ChatID: 1, Data: map[string]string{"body": "hello"}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ToUsers(ids, ev)
	}
}

package notifications

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeRecorder 是并发安全的 ResponseWriter，供 SSE 测试在写入过程中读取。
type safeRecorder struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	hdr     http.Header
	status  int
	flushed chan struct{}
}

func newSafeRecorder() *safeRecorder {
	return &safeRecorder{hdr: http.Header{}, flushed: make(chan struct{}, 1)}
}

func (r *safeRecorder) Header() http.Header { return r.hdr }

func (r *safeRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *safeRecorder) WriteHeader(code int) {
	r.mu.Lock()
	r.status = code
	r.mu.Unlock()
}

func (r *safeRecorder) Flush() {
	select {
	case r.flushed <- struct{}{}:
	default:
	}
}

func (r *safeRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func waitContains(t *testing.T, r *safeRecorder, sub string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(r.body(), sub) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时，未出现 %q，当前内容 %q", sub, r.body())
}

func TestStreamHandshakeAndBroadcast(t *testing.T) {
	s := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/admin/notifications/stream", nil).WithContext(ctx)
	rec := newSafeRecorder()
	done := make(chan struct{})
	go func() {
		s.Stream(rec, req)
		close(done)
	}()

	waitContains(t, rec, "retry: 5000")
	if rec.status != http.StatusOK {
		t.Fatalf("SSE 应 200，得到 %d", rec.status)
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type 应为 text/event-stream: %s", rec.Header().Get("Content-Type"))
	}

	res := testWrite(t, s, writeInput{title: "实时推送"})
	item, _, _ := s.getNotification(context.Background(), res.ID)
	s.hub.broadcast(item)
	waitContains(t, rec, "event: notification")
	waitContains(t, rec, `"title":"实时推送"`)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("上下文取消后 Stream 应返回")
	}
	s.hub.mu.Lock()
	n := len(s.hub.clients)
	s.hub.mu.Unlock()
	if n != 0 {
		t.Fatalf("连接断开后订阅者应被移除，剩 %d", n)
	}
}

func TestHeartbeatFrame(t *testing.T) {
	frame := heartbeatFrame()
	if !strings.HasPrefix(frame, "event: heartbeat\ndata: {") || !strings.Contains(frame, "\"ts\":") || !strings.HasSuffix(frame, "\n\n") {
		t.Fatalf("心跳帧格式不符: %q", frame)
	}
}

// errWriter 模拟已断开的连接。
type errWriter struct{}

func (errWriter) Header() http.Header       { return http.Header{} }
func (errWriter) WriteHeader(int)           {}
func (errWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (errWriter) Flush()                    {}

func TestBroadcastRemovesFailedClient(t *testing.T) {
	hub := newSSEHub()
	c := &sseClient{w: errWriter{}, flusher: errWriter{}}
	hub.add(c)
	hub.broadcast(NotificationItem{ID: 1, Attachments: []AttachmentView{}})
	hub.mu.Lock()
	n := len(hub.clients)
	hub.mu.Unlock()
	if n != 0 {
		t.Fatalf("写失败的订阅者应被移除，剩 %d", n)
	}
}

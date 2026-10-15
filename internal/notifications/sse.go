package notifications

import (
	"net/http"
	"strconv"
	"sync"
)

// sseClient 是一个 SSE 订阅者：对同一连接的写入串行化，避免心跳与广播并发写。
type sseClient struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	closed  bool
}

// write 写一帧并 flush；连接已断返回 false，调用方据此移除订阅者。
func (c *sseClient) write(frame string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	if _, err := c.w.Write([]byte(frame)); err != nil {
		c.closed = true
		return false
	}
	c.flusher.Flush()
	return true
}

// addClient 标记该连接已结束，后续写入直接失败。
func (c *sseClient) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

// sseHub 是 SSE 订阅者集合：新通知入库后立即广播，连接断开必须移除，不泄漏订阅者。
type sseHub struct {
	mu      sync.Mutex
	clients map[*sseClient]struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{clients: map[*sseClient]struct{}{}}
}

func (h *sseHub) add(c *sseClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *sseHub) remove(c *sseClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	c.close()
}

// broadcast 向所有订阅者推送新通知帧；写失败的订阅者就地移除。
func (h *sseHub) broadcast(item NotificationItem) {
	frame := "event: notification\ndata: " + marshalNoEscape(item) + "\n\n"
	h.mu.Lock()
	clients := make([]*sseClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	for _, c := range clients {
		if !c.write(frame) {
			h.remove(c)
		}
	}
}

// heartbeatFrame 生成心跳帧（内容与新通知帧同构的 { ts }）。
func heartbeatFrame() string {
	return "event: heartbeat\ndata: {\"ts\":" + strconv.FormatInt(nowSeconds(), 10) + "}\n\n"
}

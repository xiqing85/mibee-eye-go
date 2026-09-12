package web

// SSE event channel (SPEC v1 §6): GET /api/events. Replaces the WebSocket
// control hub — events are pushed as `event: <type>\ndata: <json>\n\n`
// frames with a 15s keepalive comment.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

const sseKeepalive = 15 * time.Second

type sseClient struct {
	ch     chan []byte
	closed chan struct{}
	once   sync.Once
}

type sseHub struct {
	mu      sync.Mutex
	clients map[*sseClient]struct{}
	logger  *log.Logger
}

func newSSEHub(logger *log.Logger) *sseHub {
	return &sseHub{
		clients: make(map[*sseClient]struct{}),
		logger:  logger,
	}
}

// broadcast pushes one named event to every connected client. Slow clients
// drop frames (bounded channel), not the event loop.
func (h *sseHub) broadcast(event string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		h.logger.Printf("web: sse marshal %s failed: %v", event, err)
		return
	}
	frame := fmt.Appendf(nil, "event: %s\ndata: %s\n\n", event, data)

	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.ch <- frame:
		default: // drop for slow consumers
		}
	}
}

func (h *sseHub) add(c *sseClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *sseHub) remove(c *sseClient) {
	c.once.Do(func() { close(c.closed) })
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *sseHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// handleEvents streams SSE to the client until it disconnects. The session
// cookie authenticates the subscription via authRequired.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	client := &sseClient{ch: make(chan []byte, 16), closed: make(chan struct{})}
	s.hub.add(client)
	defer s.hub.remove(client)

	// Per the SSE spec a leading retry hint lets browsers reconnect fast.
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case frame := <-client.ch:
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// runKeepaliveLog periodically logs the SSE subscriber count (diagnostics).
func (h *sseHub) runLog(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := h.count(); n > 0 {
				h.logger.Printf("web: sse clients=%d", n)
			}
		}
	}
}

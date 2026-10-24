package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"docker-sync/internal/model"
)

// ssePing is the heartbeat interval required by the contract (": ping").
const ssePing = 25 * time.Second

// clientBuffer is how many pending events one client may lag behind before
// events are dropped for that client.
const clientBuffer = 64

// Hub broadcasts store events to every connected SSE client.
type Hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	closed  bool
	log     *slog.Logger
}

func newHub(log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{clients: make(map[chan []byte]struct{}), log: log}
}

// Publish serialises ev and fans it out to all subscribers without blocking.
func (h *Hub) Publish(ev model.Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	data, err := json.Marshal(ev)
	if err != nil {
		h.log.Warn("序列化事件失败", "type", ev.Type, "error", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for ch := range h.clients {
		select {
		case ch <- data:
		default:
			h.log.Debug("SSE 客户端处理过慢，丢弃一条事件", "type", ev.Type)
		}
	}
}

// subscribe registers one client; the returned channel is closed by Close.
func (h *Hub) subscribe() (chan []byte, func()) {
	ch := make(chan []byte, clientBuffer)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.clients[ch]; ok {
			delete(h.clients, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Close disconnects every client and refuses new subscriptions.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for ch := range h.clients {
		delete(h.clients, ch)
		close(ch)
	}
}

// handleEvents streams store events as server sent events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "当前连接不支持流式响应")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	events, unsubscribe := s.hub.subscribe()
	defer unsubscribe()

	ping := time.NewTicker(ssePing)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case data, ok := <-events:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "event: update\ndata: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

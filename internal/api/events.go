package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Event types sent on /api/events;
// EventSource clients ignore the types they don't listen for.
const (
	EventIngestCurrent   = "ingest.current"
	EventIngestFinished  = "ingest.finished"
	EventStagingChanged  = "staging.changed"
	EventPromoteProgress = "promote.progress"
	EventIndexerProgress = "indexer.progress"
)

// DefaultKeepalive is how often an idle stream gets a comment line, so
// proxies and phones keep it open and a dead client is noticed.
const DefaultKeepalive = 25 * time.Second

const (
	subscriberBuffer = 16   // frames a slow client may lag before it is dropped
	sseRetryMillis   = 3000 // EventSource reconnect delay
)

// Hub fans server-sent events out to every connected /api/events client.
type Hub struct {
	mu        sync.Mutex
	subs      map[chan []byte]struct{}
	closed    bool
	snapshots []func() (string, any, error)
}

func NewHub() *Hub { return &Hub{subs: map[chan []byte]struct{}{}} }

// frame encodes one event. json.Marshal escapes control characters, so
// the data is always a single line.
func frame(typ string, data any) ([]byte, error) {
	if typ == "" || strings.ContainsAny(typ, "\r\n") {
		return nil, fmt.Errorf("bad event type %q", typ)
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode %s event: %w", typ, err)
	}
	return []byte("event: " + typ + "\ndata: " + string(b) + "\n\n"), nil
}

// Publish sends one event to every subscriber without blocking. A
// subscriber whose buffer is full is dropped: its stream ends, and the
// browser reconnects and gets fresh snapshots.
func (h *Hub) Publish(typ string, data any) error {
	msg, err := frame(typ, data)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
	return nil
}

// AddSnapshot registers current state that every new subscriber receives
// right after connecting. fn must not block.
func (h *Hub) AddSnapshot(fn func() (typ string, data any, err error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.snapshots = append(h.snapshots, fn)
}

// subscribe returns a channel of frames, closed when the subscriber is
// dropped or the hub closes, and a func that unsubscribes (idempotent).
func (h *Hub) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, subscriberBuffer)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(ch)
		return ch, func() {}
	}
	h.subs[ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// snapshotFrames encodes every registered snapshot. Failing ones are
// skipped and reported together.
func (h *Hub) snapshotFrames() ([][]byte, error) {
	h.mu.Lock()
	fns := append([]func() (string, any, error)(nil), h.snapshots...)
	h.mu.Unlock()
	var out [][]byte
	var errs []error
	for _, fn := range fns {
		typ, data, err := fn()
		if err == nil {
			var b []byte
			if b, err = frame(typ, data); err == nil {
				out = append(out, b)
				continue
			}
		}
		errs = append(errs, err)
	}
	return out, errors.Join(errs...)
}

// Close ends every stream; later subscribers get a closed channel.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
}

// Len is the number of connected subscribers.
func (h *Hub) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// handleEvents streams the hub to one client until the client leaves,
// falls too far behind, or the server closes. A failed write or flush
// means the client has gone, so the stream just ends.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ch, unsubscribe := s.hub.subscribe()
	defer unsubscribe()
	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(b []byte) bool {
		if _, err := w.Write(b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write([]byte(fmt.Sprintf("retry: %d\n\n", sseRetryMillis))) {
		return
	}
	frames, err := s.hub.snapshotFrames()
	if err != nil {
		s.log.Error("event snapshot failed", "err", err)
	}
	for _, f := range frames {
		if !write(f) {
			return
		}
	}
	keepalive := time.NewTicker(s.keepalive)
	defer keepalive.Stop()
	for {
		select {
		case f, ok := <-ch:
			if !ok || !write(f) {
				return
			}
		case <-keepalive.C:
			if !write([]byte(": keepalive\n\n")) {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

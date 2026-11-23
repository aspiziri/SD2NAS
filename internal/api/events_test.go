package api

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sseFrames connects to url and delivers each frame: its lines joined by
// "\n", without the blank line that ends it. The channel closes when the
// stream ends; cancel stops the client.
func sseFrames(t *testing.T, url string) (<-chan string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		cancel()
		t.Fatalf("status %d", res.StatusCode)
	}
	frames := make(chan string, 64)
	go func() {
		defer close(frames)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		var lines []string
		for sc.Scan() {
			if sc.Text() == "" {
				select {
				case frames <- strings.Join(lines, "\n"):
				case <-ctx.Done(): // the test stopped reading; don't block forever
					return
				}
				lines = nil
				continue
			}
			lines = append(lines, sc.Text())
		}
	}()
	return frames, cancel
}

func nextFrame(t *testing.T, frames <-chan string) string {
	t.Helper()
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatal("stream ended")
		}
		return f
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return ""
}

// eventsServer serves s over real HTTP. s.Close runs before the test
// server closes (cleanups run in reverse), or ts.Close would wait for the
// open streams.
func eventsServer(t *testing.T, s *Server) string {
	t.Helper()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	t.Cleanup(s.Close)
	return ts.URL
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestHubPublishFanOut(t *testing.T) {
	h := NewHub()
	defer h.Close()
	a, _ := h.subscribe()
	b, _ := h.subscribe()
	if err := h.Publish("x", map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	const want = "event: x\ndata: {\"a\":1}\n\n"
	for name, ch := range map[string]<-chan []byte{"a": a, "b": b} {
		select {
		case got := <-ch:
			if string(got) != want {
				t.Errorf("%s got %q, want %q", name, got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s timed out", name)
		}
	}
}

func TestHubDropsSlowSubscriber(t *testing.T) {
	h := NewHub()
	defer h.Close()
	a, _ := h.subscribe()
	b, _ := h.subscribe()
	var count atomic.Int32
	go func() {
		for range b {
			count.Add(1)
		}
	}()
	for i := 0; i < 16; i++ {
		if err := h.Publish("x", i); err != nil {
			t.Fatal(err)
		}
	}
	pollUntil(t, "B to read 16 frames", func() bool { return count.Load() == 16 })
	if err := h.Publish("x", 16); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		if _, ok := <-a; !ok {
			t.Fatalf("A closed after %d frames, want 16", i)
		}
	}
	if _, ok := <-a; ok {
		t.Fatal("A should be closed after its buffer drained")
	}
	pollUntil(t, "B to read 17 frames", func() bool { return count.Load() == 17 })
	if n := h.Len(); n != 1 {
		t.Errorf("Len = %d, want 1", n)
	}
}

func TestHubClose(t *testing.T) {
	h := NewHub()
	ch, _ := h.subscribe()
	h.Close()
	if _, ok := <-ch; ok {
		t.Error("subscriber channel should be closed")
	}
	if n := h.Len(); n != 0 {
		t.Errorf("Len = %d, want 0", n)
	}
	if err := h.Publish("x", 1); err != nil {
		t.Errorf("Publish after Close: %v", err)
	}
	late, _ := h.subscribe()
	if _, ok := <-late; ok {
		t.Error("late subscriber channel should be closed")
	}
	h.Close()
}

func TestHubRejectsBadEvents(t *testing.T) {
	h := NewHub()
	defer h.Close()
	for name, fn := range map[string]func() error{
		"newline in type": func() error { return h.Publish("a\nb", 1) },
		"empty type":      func() error { return h.Publish("", 1) },
		"unencodable":     func() error { return h.Publish("x", func() {}) },
	} {
		if err := fn(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestEventsStream(t *testing.T) {
	s := New("test", nil)
	s.hub.AddSnapshot(func() (string, any, error) { return "snap", map[string]int{"n": 1}, nil })
	url := eventsServer(t, s) + "/api/events"

	t.Run("headers", func(t *testing.T) {
		res, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		for k, want := range map[string]string{
			"Content-Type":      "text/event-stream",
			"Cache-Control":     "no-store",
			"X-Accel-Buffering": "no",
		} {
			if got := res.Header.Get(k); got != want {
				t.Errorf("%s = %q, want %q", k, got, want)
			}
		}
	})

	frames, cancel := sseFrames(t, url)
	defer cancel()
	if got := nextFrame(t, frames); got != "retry: 3000" {
		t.Fatalf("first frame %q", got)
	}
	if got := nextFrame(t, frames); got != "event: snap\ndata: {\"n\":1}" {
		t.Fatalf("snapshot frame %q", got)
	}
	if err := s.hub.Publish("x", 2); err != nil {
		t.Fatal(err)
	}
	if got := nextFrame(t, frames); got != "event: x\ndata: 2" {
		t.Fatalf("live frame %q", got)
	}
}

func TestEventsKeepalive(t *testing.T) {
	s := New("test", nil)
	s.keepalive = 20 * time.Millisecond
	frames, cancel := sseFrames(t, eventsServer(t, s)+"/api/events")
	defer cancel()
	if got := nextFrame(t, frames); got != "retry: 3000" {
		t.Fatalf("first frame %q", got)
	}
	if got := nextFrame(t, frames); got != ": keepalive" {
		t.Fatalf("got %q, want a keepalive", got)
	}
}

func TestEventsSnapshotError(t *testing.T) {
	s := New("test", nil)
	buf := &syncBuffer{}
	s.SetLogger(slog.New(slog.NewTextHandler(buf, nil)))
	s.hub.AddSnapshot(func() (string, any, error) { return "", nil, errors.New("boom") })
	s.hub.AddSnapshot(func() (string, any, error) { return "ok", 1, nil })
	frames, cancel := sseFrames(t, eventsServer(t, s)+"/api/events")
	defer cancel()
	if got := nextFrame(t, frames); got != "retry: 3000" {
		t.Fatalf("first frame %q", got)
	}
	if got := nextFrame(t, frames); got != "event: ok\ndata: 1" {
		t.Fatalf("second frame %q", got)
	}
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("log %q does not mention boom", buf.String())
	}
}

func TestEventsClientDisconnect(t *testing.T) {
	s := New("test", nil)
	frames, cancel := sseFrames(t, eventsServer(t, s)+"/api/events")
	if got := nextFrame(t, frames); got != "retry: 3000" {
		t.Fatalf("first frame %q", got)
	}
	cancel()
	pollUntil(t, "the handler to unsubscribe", func() bool { return s.hub.Len() == 0 })
}

func TestServerCloseEndsStreams(t *testing.T) {
	s := New("test", nil)
	frames, cancel := sseFrames(t, eventsServer(t, s)+"/api/events")
	defer cancel()
	if got := nextFrame(t, frames); got != "retry: 3000" {
		t.Fatalf("first frame %q", got)
	}
	s.Close()
	select {
	case _, ok := <-frames:
		if ok {
			t.Fatal("unexpected frame after Close")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not end after Close")
	}
	if n := s.hub.Len(); n != 0 {
		t.Errorf("Len = %d, want 0", n)
	}
	s.Close()
}

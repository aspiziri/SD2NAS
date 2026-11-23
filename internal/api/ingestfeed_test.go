package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/ingest"
	"github.com/aspiziri/SD2NAS/internal/media/mediatest"
)

// newFeedServer is newIngestServer with the feed's ticker effectively off,
// so tests drive s.feed.tick() themselves.
func newFeedServer(t *testing.T, free func(string) (uint64, error)) (s *Server, card string) {
	t.Helper()
	cards, photos, config := t.TempDir(), t.TempDir(), t.TempDir()
	card, err := mediatest.CopyCard(filepath.Join("..", "..", "testdata"), "SONY_A7IV", cards)
	if err != nil {
		t.Fatal(err)
	}
	db, err := catalog.Open(filepath.Join(config, "sd2nas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	runner := ingest.NewRunner(ingest.Config{Catalog: db, PhotosRoot: photos, FreeSpace: free, Log: slog.New(slog.DiscardHandler)})
	m := ingest.NewManager(runner, cards)
	m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	s = New("test", nil)
	s.feedInterval = time.Hour
	s.MountIngest(m, db, time.Minute)
	t.Cleanup(s.Close)
	return s, card
}

// takeFrames drains whatever is buffered without blocking.
func takeFrames(ch <-chan []byte) []string {
	var out []string
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, string(f))
		default:
			return out
		}
	}
}

func parseFrame(t *testing.T, f string) (typ string, data json.RawMessage) {
	t.Helper()
	rest, ok := strings.CutPrefix(f, "event: ")
	if !ok {
		t.Fatalf("bad frame %q", f)
	}
	typ, rest, _ = strings.Cut(rest, "\n")
	d, ok := strings.CutPrefix(rest, "data: ")
	if !ok {
		t.Fatalf("bad frame %q", f)
	}
	return typ, json.RawMessage(strings.TrimSuffix(d, "\n\n"))
}

func TestFeedFinishedViaWatch(t *testing.T) {
	s, card := newFeedServer(t, plenty)
	ch, unsub := s.hub.subscribe()
	defer unsub()
	rec := post(t, s, "?wait=1", cardBody(card))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var posted viewForTest
	decodeInto(t, rec, &posted)
	s.feed.tick()
	frames := takeFrames(ch)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2: %q", len(frames), frames)
	}
	typ, data := parseFrame(t, frames[0])
	if typ != EventIngestCurrent || string(data) != `{"running":null,"queued":[]}` {
		t.Errorf("frame 0 = %s %s", typ, data)
	}
	typ, data = parseFrame(t, frames[1])
	if typ != EventIngestFinished {
		t.Fatalf("frame 1 type %q", typ)
	}
	var fin viewForTest
	if err := json.Unmarshal(data, &fin); err != nil {
		t.Fatal(err)
	}
	if fin.ID != posted.ID || fin.Status != "done" || fin.Counts.New != 2 {
		t.Errorf("finished = %+v, want id %d done with 2 new", fin, posted.ID)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, has := raw["progress"]; has {
		t.Error("ingest.finished carries progress")
	}
}

func TestFeedDedupes(t *testing.T) {
	s, _ := newFeedServer(t, plenty)
	ch, unsub := s.hub.subscribe()
	defer unsub()
	s.feed.tick()
	if n := len(takeFrames(ch)); n != 1 {
		t.Fatalf("first tick published %d frames, want 1", n)
	}
	s.feed.tick()
	if n := len(ch); n != 0 {
		t.Errorf("second tick published %d frames, want 0", n)
	}
}

func TestFeedProgressAndFinish(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	s, card := newFeedServer(t, func(string) (uint64, error) { <-release; return 1e12, nil })
	ch, unsub := s.hub.subscribe()
	defer unsub()
	rec := post(t, s, "", cardBody(card))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var posted viewForTest
	decodeInto(t, rec, &posted)
	type currentForTest struct {
		Running *viewForTest `json:"running"`
	}
	pollUntil(t, "the job to be classifying", func() bool {
		var cur currentForTest
		decodeInto(t, get(t, s, "/api/ingest/current"), &cur)
		return cur.Running != nil && cur.Running.Progress != nil && cur.Running.Progress.Phase == "classifying"
	})
	s.feed.tick()
	frames := takeFrames(ch)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1: %q", len(frames), frames)
	}
	typ, data := parseFrame(t, frames[0])
	var cur currentForTest
	if err := json.Unmarshal(data, &cur); err != nil {
		t.Fatal(err)
	}
	if typ != EventIngestCurrent || cur.Running == nil || cur.Running.Status != "running" ||
		cur.Running.Progress == nil || cur.Running.Progress.Phase != "classifying" {
		t.Errorf("frame = %s %s", typ, data)
	}

	open()
	pollUntil(t, "the ingest to be done", func() bool {
		var v viewForTest
		decodeInto(t, get(t, s, "/api/ingests/"+strconv.FormatInt(posted.ID, 10)), &v)
		return v.Status == "done"
	})
	s.feed.tick()
	frames = takeFrames(ch)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2: %q", len(frames), frames)
	}
	typ, data = parseFrame(t, frames[0])
	if typ != EventIngestCurrent || string(data) != `{"running":null,"queued":[]}` {
		t.Errorf("frame 0 = %s %s", typ, data)
	}
	typ, data = parseFrame(t, frames[1])
	var fin viewForTest
	if err := json.Unmarshal(data, &fin); err != nil {
		t.Fatal(err)
	}
	if typ != EventIngestFinished || fin.ID != posted.ID {
		t.Errorf("frame 1 = %s %s", typ, data)
	}
}

func TestFeedFinishedHook(t *testing.T) {
	s, card := newFeedServer(t, plenty)
	got := make(chan int64, 4)
	s.addFinishedHook(func(in catalog.Ingest) { got <- in.ID })
	rec := post(t, s, "?wait=1", cardBody(card))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var posted viewForTest
	decodeInto(t, rec, &posted)
	s.feed.tick()
	select {
	case id := <-got:
		if id != posted.ID {
			t.Errorf("hook got ingest %d, want %d", id, posted.ID)
		}
	default:
		t.Fatal("finished hook did not run")
	}
}

func TestEventsSnapshotIngestCurrent(t *testing.T) {
	s, _ := newIngestServer(t, plenty, time.Minute)
	frames, cancel := sseFrames(t, eventsServer(t, s)+"/api/events")
	defer cancel()
	if f := nextFrame(t, frames); f != "retry: 3000" {
		t.Fatalf("first frame %q", f)
	}
	if f, want := nextFrame(t, frames), "event: ingest.current\ndata: {\"running\":null,\"queued\":[]}"; f != want {
		t.Errorf("snapshot %q, want %q", f, want)
	}
}

func TestFeedLive(t *testing.T) {
	s, card := newIngestServer(t, plenty, time.Minute)
	ch, unsub := s.hub.subscribe()
	defer unsub()
	rec := post(t, s, "?wait=1", cardBody(card))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var posted viewForTest
	decodeInto(t, rec, &posted)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				t.Fatal("subscription closed")
			}
			typ, data := parseFrame(t, string(f))
			if typ != EventIngestFinished {
				continue
			}
			var fin viewForTest
			if err := json.Unmarshal(data, &fin); err != nil {
				t.Fatal(err)
			}
			if fin.ID != posted.ID {
				t.Fatalf("finished id %d, want %d", fin.ID, posted.ID)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for ingest.finished")
		}
	}
}

func TestServerCloseStopsFeed(t *testing.T) {
	s, _ := newIngestServer(t, plenty, time.Minute)
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Server.Close did not return")
	}
}

// A client that connects while a short job is running gets a running
// snapshot. If the job ends before the next tick, that tick must still
// publish the idle state, even though it equals what the feed last published.
func TestFeedSnapshotForcesRepublish(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	s, card := newFeedServer(t, func(string) (uint64, error) { <-release; return 1e12, nil })
	s.feed.tick() // the feed has published idle
	rec := post(t, s, "", cardBody(card))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var posted viewForTest
	decodeInto(t, rec, &posted)
	typ, data, err := s.feed.snapshot() // the client connects mid-job
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(data); typ != EventIngestCurrent || string(b) == `{"running":null,"queued":[]}` {
		t.Fatalf("snapshot %s %s should show the job in flight", typ, b)
	}
	open()
	pollUntil(t, "the ingest to be done", func() bool {
		var v viewForTest
		decodeInto(t, get(t, s, "/api/ingests/"+strconv.FormatInt(posted.ID, 10)), &v)
		return v.Status == "done"
	})
	ch, unsub := s.hub.subscribe()
	defer unsub()
	s.feed.tick()
	frames := takeFrames(ch)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want ingest.current and ingest.finished: %q", len(frames), frames)
	}
	if typ, data := parseFrame(t, frames[0]); typ != EventIngestCurrent || string(data) != `{"running":null,"queued":[]}` {
		t.Errorf("frame 0 = %s %s", typ, data)
	}
}

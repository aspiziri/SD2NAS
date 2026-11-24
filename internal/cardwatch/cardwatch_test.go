package cardwatch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/ingest"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	t      *testing.T
	root   string
	w      *Watcher
	logs   *syncBuf
	mounts map[string]bool
	mgr    *ingest.Manager

	mu      sync.Mutex
	reqs    []ingest.Request
	failErr error
}

func plenty(string) (uint64, error) { return 1e12, nil }

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := filepath.Join(t.TempDir(), "cards")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgDir := t.TempDir()
	photos := t.TempDir()
	db, err := catalog.Open(filepath.Join(cfgDir, "sd2nas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	runner := ingest.NewRunner(ingest.Config{Catalog: db, PhotosRoot: photos, FreeSpace: plenty, Log: slog.New(slog.DiscardHandler)})
	// Never started: jobs are recorded and queued, not run.
	mgr := ingest.NewManager(runner, root)
	h := &harness{t: t, root: root, logs: &syncBuf{}, mounts: map[string]bool{}, mgr: mgr}
	h.w = New(Config{
		Root: root,
		Submit: func(ctx context.Context, req ingest.Request) (*ingest.Job, error) {
			h.mu.Lock()
			h.reqs = append(h.reqs, req)
			fail := h.failErr
			h.mu.Unlock()
			if fail != nil {
				return nil, fail
			}
			return mgr.Submit(ctx, req)
		},
		Log: slog.New(slog.NewTextHandler(h.logs, nil)),
	})
	h.w.isMount = func(dir, _ string) (bool, error) { return h.mounts[filepath.Base(dir)], nil }
	t.Cleanup(h.w.Close)
	return h
}

// card creates root/name with the given top-level directories and marks it
// a mount point.
func (h *harness) card(name string, dirs ...string) {
	h.t.Helper()
	h.dir(name, dirs...)
	h.mounts[name] = true
}

// dir creates root/name with the given top-level directories, not a mount.
func (h *harness) dir(name string, dirs ...string) {
	h.t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(h.root, name, filepath.FromSlash(d)), 0o755); err != nil {
			h.t.Fatal(err)
		}
	}
	if len(dirs) == 0 {
		if err := os.MkdirAll(filepath.Join(h.root, name), 0o755); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) remove(name string) {
	h.t.Helper()
	if err := os.RemoveAll(filepath.Join(h.root, name)); err != nil {
		h.t.Fatal(err)
	}
	delete(h.mounts, name)
}

func (h *harness) submits() []ingest.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ingest.Request(nil), h.reqs...)
}

func (h *harness) wantSubmits(n int, when string) {
	h.t.Helper()
	if got := len(h.submits()); got != n {
		h.t.Fatalf("%s: %d submits, want %d; logs:\n%s", when, got, n, h.logs.String())
	}
}

func TestPollerStartupMountsNotImported(t *testing.T) {
	h := newHarness(t)
	h.card("CARD1", "DCIM")
	h.w.tick(t0)
	h.w.tick(t0.Add(20 * time.Second))
	h.w.tick(t0.Add(40 * time.Second))
	h.wantSubmits(0, "startup mount")
	if !strings.Contains(h.logs.String(), "not importing what was already mounted at startup") {
		t.Errorf("missing startup log:\n%s", h.logs.String())
	}
}

func TestPollerImportsNewCardAfterSettle(t *testing.T) {
	h := newHarness(t)
	h.w.tick(t0)
	t1 := t0.Add(time.Minute)
	h.card("CARD", "dcim")
	h.w.tick(t1)
	h.wantSubmits(0, "t1")
	h.w.tick(t1.Add(5 * time.Second))
	h.wantSubmits(0, "t1+5s")
	h.w.tick(t1.Add(10 * time.Second))
	h.wantSubmits(1, "t1+10s")
	got := h.submits()[0]
	if got.Path != filepath.Join(h.root, "CARD") || got.Label != "CARD" {
		t.Errorf("request = %+v", got)
	}
	h.w.tick(t1.Add(20 * time.Second))
	h.w.tick(t1.Add(60 * time.Second))
	h.wantSubmits(1, "later")
	if !strings.Contains(h.logs.String(), "card poller: importing") {
		t.Errorf("missing importing log:\n%s", h.logs.String())
	}
}

func TestPollerReinsertedCardImportedAgain(t *testing.T) {
	h := newHarness(t)
	h.w.tick(t0)
	t1 := t0.Add(time.Minute)
	h.card("CARD", "DCIM")
	h.w.tick(t1)
	h.w.tick(t1.Add(10 * time.Second))
	h.wantSubmits(1, "first")
	h.remove("CARD")
	t2 := t1.Add(30 * time.Second)
	h.w.tick(t2)
	h.card("CARD", "DCIM")
	h.w.tick(t2.Add(5 * time.Second))
	h.w.tick(t2.Add(15 * time.Second))
	h.wantSubmits(2, "reinserted")
}

func TestPollerIgnoresNonCards(t *testing.T) {
	h := newHarness(t)
	h.w.tick(t0)
	h.dir("plain", "DCIM")        // not a mount point
	h.card("nodcim", "Documents") // no DCIM/PRIVATE
	h.card(".hidden", "DCIM")     // hidden
	if err := os.WriteFile(filepath.Join(h.root, "CARD"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mounts["CARD"] = true
	h.card("filedcim") // DCIM is a file, not a folder
	if err := os.WriteFile(filepath.Join(h.root, "filedcim", "DCIM"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		h.w.tick(t0.Add(time.Duration(i+1) * 10 * time.Second))
	}
	h.wantSubmits(0, "non-cards")
}

func TestPollerPrivateCounts(t *testing.T) {
	h := newHarness(t)
	h.w.tick(t0)
	h.card("SONY", "PRIVATE/M4ROOT")
	h.w.tick(t0.Add(5 * time.Second))
	h.w.tick(t0.Add(15 * time.Second))
	h.wantSubmits(1, "private")
}

func TestPollerSubmitErrorNotRetried(t *testing.T) {
	h := newHarness(t)
	h.failErr = errors.New("queue is full")
	h.w.tick(t0)
	h.card("CARD", "DCIM")
	h.w.tick(t0.Add(5 * time.Second))
	h.w.tick(t0.Add(15 * time.Second))
	h.w.tick(t0.Add(30 * time.Second))
	h.w.tick(t0.Add(60 * time.Second))
	h.wantSubmits(1, "error")
	if !strings.Contains(h.logs.String(), "could not start an import") {
		t.Errorf("missing error log:\n%s", h.logs.String())
	}
}

func TestPollerMountAppearsLate(t *testing.T) {
	h := newHarness(t)
	h.w.tick(t0)
	h.dir("CARD", "DCIM")
	h.w.tick(t0.Add(5 * time.Second))
	h.w.tick(t0.Add(10 * time.Second))
	h.mounts["CARD"] = true
	h.w.tick(t0.Add(15 * time.Second)) // qualifies here
	h.w.tick(t0.Add(20 * time.Second))
	h.wantSubmits(0, "settling")
	h.w.tick(t0.Add(25 * time.Second))
	h.wantSubmits(1, "settled")
}

func TestPollerRootUnreadable(t *testing.T) {
	h := newHarness(t)
	if err := os.RemoveAll(h.root); err != nil {
		t.Fatal(err)
	}
	h.w.tick(t0)
	h.w.tick(t0.Add(5 * time.Second))
	if n := strings.Count(h.logs.String(), "cannot read the cards folder"); n != 1 {
		t.Errorf("%d error lines, want 1:\n%s", n, h.logs.String())
	}
	h.wantSubmits(0, "unreadable")
}

func TestPollerStartClose(t *testing.T) {
	root := t.TempDir()
	w := New(Config{
		Root:   root,
		Submit: func(context.Context, ingest.Request) (*ingest.Job, error) { return nil, errors.New("unused") },
		Every:  10 * time.Millisecond,
		Log:    slog.New(slog.DiscardHandler),
	})
	w.Start()
	w.Start()
	time.Sleep(50 * time.Millisecond) // let a few real ticks run
	done := make(chan struct{})
	go func() {
		w.Close()
		w.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	w.Start() // no-op after Close
}

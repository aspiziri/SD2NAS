// Package cardwatch is the opt-in fallback for setups without the Unassigned
// Devices host script: it polls the cards folder and submits an import for
// each card that shows up there.
package cardwatch

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aspiziri/SD2NAS/internal/ingest"
)

const (
	DefaultEvery  = 5 * time.Second
	DefaultSettle = 10 * time.Second
)

// Config configures a Watcher.
type Config struct {
	Root   string                                                     // the cards folder
	Submit func(context.Context, ingest.Request) (*ingest.Job, error) // api.Server.SubmitIngest
	Every  time.Duration                                              // 0: DefaultEvery
	Settle time.Duration                                              // 0: DefaultSettle
	Log    *slog.Logger                                               // nil: slog.Default()
}

// Watcher polls the cards folder.
type Watcher struct {
	cfg Config
	log *slog.Logger

	// isMount is a seam for tests.
	isMount func(dir, root string) (bool, error)

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	started bool
	closed  bool
	done    chan struct{}

	// Touched only by the loop goroutine (or a test calling tick).
	baselined bool
	handled   map[string]bool
	since     map[string]time.Time
	lastErr   string
}

// New returns a Watcher. Call Start to begin polling.
func New(cfg Config) *Watcher {
	if cfg.Every <= 0 {
		cfg.Every = DefaultEvery
	}
	if cfg.Settle <= 0 {
		cfg.Settle = DefaultSettle
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Watcher{
		cfg:     cfg,
		log:     log,
		isMount: isMount,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		handled: map[string]bool{},
		since:   map[string]time.Time{},
	}
}

// Start begins polling. It does nothing after Close, or if already started.
func (w *Watcher) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.closed {
		return
	}
	w.started = true
	go w.loop()
}

// Close stops polling and waits for the loop to end. It is safe to call twice.
func (w *Watcher) Close() {
	w.mu.Lock()
	w.closed = true
	started := w.started
	w.mu.Unlock()
	w.cancel()
	if started {
		<-w.done
	}
}

func (w *Watcher) loop() {
	defer close(w.done)
	t := time.NewTicker(w.cfg.Every)
	defer t.Stop()
	w.tick(time.Now())
	for {
		select {
		case <-w.ctx.Done():
			return
		case now := <-t.C:
			w.tick(now)
		}
	}
}

// tick reads the cards folder once (Decision 10). Only the loop goroutine
// (or a test) calls it.
func (w *Watcher) tick(now time.Time) {
	entries, err := os.ReadDir(w.cfg.Root)
	if err != nil {
		if msg := err.Error(); msg != w.lastErr {
			w.log.Error("card poller: cannot read the cards folder", "path", w.cfg.Root, "err", err)
			w.lastErr = msg
		}
		return
	}
	w.lastErr = ""
	present := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			present[e.Name()] = true
		}
	}
	for name := range w.handled {
		if !present[name] {
			delete(w.handled, name) // gone: next time it is a new card
		}
	}
	// A handled folder that stays but stops being a mount point (Unassigned
	// Devices can leave the folder behind after an unmount) is re-armed, so
	// the next card mounted there is new. An error keeps it handled.
	for name := range w.handled {
		dir := filepath.Join(w.cfg.Root, name)
		mounted, err := w.isMount(dir, w.cfg.Root)
		if err != nil {
			w.log.Warn("card poller: cannot tell whether a handled card is still mounted", "path", dir, "err", err)
			continue
		}
		if !mounted {
			delete(w.handled, name)
		}
	}
	for name := range w.since {
		if !present[name] {
			delete(w.since, name)
		}
	}
	if !w.baselined {
		w.baselined = true
		names := slices.Sorted(maps.Keys(present))
		for _, name := range names {
			w.handled[name] = true
		}
		if len(names) > 0 {
			w.log.Info("card poller: not importing what was already mounted at startup", "names", names)
		}
		return
	}
	for _, name := range slices.Sorted(maps.Keys(present)) {
		if w.handled[name] {
			continue
		}
		dir := filepath.Join(w.cfg.Root, name)
		ok, err := w.looksLikeCard(dir)
		if err != nil {
			w.log.Warn("card poller: cannot look into a mount", "path", dir, "err", err)
		}
		if !ok {
			delete(w.since, name)
			continue
		}
		first, seen := w.since[name]
		if !seen {
			w.since[name] = now
			continue
		}
		if now.Sub(first) < w.cfg.Settle {
			continue
		}
		delete(w.since, name)
		w.handled[name] = true // one submit per appearance, whatever happens
		job, err := w.cfg.Submit(w.ctx, ingest.Request{Path: dir, Label: name})
		if err != nil {
			w.log.Error("card poller: could not start an import", "card", dir, "err", err)
			continue
		}
		w.log.Info("card poller: importing", "card", dir, "ingest", job.ID())
	}
}

// looksLikeCard reports whether dir is a mount point holding a top-level
// DCIM or PRIVATE folder (any case).
func (w *Watcher) looksLikeCard(dir string) (bool, error) {
	mounted, err := w.isMount(dir, w.cfg.Root)
	if err != nil || !mounted {
		return false, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.EqualFold(e.Name(), "DCIM") || strings.EqualFold(e.Name(), "PRIVATE") {
			return true, nil
		}
	}
	return false, nil
}

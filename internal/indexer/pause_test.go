package indexer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// frames records every published Status.
type frames struct {
	mu  sync.Mutex
	all []Status
}

func recordFrames(s *Service) *frames {
	f := &frames{}
	s.OnChange(func(st Status) {
		f.mu.Lock()
		f.all = append(f.all, st)
		f.mu.Unlock()
	})
	return f
}

func (f *frames) pausedBy(by string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, st := range f.all {
		if st.Running != nil && st.Running.PausedBy == by {
			return true
		}
	}
	return false
}

func pausedBy(s *Service) string {
	if r := s.Status().Running; r != nil {
		return r.PausedBy
	}
	return "(not running)"
}

func putFive(t *testing.T, photos string) {
	t.Helper()
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		put(t, photos, "Archive/T/"+n+".JPG", "content "+n, t0)
	}
}

func TestPausesWhileBusy(t *testing.T) {
	s, _, photos := setup(t)
	s.idleEvery = 10 * time.Millisecond
	var busy atomic.Bool
	busy.Store(true)
	s.busy = busy.Load
	putFive(t, photos)
	n := countHashes(s)
	fr := recordFrames(s)
	if !s.StartRescan(false) {
		t.Fatal("StartRescan = false")
	}
	waitFor(t, "paused_by busy", func() bool { return pausedBy(s) == "busy" })
	time.Sleep(200 * time.Millisecond) // negative check: nothing is read while busy
	if n.Load() != 0 {
		t.Errorf("hash calls while busy = %d; want 0", n.Load())
	}
	if !fr.pausedBy("busy") {
		t.Error("no frame with paused_by busy")
	}
	busy.Store(false)
	waitFor(t, "the pass to end", func() bool { return s.Status().Running == nil })
	if l := s.Status().Last; l == nil || l.Added != 5 || l.Stopped {
		t.Errorf("Last = %+v; want Added 5", l)
	}
}

func TestPauseResume(t *testing.T) {
	s, db, photos := setup(t)
	s.idleEvery = time.Hour // only Resume can wake the pass
	putFive(t, photos)
	n := countHashes(s)
	fr := recordFrames(s)
	var reads int
	s.beforeRead = func(_ context.Context, _ string) {
		reads++
		if reads == 2 {
			if _, err := s.Pause(); err != nil {
				t.Error(err)
			}
		}
	}
	if !s.StartRescan(false) {
		t.Fatal("StartRescan = false")
	}
	waitFor(t, "the second file", func() bool { return n.Load() == 2 && assetIndexed(t, s, "content b") })
	time.Sleep(200 * time.Millisecond) // negative check: no third read
	if n.Load() != 2 {
		t.Errorf("hash calls while paused = %d; want 2", n.Load())
	}
	if got := pausedBy(s); got != "user" {
		t.Errorf("paused_by = %q; want user", got)
	}
	if !fr.pausedBy("user") {
		t.Error("no frame with paused_by user")
	}
	asset(t, db, "content a")
	asset(t, db, "content b")
	noAsset(t, db, "content c")

	st, err := s.Resume()
	if err != nil {
		t.Fatal(err)
	}
	if st.Running == nil || st.Running.PausedBy != "" {
		t.Errorf("status after Resume = %+v", st.Running)
	}
	waitFor(t, "the pass to end", func() bool { return s.Status().Running == nil })
	if l := s.Status().Last; l == nil || l.Added != 5 || l.Stopped {
		t.Errorf("Last = %+v; want Added 5", l)
	}
	if _, err := s.Pause(); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Pause after the pass = %v; want ErrNotRunning", err)
	}
}

func assetIndexed(t *testing.T, s *Service, content string) bool {
	t.Helper()
	got, err := s.db.AssetsByHash(t.Context(), []string{sum(content)})
	if err != nil {
		t.Fatal(err)
	}
	return len(got) == 1
}

func TestPauseNoOps(t *testing.T) {
	s, _, photos := setup(t)
	s.idleEvery = time.Hour
	putFive(t, photos)
	gate := make(chan struct{})
	s.beforeRead = func(ctx context.Context, _ string) {
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	if !s.StartRescan(false) {
		t.Fatal("StartRescan = false")
	}
	defer close(gate)
	if st, err := s.Resume(); err != nil || st.Running == nil || st.Running.PausedBy != "" {
		t.Errorf("Resume of a running pass = %+v, %v; want a no-op", st.Running, err)
	}
	for i := 0; i < 2; i++ {
		if st, err := s.Pause(); err != nil || st.Running == nil || st.Running.PausedBy != "user" {
			t.Errorf("Pause #%d = %+v, %v", i+1, st.Running, err)
		}
	}
}

func TestPauseNotRunning(t *testing.T) {
	s, _, _ := setup(t)
	if _, err := s.Pause(); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Pause = %v; want ErrNotRunning", err)
	}
	if _, err := s.Resume(); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Resume = %v; want ErrNotRunning", err)
	}
	s.Close()
	if _, err := s.Pause(); !errors.Is(err, ErrClosed) {
		t.Errorf("Pause after Close = %v; want ErrClosed", err)
	}
	if _, err := s.Resume(); !errors.Is(err, ErrClosed) {
		t.Errorf("Resume after Close = %v; want ErrClosed", err)
	}
}

func TestUserPauseWinsOverBusy(t *testing.T) {
	s, _, photos := setup(t)
	s.idleEvery = 10 * time.Millisecond
	var busy atomic.Bool
	busy.Store(true)
	s.busy = busy.Load
	putFive(t, photos)
	if !s.StartRescan(false) {
		t.Fatal("StartRescan = false")
	}
	waitFor(t, "paused_by busy", func() bool { return pausedBy(s) == "busy" })
	if _, err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // negative check: the loop must not flip it back to busy
	if got := pausedBy(s); got != "user" {
		t.Errorf("paused_by = %q; want user while both apply", got)
	}
	if _, err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "paused_by busy again", func() bool { return pausedBy(s) == "busy" })
	busy.Store(false)
	waitFor(t, "the pass to end", func() bool { return s.Status().Running == nil })
	if l := s.Status().Last; l == nil || l.Added != 5 {
		t.Errorf("Last = %+v; want Added 5", l)
	}
}

func TestCloseWhilePaused(t *testing.T) {
	s, db, photos := setup(t)
	s.idleEvery = time.Hour
	putFive(t, photos)
	n := countHashes(s)
	var reads int
	s.beforeRead = func(_ context.Context, _ string) {
		reads++
		if reads == 2 {
			if _, err := s.Pause(); err != nil {
				t.Error(err)
			}
		}
	}
	if !s.StartRescan(false) {
		t.Fatal("StartRescan = false")
	}
	waitFor(t, "the second file", func() bool { return n.Load() == 2 && assetIndexed(t, s, "content b") })
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return while paused")
	}
	if l := s.Status().Last; l == nil || !l.Stopped || l.Added != 2 {
		t.Errorf("Last = %+v; want Stopped, Added 2", l)
	}

	s2, err := New(Config{DB: db, PhotosRoot: photos})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s2.Close)
	s2.idleEvery = time.Hour
	r, err := s2.Rescan(t.Context(), TriggerManual, false) // would hang if it started paused
	if err != nil {
		t.Fatal(err)
	}
	wantCounts(t, r, ScanResult{Listed: 5, Unchanged: 2, Added: 3})
}

// A Pause just before a pass fails to start does not leave the next pass
// paused (T10-m1).
func TestFailedStartClearsPause(t *testing.T) {
	s, db, _ := setup(t)
	s.beginPass(TriggerManual, false) // as StartRescan does, before the catalog writes
	if _, err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	v := s.Status().Version
	if err := db.Close(); err != nil { // the start's catalog writes fail
		t.Fatal(err)
	}
	if _, err := s.Rescan(t.Context(), TriggerManual, false); err == nil {
		t.Fatal("Rescan on a closed catalog succeeded")
	}
	st := s.Status()
	if s.isUserPaused() || st.Running != nil || st.Version <= v {
		t.Errorf("after a failed start: userPaused %v, Running %+v, Version %d (was %d)", s.isUserPaused(), st.Running, st.Version, v)
	}
}

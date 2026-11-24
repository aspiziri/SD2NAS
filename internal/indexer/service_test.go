package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// waitFor polls cond for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStartRescanOnce(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/2024-05-01/a.jpg", "a", t0)
	release := make(chan struct{})
	s.beforeRead = func(ctx context.Context, rel string) {
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	if !s.StartRescan(false) {
		t.Fatal("first StartRescan = false")
	}
	if s.StartRescan(false) {
		t.Error("second StartRescan = true while a pass runs")
	}
	run := s.Status().Running
	if run == nil || run.Trigger != TriggerManual {
		t.Fatalf("Running = %+v, want a manual pass", run)
	}
	before := s.Status().Version
	close(release)
	waitFor(t, "the pass to end", func() bool { return s.Status().Running == nil })
	st := s.Status()
	if st.Last == nil || st.Last.Added != 1 {
		t.Fatalf("Last = %+v, want Added 1", st.Last)
	}
	if st.Version <= before {
		t.Errorf("Version %d did not bump past %d", st.Version, before)
	}
	row, err := db.LastArchiveScan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !row.Complete {
		t.Error("scan row is not complete")
	}
	want, err := json.Marshal(st.Last)
	if err != nil {
		t.Fatal(err)
	}
	if row.Result != string(want) {
		t.Errorf("stored result %s != Last %s", row.Result, want)
	}
	if !s.StartRescan(false) { // a finished pass frees the slot
		t.Error("StartRescan after the pass = false")
	}
	waitFor(t, "the second pass to end", func() bool { return s.Status().Running == nil })
}

func TestCloseStopsPass(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/S/a.jpg", "a", t0)
	put(t, photos, "Archive/S/b.jpg", "b", t0)
	inRead := make(chan struct{})
	var once sync.Once
	s.beforeRead = func(ctx context.Context, rel string) {
		once.Do(func() { close(inRead) })
		<-ctx.Done()
	}
	if !s.StartRescan(false) {
		t.Fatal("StartRescan = false")
	}
	select {
	case <-inRead:
	case <-time.After(10 * time.Second):
		t.Fatal("the pass never reached a read")
	}
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	st := s.Status()
	if st.Running != nil || st.Last == nil || !st.Last.Stopped {
		t.Fatalf("status after Close = running %+v, last %+v; want a stopped Last", st.Running, st.Last)
	}
	if s.StartRescan(false) {
		t.Error("StartRescan after Close = true")
	}
	row, err := db.LastArchiveScan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if row.Complete {
		t.Error("the stopped scan row is complete")
	}

	s2, err := New(Config{DB: db, PhotosRoot: photos})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s2.Close)
	reads := countHashes(s2)
	r, err := s2.Rescan(t.Context(), TriggerManual, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Added != 2 || reads.Load() != 2 {
		t.Errorf("second service: Added %d, reads %d; want 2 and 2", r.Added, reads.Load())
	}
}

func TestInterruptedScanRecorded(t *testing.T) {
	_, db, photos := setup(t)
	if _, err := db.StartArchiveScan(t.Context(), "nightly", time.Time{}, t0); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{DB: db, PhotosRoot: photos})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	last := s.Status().Last
	if last == nil || !last.Stopped || last.Trigger != "nightly" {
		t.Fatalf("Last = %+v, want a stopped nightly pass", last)
	}
	if want := []string{"SD2NAS stopped before this scan finished"}; !reflect.DeepEqual(last.Errors, want) {
		t.Errorf("Errors = %q, want %q", last.Errors, want)
	}
	open, err := db.UnfinishedArchiveScans(t.Context())
	if err != nil || len(open) != 0 {
		t.Errorf("unfinished scans = %v, %v; want none", open, err)
	}
}

func TestNewOnFreshCatalog(t *testing.T) {
	s, _, _ := setup(t)
	st := s.Status()
	if st.Last != nil || st.Running != nil || st.Changed != 0 || st.Missing != 0 {
		t.Errorf("fresh status = %+v", st)
	}
}

func TestProgressThrottled(t *testing.T) {
	s, _, photos := setup(t)
	for i := range 300 {
		put(t, photos, fmt.Sprintf("Archive/S%d/f%03d.jpg", i%10, i), fmt.Sprintf("content %d", i), t0)
	}
	type frame struct {
		at time.Time
		st Status
	}
	var mu sync.Mutex
	var frames []frame
	s.OnChange(func(st Status) {
		mu.Lock()
		frames = append(frames, frame{time.Now(), st})
		mu.Unlock()
	})
	// Slow each read a little so the pass spans several throttle windows.
	s.beforeRead = func(ctx context.Context, rel string) { time.Sleep(2 * time.Millisecond) }
	if !s.StartRescan(false) {
		t.Fatal("StartRescan = false")
	}
	waitFor(t, "the pass to end", func() bool { return s.Status().Running == nil })

	mu.Lock()
	defer mu.Unlock()
	if len(frames) < 3 {
		t.Fatalf("only %d frames", len(frames))
	}
	for i := 1; i < len(frames); i++ {
		a, b := frames[i-1], frames[i]
		if a.st.Running == nil || b.st.Running == nil {
			continue
		}
		if a.st.Running.Phase != b.st.Running.Phase || a.st.Running.PausedBy != b.st.Running.PausedBy {
			continue // a forced frame
		}
		if gap := b.at.Sub(a.at); gap < 240*time.Millisecond {
			t.Errorf("frames %d and %d in phase %q are %v apart", i-1, i, b.st.Running.Phase, gap)
		}
	}
	last := frames[len(frames)-1].st
	if last.Running != nil || last.Last == nil || last.Last.Added != 300 {
		t.Errorf("last frame = running %+v, last %+v; want the finished pass with Added 300", last.Running, last.Last)
	}
	var phases []string
	for _, f := range frames {
		if f.st.Running != nil && (len(phases) == 0 || phases[len(phases)-1] != f.st.Running.Phase) {
			phases = append(phases, f.st.Running.Phase)
		}
	}
	if want := []string{"", PhaseListing, PhaseHashing, PhaseChecking}; !reflect.DeepEqual(phases, want) {
		t.Errorf("phases = %q, want %q", phases, want)
	}
}

func TestStatsAndAccept(t *testing.T) {
	s, _, photos := setup(t)
	pa := put(t, photos, "Archive/S/edit.jpg", "one", t0)
	pb := put(t, photos, "Archive/S/gone.jpg", "two", t0)
	put(t, photos, "Archive/S/keep.jpg", "three", t0)
	rescan(t, s)
	put(t, photos, "Archive/S/edit.jpg", "one edited", t0.Add(time.Minute))
	if err := os.Remove(pb); err != nil {
		t.Fatal(err)
	}
	_ = pa
	rescan(t, s)

	var frames []Status
	var mu sync.Mutex
	s.OnChange(func(st Status) { mu.Lock(); frames = append(frames, st); mu.Unlock() })

	st, err := s.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 2 || st.Bytes != int64(len("one edited")+len("three")) {
		t.Errorf("Files %d Bytes %d", st.Files, st.Bytes)
	}
	if st.Changed != 1 || st.Missing != 1 || len(st.MissingFiles) != 1 || st.MissingFiles[0].RelPath != "Archive/S/gone.jpg" {
		t.Errorf("Changed %d Missing %d MissingFiles %+v", st.Changed, st.Missing, st.MissingFiles)
	}
	if st.Recent == nil {
		t.Error("Recent is nil")
	}
	if st.RescanAt != "03:00" {
		t.Errorf("RescanAt = %q", st.RescanAt)
	}
	if want := []string{"Output/", "CaptureOne/", "*.cosessiondb"}; !reflect.DeepEqual(st.SkipRules, want) {
		t.Errorf("SkipRules = %q, want %q", st.SkipRules, want)
	}
	if want := filepath.Join(photos, "Management", "SD2NAS"); st.BackupDir != want {
		t.Errorf("BackupDir = %q, want %q", st.BackupDir, want)
	}
	if st.ArchivePath != filepath.Join(photos, "Archive") || !st.ArchivePresent {
		t.Errorf("ArchivePath %q present %v", st.ArchivePath, st.ArchivePresent)
	}
	cl, err := s.Changes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(cl.Changes) != 1 || cl.Total != 1 || cl.LatestID < 1 || cl.Version != st.Version {
		t.Fatalf("Changes = %+v (stats version %d)", cl, st.Version)
	}

	v := s.Status().Version
	n, err := s.Accept(t.Context(), cl.LatestID)
	if err != nil || n != 1 {
		t.Fatalf("Accept = %d, %v", n, err)
	}
	after := s.Status()
	if after.Changed != 0 || after.Version <= v {
		t.Errorf("after accept: Changed %d, Version %d (was %d)", after.Changed, after.Version, v)
	}
	mu.Lock()
	got := len(frames)
	mu.Unlock()
	if got == 0 {
		t.Error("accept published no frame")
	}
	cl, err = s.Changes(t.Context())
	if err != nil || cl.Changes == nil || len(cl.Changes) != 0 || cl.LatestID != 0 {
		t.Errorf("Changes after accept = %+v, %v", cl, err)
	}
	if _, err := s.Accept(t.Context(), 0); err == nil || errors.Is(err, ErrClosed) {
		t.Errorf("Accept(0) = %v, want a plain error", err)
	}
	s.Close()
	if _, err := s.Accept(t.Context(), 1); !errors.Is(err, ErrClosed) {
		t.Errorf("Accept after Close = %v, want ErrClosed", err)
	}
	if _, err := s.Stats(t.Context()); err != nil {
		t.Errorf("Stats after Close: %v", err)
	}
	if _, err := s.Changes(t.Context()); err != nil {
		t.Errorf("Changes after Close: %v", err)
	}
}

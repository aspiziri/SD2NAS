package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

func TestNextRun(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	at := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, la) }
	cases := []struct {
		name string
		now  time.Time
		at   time.Duration
		want time.Time
	}{
		{"before", at(2026, 6, 10, 2, 0), 3 * time.Hour, at(2026, 6, 10, 3, 0)},
		{"exactly", at(2026, 6, 10, 3, 0), 3 * time.Hour, at(2026, 6, 11, 3, 0)},
		{"after", at(2026, 6, 10, 23, 30), 3 * time.Hour, at(2026, 6, 11, 3, 0)},
		{"midnight", at(2026, 6, 10, 23, 59), 0, at(2026, 6, 11, 0, 0)},
	}
	for _, c := range cases {
		if got := nextRun(c.now, c.at); !got.Equal(c.want) {
			t.Errorf("%s: nextRun = %v; want %v", c.name, got, c.want)
		}
	}
	// DST gap: 02:30 does not exist on 2026-03-08.
	now := at(2026, 3, 8, 1, 0)
	got := nextRun(now, 2*time.Hour+30*time.Minute)
	if !got.After(now) || got.Year() != 2026 || got.Month() != 3 || got.Day() != 8 {
		t.Errorf("spring forward: nextRun = %v; want a time on 2026-03-08 after %v", got, now)
	}
	// DST overlap: 01:30 happens twice on 2026-11-01.
	now = at(2026, 11, 1, 0, 0)
	got = nextRun(now, time.Hour+30*time.Minute)
	if !got.After(now) || got.Year() != 2026 || got.Month() != 11 || got.Day() != 1 {
		t.Errorf("fall back: nextRun = %v; want a time on 2026-11-01 after %v", got, now)
	}
}

// fakeTimer is the Service's after: it records each wait and lets a test fire it.
type fakeTimer struct {
	mu    sync.Mutex
	waits []time.Duration
	chans []chan time.Time
}

func (f *fakeTimer) after(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	f.waits = append(f.waits, d)
	f.chans = append(f.chans, ch)
	return ch
}

func (f *fakeTimer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waits)
}

func (f *fakeTimer) wait(i int) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waits[i]
}

func (f *fakeTimer) fire(t *testing.T, i int) {
	t.Helper()
	waitFor(t, "the loop to arm its timer", func() bool { return f.count() > i })
	f.mu.Lock()
	ch := f.chans[i]
	f.mu.Unlock()
	ch <- time.Now()
}

func TestNightlyBacksUpThenScans(t *testing.T) {
	s, _, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "content a", t0)
	ft := &fakeTimer{}
	s.after = ft.after
	s.Start()
	waitFor(t, "the first timer", func() bool { return ft.count() == 1 })
	ft.fire(t, 0)
	waitFor(t, "the backup and the nightly pass", func() bool {
		st := s.Status()
		return st.LastBackup != nil && st.Last != nil && st.Running == nil && !st.BackingUp
	})
	st := s.Status()
	if st.LastBackup.Error != "" {
		t.Fatalf("backup error: %s", st.LastBackup.Error)
	}
	want := filepath.Join(photos, "Management", "SD2NAS", "catalog-"+st.LastBackup.At.Format("2006-01-02")+".db")
	if st.LastBackup.Path != want {
		t.Errorf("backup path = %q; want %q", st.LastBackup.Path, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Error(err)
	}
	if st.Last.Trigger != TriggerNightly || st.Last.Added != 1 {
		t.Errorf("Last = %+v; want a nightly pass adding 1", st.Last)
	}
	if st.LastBackup.At.After(st.Last.StartedAt) {
		t.Errorf("backup at %v is after the pass started at %v", st.LastBackup.At, st.Last.StartedAt)
	}
	waitFor(t, "the loop to arm again", func() bool { return ft.count() == 2 })
	s.Close() // returns: the loop ended
}

func TestNightlyBackupWhilePaused(t *testing.T) {
	s, db, photos := setup(t)
	s.idleEvery = time.Hour
	putFive(t, photos)
	ft := &fakeTimer{}
	s.after = ft.after
	s.Start()
	waitFor(t, "the first timer", func() bool { return ft.count() == 1 })
	var once sync.Once
	s.beforeRead = func(_ context.Context, _ string) {
		once.Do(func() {
			if _, err := s.Pause(); err != nil {
				t.Error(err)
			}
		})
	}
	if !s.StartRescan(false) {
		t.Fatal("StartRescan = false")
	}
	waitFor(t, "paused by user", func() bool { return pausedBy(s) == "user" })
	ft.fire(t, 0)
	waitFor(t, "the backup", func() bool { return s.Status().LastBackup != nil })
	st := s.Status()
	if st.LastBackup.Error != "" {
		t.Errorf("backup error: %s", st.LastBackup.Error)
	}
	if st.Running == nil || st.Running.PausedBy != "user" || st.Running.Trigger != TriggerManual {
		t.Errorf("Running = %+v; want the manual pass still paused by user", st.Running)
	}
	waitFor(t, "the loop to arm again", func() bool { return ft.count() == 2 })
	time.Sleep(100 * time.Millisecond) // negative check: no nightly pass starts
	if r := s.Status().Running; r == nil || r.Trigger != TriggerManual {
		t.Errorf("Running = %+v; want only the manual pass", r)
	}
	if _, err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the manual pass to finish", func() bool { return s.Status().Running == nil })
	if l := s.Status().Last; l == nil || l.Trigger != TriggerManual || l.Added != 5 {
		t.Errorf("Last = %+v; want the manual pass, 5 added", l)
	}
	last, err := db.LastArchiveScan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if last.Kind != TriggerManual {
		t.Errorf("last scan row kind = %q; want manual only", last.Kind)
	}
}

func TestSettingsChangedReschedules(t *testing.T) {
	s, db, _ := setup(t)
	ft := &fakeTimer{}
	s.after = ft.after
	s.Start()
	waitFor(t, "the first timer", func() bool { return ft.count() == 1 })
	v := s.Status().Version
	if err := db.SetSetting(t.Context(), catalog.KeyArchiveRescanAt, "05:15"); err != nil {
		t.Fatal(err)
	}
	s.SettingsChanged()
	waitFor(t, "the second timer", func() bool { return ft.count() == 2 })
	next := s.Status().NextScan
	if next.Hour() != 5 || next.Minute() != 15 {
		t.Errorf("NextScan = %v; want 05:15", next)
	}
	if d := ft.wait(1); d <= 0 || d > 24*time.Hour+time.Hour {
		t.Errorf("timer wait = %v; want a positive wait of at most a day", d)
	}
	if s.Status().Version <= v {
		t.Error("Version did not advance")
	}
}

// A Rescan that is waiting for a running pass when Close runs must not
// record a stopped scan.
func TestCloseWhileRescanWaits(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "content a", t0)
	s.pass.Lock() // stands in for a running pass
	done := make(chan error, 1)
	go func() {
		_, err := s.Rescan(context.Background(), TriggerManual, false)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // let it block on the lock
	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()
	waitFor(t, "Close to cancel", func() bool { return s.bgCtx.Err() != nil })
	s.pass.Unlock()
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("Rescan = %v; want ErrClosed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Rescan did not return")
	}
	<-closed
	if _, err := db.LastArchiveScan(t.Context()); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("LastArchiveScan = %v; want ErrNotFound (no scan recorded)", err)
	}
}

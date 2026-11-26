package notices

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/indexer"
	"github.com/aspiziri/SD2NAS/internal/media"
	"github.com/aspiziri/SD2NAS/internal/staging"
)

// chatham is far from UTC (+12:45/+13:45), so a check that used UTC or
// time.Local for "09:00" or the date would fail.
func chatham(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Pacific/Chatham")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// clock is a settable now.
type clock struct {
	mu  sync.Mutex
	t   time.Time
	loc *time.Location
}

func newClock(t *testing.T, at string) *clock {
	c := &clock{loc: chatham(t)}
	c.set(t, at)
	return c
}

// set moves the clock to at ("2006-01-02 15:04", Chatham time).
func (c *clock) set(t *testing.T, at string) {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", at, c.loc)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.t = v
	c.mu.Unlock()
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func newDB(t *testing.T) *catalog.DB {
	t.Helper()
	db, _ := newDBPath(t)
	return db
}

// newDBPath is newDB that also returns the catalog's file.
func newDBPath(t *testing.T) (*catalog.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sd2nas.db")
	db, err := catalog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db, path
}

// newService fills in a discarding logger and closes the service before the DB.
func newService(t *testing.T, cfg Config) *Service {
	t.Helper()
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func pending(t *testing.T, db *catalog.DB) []catalog.Notification {
	t.Helper()
	ns, err := db.PendingNotifications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

func wantNotice(t *testing.T, n catalog.Notification, key, level, msg string) {
	t.Helper()
	if n.Key != key || n.Level != level || n.Message != msg {
		t.Errorf("notice = %q %q %q, want %q %q %q", n.Key, n.Level, n.Message, key, level, msg)
	}
}

// fakeStaging counts its calls.
type fakeStaging struct {
	calls int
	l     staging.Listing
	err   error
}

func (f *fakeStaging) list(context.Context) (staging.Listing, error) {
	f.calls++
	return f.l, f.err
}

func staleListing() staging.Listing {
	return staging.Listing{StaleAfterDays: 30, Folders: []staging.Folder{
		{Name: "A", AgeDays: 35, Stale: true},
		{Name: "B", AgeDays: 40, Stale: true},
		{Name: "C", AgeDays: 3},
	}}
}

func TestNewNeedsDB(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New without a DB succeeded")
	}
}

func TestCheckBackupFailureOnce(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 03:00")
	at := c.now().Add(-time.Hour)
	archive := func() indexer.Status {
		return indexer.Status{LastBackup: &indexer.BackupResult{At: at, Error: "disk full."}}
	}
	s := newService(t, Config{DB: db, Archive: archive, Now: c.now})
	s.check(ctx)
	s.check(ctx)
	ns := pending(t, db)
	if len(ns) != 1 {
		t.Fatalf("pending = %+v, want one", ns)
	}
	wantNotice(t, ns[0], "backup:"+at.UTC().Format(time.RFC3339Nano), catalog.NoticeAlert,
		"Nightly catalog backup failed: disk full. Older backups were kept.")

	restarted := newService(t, Config{DB: db, Archive: archive, Now: c.now})
	restarted.check(ctx)
	if ns := pending(t, db); len(ns) != 1 {
		t.Fatalf("after a restart pending = %+v, want one", ns)
	}
}

func TestCheckOldOrSuccessfulBackupIgnored(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 03:00")
	var st indexer.Status
	s := newService(t, Config{DB: db, Archive: func() indexer.Status { return st }, Now: c.now})

	markMissing(t, db, 2, c.now().Add(-25*time.Hour))
	st = indexer.Status{
		LastBackup: &indexer.BackupResult{At: c.now().Add(-25 * time.Hour), Error: "disk full"},
		Last:       &indexer.ScanResult{FinishedAt: c.now().Add(-25 * time.Hour), Missing: 2},
		Missing:    2,
	}
	s.check(ctx)
	if ns := pending(t, db); len(ns) != 0 {
		t.Fatalf("old events queued %+v", ns)
	}

	st = indexer.Status{LastBackup: &indexer.BackupResult{At: c.now().Add(-time.Hour)}}
	s.check(ctx)
	if ns := pending(t, db); len(ns) != 0 {
		t.Fatalf("a successful backup queued %+v", ns)
	}
}

// nextAsset numbers the assets markMissing adds (the tests don't run in parallel).
var nextAsset int

// markMissing adds n archived assets and marks them missing at at, as a
// pass of the archive indexer does.
func markMissing(t *testing.T, db *catalog.DB, n int, at time.Time) {
	t.Helper()
	for range n {
		nextAsset++
		sha := fmt.Sprintf("%064x", nextAsset)
		a := catalog.Asset{SHA256: sha, Size: 1, Kind: media.KindImage, State: catalog.StateArchived,
			RelPath: fmt.Sprintf("Archive/s/%d.JPG", nextAsset), UpdatedAt: at.Add(-time.Hour)}
		if err := db.InsertAsset(t.Context(), a, ""); err != nil {
			t.Fatal(err)
		}
		if err := db.MarkMissing(t.Context(), sha, at); err != nil {
			t.Fatal(err)
		}
	}
}

// missingKey is the key of a missing-files notice whose newest row was marked at at.
func missingKey(at time.Time) string { return "missing:" + at.UTC().Format(time.RFC3339Nano) }

func TestCheckMissing(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 03:00")
	var st indexer.Status
	s := newService(t, Config{DB: db, Archive: func() indexer.Status { return st }, Now: c.now})

	first := c.now().Add(-5 * time.Minute)
	markMissing(t, db, 3, first)
	st = indexer.Status{Last: &indexer.ScanResult{FinishedAt: first, Missing: 3}, Missing: 3}
	s.check(ctx)
	second := c.now().Add(-4 * time.Minute)
	markMissing(t, db, 1, second)
	st = indexer.Status{Last: &indexer.ScanResult{FinishedAt: second, Missing: 1}, Missing: 4}
	s.check(ctx)
	st = indexer.Status{Last: &indexer.ScanResult{FinishedAt: c.now().Add(-3 * time.Minute), Missing: 0}, Missing: 4}
	s.check(ctx)

	ns := pending(t, db)
	if len(ns) != 2 {
		t.Fatalf("pending = %+v, want two", ns)
	}
	wantNotice(t, ns[0], missingKey(first), catalog.NoticeAlert,
		"The archive scan marked 3 files missing from Archive/. See the Archive index page.")
	wantNotice(t, ns[1], missingKey(second), catalog.NoticeAlert,
		"The archive scan marked 1 file missing from Archive/. See the Archive index page.")
}

func TestCheckStagingDaily(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 08:59")
	f := &fakeStaging{l: staleListing()}
	s := newService(t, Config{DB: db, Staging: f.list, Now: c.now})

	s.check(ctx)
	if f.calls != 0 || len(pending(t, db)) != 0 {
		t.Fatalf("at 08:59: %d calls, pending %+v", f.calls, pending(t, db))
	}

	c.set(t, "2026-11-23 09:00") // 2026-11-22 19:15 UTC
	s.check(ctx)
	ns := pending(t, db)
	if f.calls != 1 || len(ns) != 1 {
		t.Fatalf("at 09:00: %d calls, pending %+v", f.calls, ns)
	}
	wantNotice(t, ns[0], "staging:2026-11-23", catalog.NoticeWarning,
		"2 folders have been in Staging for 30 days or more: B and A.")

	c.set(t, "2026-11-23 15:00")
	s.check(ctx)
	if f.calls != 1 {
		t.Fatalf("at 15:00: %d calls, want 1", f.calls)
	}

	c.set(t, "2026-11-24 09:00")
	s.check(ctx)
	ns = pending(t, db)
	if f.calls != 2 || len(ns) != 2 || ns[1].Key != "staging:2026-11-24" {
		t.Fatalf("next day: %d calls, pending %+v", f.calls, ns)
	}

	c.set(t, "2026-11-24 10:00")
	restarted := newService(t, Config{DB: db, Staging: f.list, Now: c.now})
	restarted.check(ctx)
	if f.calls != 2 {
		t.Fatalf("after a restart: %d calls, want 2", f.calls)
	}
	if len(pending(t, db)) != 2 {
		t.Fatalf("after a restart pending = %+v", pending(t, db))
	}
}

func TestCheckStagingNoneStale(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 09:00")
	f := &fakeStaging{l: staging.Listing{StaleAfterDays: 30, Folders: []staging.Folder{{Name: "A", AgeDays: 3}}}}
	s := newService(t, Config{DB: db, Staging: f.list, Now: c.now})

	s.check(ctx)
	c.set(t, "2026-11-23 10:00")
	s.check(ctx)
	if f.calls != 1 || len(pending(t, db)) != 0 {
		t.Fatalf("%d calls, pending %+v; want 1 call, none", f.calls, pending(t, db))
	}

	restarted := newService(t, Config{DB: db, Staging: f.list, Now: c.now})
	restarted.check(ctx)
	if f.calls != 2 || len(pending(t, db)) != 0 {
		t.Fatalf("after a restart: %d calls, pending %+v", f.calls, pending(t, db))
	}

	off := &fakeStaging{l: staging.Listing{StaleAfterDays: 0, Folders: []staging.Folder{{Name: "A", AgeDays: 90, Stale: true}}}}
	s = newService(t, Config{DB: db, Staging: off.list, Now: c.now})
	s.check(ctx)
	if off.calls != 1 || len(pending(t, db)) != 0 {
		t.Fatalf("warnings off: %d calls, pending %+v", off.calls, pending(t, db))
	}
}

func TestStaleTexts(t *testing.T) {
	named := func(names ...string) []staging.Folder {
		var fs []staging.Folder
		for _, n := range names {
			fs = append(fs, staging.Folder{Name: n, AgeDays: 30, Stale: true})
		}
		return fs
	}
	for _, tc := range []struct {
		name    string
		days    int
		folders []staging.Folder
		want    string
	}{
		{"one folder, one day", 1, []staging.Folder{{Name: "X", AgeDays: 1}},
			"1 folder has been in Staging for 1 day or more: X."},
		{"two", 30, named("a", "b"),
			"2 folders have been in Staging for 30 days or more: a and b."},
		{"three", 30, named("a", "b", "c"),
			"3 folders have been in Staging for 30 days or more: a, b and c."},
		{"five", 30, named("f1", "f2", "f3", "f4", "f5"),
			"5 folders have been in Staging for 30 days or more: f1, f2, f3, f4 and f5."},
		{"seven", 30, named("f1", "f2", "f3", "f4", "f5", "f6", "f7"),
			"7 folders have been in Staging for 30 days or more: f1, f2, f3, f4, f5 and 2 more."},
		{"oldest first, then by name", 30, []staging.Folder{
			{Name: "c", AgeDays: 31}, {Name: "b", AgeDays: 50}, {Name: "a", AgeDays: 31}, {Name: "d", AgeDays: 60}},
			"4 folders have been in Staging for 30 days or more: d, b, a and c."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]staging.Folder(nil), tc.folders...)
			if got := staleText(tc.days, tc.folders); got != tc.want {
				t.Errorf("staleText = %q\nwant         %q", got, tc.want)
			}
			for i := range in {
				if in[i].Name != tc.folders[i].Name {
					t.Fatalf("staleText reordered its argument: %+v", tc.folders)
				}
			}
		})
	}
}

func TestCheckArchiveNoRepeatWrites(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 03:00")
	at := c.now().Add(-time.Hour)
	archive := func() indexer.Status {
		return indexer.Status{LastBackup: &indexer.BackupResult{At: at, Error: "disk full"}}
	}
	s := newService(t, Config{DB: db, Archive: archive, Now: c.now})
	s.check(ctx)
	if s.adds != 1 {
		t.Fatalf("first check: %d adds, want 1", s.adds)
	}
	for range 10 {
		s.check(ctx)
	}
	if s.adds != 1 {
		t.Fatalf("after ten more checks: %d adds, want 1", s.adds)
	}

	restarted := newService(t, Config{DB: db, Archive: archive, Now: c.now})
	for range 5 {
		restarted.check(ctx)
	}
	if restarted.adds != 1 {
		t.Fatalf("after a restart: %d adds, want 1", restarted.adds)
	}
	if ns := pending(t, db); len(ns) != 1 {
		t.Fatalf("pending = %+v, want one", ns)
	}
}

func TestCheckStagingWaitsForRescan(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 09:00")
	f := &fakeStaging{l: staleListing()}
	ready := false
	s := newService(t, Config{DB: db, Staging: f.list, StagingReady: func() bool { return ready }, Now: c.now})

	s.check(ctx)
	if f.calls != 0 || s.stagingDay != "" {
		t.Fatalf("not ready: %d calls, day %q", f.calls, s.stagingDay)
	}

	ready = true
	c.set(t, "2026-11-23 09:01")
	s.check(ctx)
	if f.calls != 1 || len(pending(t, db)) != 1 {
		t.Fatalf("ready: %d calls, pending %+v", f.calls, pending(t, db))
	}
}

func TestCheckStagingErrorRetriesHourly(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 09:00")
	f := &fakeStaging{l: staleListing(), err: errors.New("staging is unreadable")}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	s := newService(t, Config{DB: db, Staging: f.list, Now: c.now, Log: log})

	s.check(ctx)
	if f.calls != 1 || len(pending(t, db)) != 0 {
		t.Fatalf("failing: %d calls, pending %+v", f.calls, pending(t, db))
	}
	if !strings.Contains(logs.String(), "stale-staging check failed") || !strings.Contains(logs.String(), "staging is unreadable") {
		t.Fatalf("the failure was not logged: %s", logs.String())
	}

	f.err = nil
	c.set(t, "2026-11-23 09:30")
	s.check(ctx)
	if f.calls != 1 {
		t.Fatalf("at 09:30: %d calls, want 1", f.calls)
	}

	c.set(t, "2026-11-23 10:00")
	s.check(ctx)
	ns := pending(t, db)
	if f.calls != 2 || len(ns) != 1 || ns[0].Key != "staging:2026-11-23" {
		t.Fatalf("at 10:00: %d calls, pending %+v", f.calls, ns)
	}
}

func TestTakeAndClose(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 03:00")
	markMissing(t, db, 2, c.now().Add(-time.Minute))
	st := indexer.Status{
		LastBackup: &indexer.BackupResult{At: c.now().Add(-time.Hour), Error: "disk full"},
		Last:       &indexer.ScanResult{FinishedAt: c.now().Add(-time.Minute), Missing: 2},
		Missing:    2,
	}
	s := newService(t, Config{DB: db, Archive: func() indexer.Status { return st }, Now: c.now})
	s.check(ctx)

	got, err := s.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Take = %+v, want two", got)
	}
	left, err := s.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if left == nil || len(left) != 0 {
		t.Fatalf("Pending after Take = %#v, want empty", left)
	}

	s.Close()
	if _, err := s.Take(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Take after Close: %v, want ErrClosed", err)
	}
	if _, err := s.Pending(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Pending after Close: %v, want ErrClosed", err)
	}
}

func TestStartChecksAndStops(t *testing.T) {
	db := newDB(t)
	at := time.Now().Add(-time.Hour)
	archive := func() indexer.Status {
		return indexer.Status{LastBackup: &indexer.BackupResult{At: at, Error: "disk full"}}
	}
	s := newService(t, Config{DB: db, Archive: archive, Every: 10 * time.Millisecond})
	s.Start()
	s.Start() // a second call does nothing

	deadline := time.Now().Add(10 * time.Second)
	for {
		ns, err := s.Pending(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(ns) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no notice within 10 s: %+v", ns)
		}
		time.Sleep(10 * time.Millisecond)
	}

	closed := make(chan struct{})
	go func() {
		s.Close()
		s.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	s.Start() // after Close: does nothing
}

func TestMissingText(t *testing.T) {
	for n, want := range map[int]string{
		1: "The archive scan marked 1 file missing from Archive/. See the Archive index page.",
		2: "The archive scan marked 2 files missing from Archive/. See the Archive index page.",
	} {
		if got := missingText(n); got != want {
			t.Errorf("missingText(%d) = %q, want %q", n, got, want)
		}
	}
}

func setNoticeAt(t *testing.T, db *catalog.DB, at string) {
	t.Helper()
	if err := db.SetSetting(t.Context(), catalog.KeyStagingNoticeAt, at); err != nil {
		t.Fatal(err)
	}
}

func TestStagingNoticeAtSetting(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	setNoticeAt(t, db, "18:00")
	c := newClock(t, "2026-11-23 17:59")
	f := &fakeStaging{l: staleListing()}
	s := newService(t, Config{DB: db, Staging: f.list, Now: c.now})

	s.check(ctx)
	if f.calls != 0 || len(pending(t, db)) != 0 {
		t.Fatalf("at 17:59: %d calls, pending %+v", f.calls, pending(t, db))
	}
	c.set(t, "2026-11-23 18:00")
	s.check(ctx)
	ns := pending(t, db)
	if f.calls != 1 || len(ns) != 1 || ns[0].Key != "staging:2026-11-23" {
		t.Fatalf("at 18:00: %d calls, pending %+v", f.calls, ns)
	}
}

func TestStagingNoticeLaterAfterSent(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 09:00")
	f := &fakeStaging{l: staleListing()}
	s := newService(t, Config{DB: db, Staging: f.list, Now: c.now})
	s.check(ctx)
	if s.adds != 1 || len(pending(t, db)) != 1 {
		t.Fatalf("at 09:00: %d adds, pending %+v", s.adds, pending(t, db))
	}

	setNoticeAt(t, db, "20:00")
	s.SettingsChanged()
	c.set(t, "2026-11-23 20:01")
	s.check(ctx)
	if s.adds != 1 {
		t.Fatalf("at 20:01: %d adds, want 1", s.adds)
	}
	restarted := newService(t, Config{DB: db, Staging: f.list, Now: c.now})
	restarted.check(ctx)
	if restarted.adds != 0 || len(pending(t, db)) != 1 {
		t.Fatalf("after a restart at 20:01: %d adds, pending %+v", restarted.adds, pending(t, db))
	}
}

func TestStagingNoticeEarlierRunsNow(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 08:00")
	checks := make(chan struct{}, 100)
	archive := func() indexer.Status {
		select {
		case checks <- struct{}{}:
		default:
		}
		return indexer.Status{}
	}
	f := &fakeStaging{l: staleListing()}
	s := newService(t, Config{DB: db, Archive: archive, Staging: f.list, Now: c.now, Every: time.Hour})
	s.check(ctx) // reads 09:00 before the loop runs, so the loop re-reads only after SettingsChanged
	<-checks
	if ns := pending(t, db); len(ns) != 0 {
		t.Fatalf("at 08:00 with 09:00: pending %+v", ns)
	}

	setNoticeAt(t, db, "07:00")
	s.Start()
	<-checks // the loop's first check has started
	s.SettingsChanged()
	select {
	case <-checks: // within an hour only the nudge can start a second check
	case <-time.After(10 * time.Second):
		t.Fatal("SettingsChanged did not start a check within 10 s")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ns, err := s.Pending(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ns) == 1 && ns[0].Key == "staging:2026-11-23" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no notice within 10 s: %+v", ns)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNoticeAtReadOnce(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 10:00")
	f := &fakeStaging{l: staleListing()}
	s := newService(t, Config{DB: db, Staging: f.list, StagingReady: func() bool { return false }, Now: c.now})
	for range 100 {
		s.check(ctx)
	}
	if s.settingsReads != 1 {
		t.Fatalf("100 checks read the settings %d times, want 1", s.settingsReads)
	}
	s.SettingsChanged()
	for range 10 {
		s.check(ctx)
	}
	if s.settingsReads != 2 {
		t.Fatalf("after SettingsChanged: %d reads, want 2", s.settingsReads)
	}
	if f.calls != 0 {
		t.Fatalf("not ready, yet staging was listed %d times", f.calls)
	}
}

func TestNoticeAtReadFailure(t *testing.T) {
	ctx := context.Background()
	db, path := newDBPath(t)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Error(err)
		}
	})
	store := func(v string) {
		t.Helper()
		if _, err := raw.Exec(`INSERT INTO settings(key, value) VALUES ('staging_notice_at', ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, v); err != nil {
			t.Fatal(err)
		}
	}
	store("noon")
	c := newClock(t, "2026-11-23 08:59")
	f := &fakeStaging{l: staleListing()}
	var logs bytes.Buffer
	s := newService(t, Config{DB: db, Staging: f.list, Now: c.now, Log: slog.New(slog.NewTextHandler(&logs, nil))})

	s.check(ctx)
	if f.calls != 0 || len(pending(t, db)) != 0 {
		t.Fatalf("at 08:59 with the default 09:00: %d calls, pending %+v", f.calls, pending(t, db))
	}
	for _, want := range []string{"level=ERROR", `msg="could not read staging_notice_at; using the last value"`, "at=09:00", "noon"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log lacks %s: %s", want, logs.String())
		}
	}

	c.set(t, "2026-11-23 09:00")
	s.check(ctx)
	if s.settingsReads != 2 {
		t.Fatalf("the failed read was not retried: %d reads, want 2", s.settingsReads)
	}
	if ns := pending(t, db); len(ns) != 1 || ns[0].Key != "staging:2026-11-23" {
		t.Fatalf("at 09:00 with the default: pending %+v", ns)
	}

	store("07:00")
	c.set(t, "2026-11-24 07:00")
	s.check(ctx)
	if s.settingsReads != 3 {
		t.Fatalf("next day: %d reads, want 3", s.settingsReads)
	}
	if ns := pending(t, db); len(ns) != 2 || ns[1].Key != "staging:2026-11-24" {
		t.Fatalf("at 07:00 with 07:00 readable again: pending %+v", ns)
	}
}

// archiveStatus returns *st, which the test may change between checks.
func archiveStatus(st *indexer.Status) func() indexer.Status {
	return func() indexer.Status { return *st }
}

func TestMissingTwoPassesOneCheck(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 03:00")
	markMissing(t, db, 1, c.now().Add(-10*time.Minute))
	newest := c.now().Add(-5 * time.Minute)
	markMissing(t, db, 2, newest)
	st := indexer.Status{Last: &indexer.ScanResult{FinishedAt: newest, Missing: 2}, Missing: 3}
	s := newService(t, Config{DB: db, Archive: archiveStatus(&st), Now: c.now})
	s.check(ctx)
	ns := pending(t, db)
	if len(ns) != 1 {
		t.Fatalf("pending = %+v, want one", ns)
	}
	wantNotice(t, ns[0], missingKey(newest), catalog.NoticeAlert,
		"The archive scan marked 3 files missing from Archive/. See the Archive index page.")
}

func TestMissingAfterCrash(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 03:00")
	at := c.now().Add(-time.Hour)
	markMissing(t, db, 2, at)
	st := indexer.Status{Missing: 2} // the pass that marked them never finished
	s := newService(t, Config{DB: db, Archive: archiveStatus(&st), Now: c.now})
	s.check(ctx)
	ns := pending(t, db)
	if len(ns) != 1 {
		t.Fatalf("pending = %+v, want one", ns)
	}
	wantNotice(t, ns[0], missingKey(at), catalog.NoticeAlert,
		"The archive scan marked 2 files missing from Archive/. See the Archive index page.")
}

func TestMissingNotRepeatedAfterRestart(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 03:00")
	at := c.now().Add(-30 * time.Minute)
	markMissing(t, db, 1, at.Add(-10*time.Minute))
	markMissing(t, db, 1, at)
	if _, err := db.AddNotification(ctx, catalog.Notification{Key: missingKey(at), Level: catalog.NoticeAlert,
		Message: missingText(2), CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	st := indexer.Status{Last: &indexer.ScanResult{FinishedAt: at, Missing: 1}, Missing: 2}
	s := newService(t, Config{DB: db, Archive: archiveStatus(&st), Now: c.now})
	s.check(ctx)
	if s.adds != 0 || len(pending(t, db)) != 1 {
		t.Fatalf("after a restart: %d adds, pending %+v", s.adds, pending(t, db))
	}
}

func TestMissingOlderThanADay(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 03:00")
	markMissing(t, db, 2, c.now().Add(-25*time.Hour))
	st := indexer.Status{Missing: 2}
	s := newService(t, Config{DB: db, Archive: archiveStatus(&st), Now: c.now})
	s.check(ctx)
	if s.adds != 0 || len(pending(t, db)) != 0 {
		t.Fatalf("%d adds, pending %+v; want none", s.adds, pending(t, db))
	}
}

// TestMissingFloorEveryQuery: the MaxAge floor applies to each query, not
// only the first. A pass that runs longer than a day marks rows early on;
// when it finishes, the rows older than MaxAge are not notified.
func TestMissingFloorEveryQuery(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 03:00")
	start := c.now()
	st := indexer.Status{Last: &indexer.ScanResult{FinishedAt: start.Add(-time.Hour)}}
	s := newService(t, Config{DB: db, Archive: archiveStatus(&st), Now: c.now})
	s.check(ctx) // nothing missing; the watermark is now start - MaxAge
	if s.missingQueries != 1 || s.adds != 0 {
		t.Fatalf("first check: %d queries, %d adds", s.missingQueries, s.adds)
	}

	st = indexer.Status{Running: &indexer.Progress{StartedAt: start}}
	markMissing(t, db, 2, start.Add(time.Hour)) // early in a 30 h pass
	late := start.Add(29 * time.Hour)
	markMissing(t, db, 1, late)
	c.set(t, "2026-11-24 09:00") // start + 30 h
	st = indexer.Status{Last: &indexer.ScanResult{FinishedAt: c.now(), Missing: 3}, Missing: 3}
	s.check(ctx)
	ns := pending(t, db)
	if len(ns) != 1 {
		t.Fatalf("pending = %+v, want one", ns)
	}
	wantNotice(t, ns[0], missingKey(late), catalog.NoticeAlert,
		"The archive scan marked 1 file missing from Archive/. See the Archive index page.")
}

func TestMissingWaitsForPass(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 03:00")
	at := c.now().Add(-time.Minute)
	markMissing(t, db, 1, at)
	st := indexer.Status{Running: &indexer.Progress{StartedAt: at.Add(-time.Minute)}, Missing: 1}
	s := newService(t, Config{DB: db, Archive: archiveStatus(&st), Now: c.now})
	for range 3 {
		s.check(ctx)
	}
	if s.missingQueries != 0 || len(pending(t, db)) != 0 {
		t.Fatalf("while a pass runs: %d queries, pending %+v", s.missingQueries, pending(t, db))
	}
	st = indexer.Status{Last: &indexer.ScanResult{FinishedAt: at, Missing: 1}, Missing: 1}
	s.check(ctx)
	ns := pending(t, db)
	if len(ns) != 1 {
		t.Fatalf("after the pass: pending %+v, want one", ns)
	}
	wantNotice(t, ns[0], missingKey(at), catalog.NoticeAlert,
		"The archive scan marked 1 file missing from Archive/. See the Archive index page.")
}

func TestMissingQueriesOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	c := newClock(t, "2026-11-23 03:00")
	first := c.now().Add(-10 * time.Minute)
	markMissing(t, db, 2, first)
	st := indexer.Status{Last: &indexer.ScanResult{FinishedAt: first, Missing: 2}, Missing: 2}
	s := newService(t, Config{DB: db, Archive: archiveStatus(&st), Now: c.now})
	for range 50 {
		s.check(ctx)
	}
	if s.missingQueries != 1 || s.adds != 1 {
		t.Fatalf("50 unchanged checks: %d queries, %d adds; want 1, 1", s.missingQueries, s.adds)
	}

	second := c.now().Add(-time.Minute)
	markMissing(t, db, 1, second)
	st = indexer.Status{Last: &indexer.ScanResult{FinishedAt: second, Missing: 1}, Missing: 3}
	s.check(ctx)
	ns := pending(t, db)
	if s.missingQueries != 2 || len(ns) != 2 {
		t.Fatalf("after a new pass: %d queries, pending %+v", s.missingQueries, ns)
	}
	wantNotice(t, ns[1], missingKey(second), catalog.NoticeAlert,
		"The archive scan marked 1 file missing from Archive/. See the Archive index page.")
}

package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

func fullRescan(t *testing.T, s *Service) ScanResult {
	t.Helper()
	r, err := s.Rescan(t.Context(), TriggerManual, true)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFullReindexRereadsEverything(t *testing.T) {
	s, db, photos := setup(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		put(t, photos, "Archive/T/"+n+".JPG", "content "+n, t0)
	}
	put(t, photos, "Archive/T/copy-of-a.JPG", "content a", t0)
	wantCounts(t, rescan(t, s), ScanResult{Listed: 5, Added: 4, Copies: 1})
	before, err := db.ArchiveCounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if before.Files != 4 || before.Copies != 1 {
		t.Fatalf("counts before = %+v", before)
	}

	n := countHashes(s)
	s.beforeRead = func(_ context.Context, rel string) { // the index is never emptied
		c, err := db.ArchiveCounts(t.Context())
		if err != nil || c.Files != 4 || c.Copies != 1 {
			t.Errorf("counts while re-indexing %s = %+v, %v", rel, c, err)
		}
	}
	start := time.Now().Truncate(time.Second)
	r := fullRescan(t, s)
	if !r.Full {
		t.Error("Full = false")
	}
	wantCounts(t, r, ScanResult{Listed: 5, Unchanged: 5})
	noErrors(t, r)
	if n.Load() != 5 {
		t.Errorf("full reads = %d; want 5", n.Load())
	}
	if after, err := db.ArchiveCounts(t.Context()); err != nil || after != before {
		t.Errorf("counts after = %+v, %v; want %+v", after, err, before)
	}
	entries, err := db.ArchiveEntries(t.Context())
	if err != nil || len(entries) != 4 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	for _, e := range entries {
		if e.CheckedAt.Before(start) {
			t.Errorf("%s: checked_at %v is before the pass start %v", e.RelPath, e.CheckedAt, start)
		}
	}
	for _, c := range copyRows(t, db) {
		if c.CheckedAt.Before(start) {
			t.Errorf("copy %s: checked_at %v is before the pass start %v", c.RelPath, c.CheckedAt, start)
		}
	}
	if s.Status().PendingFull {
		t.Error("PendingFull after a finished re-index")
	}
}

func TestFullReindexFindsChange(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "aaaa", t0)
	put(t, photos, "Archive/T/b.JPG", "bbbb", t0)
	rescan(t, s)
	put(t, photos, "Archive/T/a.JPG", "AAAA", t0) // same size, same time
	n := countHashes(s)
	wantCounts(t, rescan(t, s), ScanResult{Listed: 2, Unchanged: 2})
	if n.Load() != 0 {
		t.Fatalf("a normal pass read %d files; want 0", n.Load())
	}

	r := fullRescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 2, Unchanged: 1, Changed: 1})
	noErrors(t, r)
	noAsset(t, db, "aaaa")
	wantAsset(t, db, "AAAA", catalog.StateArchived, "Archive/T/a.JPG")
	if a, err := db.AssetBySHA(t.Context(), sum("aaaa")); err != nil || a.SHA256 != sum("AAAA") {
		t.Errorf("AssetBySHA(old) = %+v, %v; want the alias to the new content", a, err)
	}
	if ch := changeEntries(t, db); len(ch) != 1 || ch[0].OldSHA256 != sum("aaaa") || ch[0].SHA256 != sum("AAAA") {
		t.Errorf("changes = %+v", ch)
	}
}

func TestFullReindexResumes(t *testing.T) {
	s, db, photos := setup(t)
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	for _, n := range names {
		put(t, photos, "Archive/T/"+n+".JPG", "content "+n, t0)
	}
	clock := newStepClock(s, time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	rescan(t, s)
	clock.add(time.Second)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var reads int
	s.beforeRead = func(_ context.Context, _ string) {
		clock.add(time.Millisecond) // each read is recorded after the re-index started
		reads++
		if reads == 5 {
			cancel() // after the fourth read
		}
	}
	r, err := s.Rescan(ctx, TriggerManual, true)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Stopped || !r.Full {
		t.Fatalf("result = Stopped %v Full %v", r.Stopped, r.Full)
	}
	if !s.Status().PendingFull {
		t.Error("PendingFull = false after a stopped re-index")
	}
	first, err := db.LastArchiveScan(t.Context())
	if err != nil || !first.Reread || first.RereadBefore.IsZero() {
		t.Fatalf("scan row = %+v, %v", first, err)
	}

	s.beforeRead = nil
	n := countHashes(s)
	r = rescan(t, s) // a normal pass continues it
	if !r.Full {
		t.Error("Full = false on the continuing pass")
	}
	wantCounts(t, r, ScanResult{Listed: 10, Unchanged: 10})
	if n.Load() != 6 {
		t.Errorf("full reads = %d; want 6", n.Load())
	}
	second, err := db.LastArchiveScan(t.Context())
	if err != nil || second.ID == first.ID || !second.RereadBefore.Equal(first.RereadBefore) {
		t.Errorf("reread_before %v; want %v (rows %d, %d, %v)", second.RereadBefore, first.RereadBefore, first.ID, second.ID, err)
	}
	if s.Status().PendingFull {
		t.Error("PendingFull after the re-index finished")
	}

	n2 := countHashes(s)
	r = rescan(t, s)
	if r.Full || n2.Load() != 0 {
		t.Errorf("the next normal pass: Full %v, reads %d; want a plain pass reading nothing", r.Full, n2.Load())
	}
}

func TestPendingFullLoadedAtStart(t *testing.T) {
	s, db, photos := setup(t)
	for _, n := range []string{"a", "b", "c"} {
		put(t, photos, "Archive/T/"+n+".JPG", "content "+n, t0)
	}
	rescan(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.beforeRead = func(_ context.Context, _ string) { cancel() }
	if _, err := s.Rescan(ctx, TriggerManual, true); err != nil {
		t.Fatal(err)
	}
	s2, err := New(Config{DB: db, PhotosRoot: photos})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s2.Close)
	if !s2.Status().PendingFull {
		t.Error("PendingFull = false after a restart")
	}
}

func TestFullReindexGoneFile(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "alpha", t0)
	put(t, photos, "Archive/T/b.JPG", "bravo", t0)
	rescan(t, s)
	remove(t, photos, "Archive/T/b.JPG")
	r := fullRescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Unchanged: 1, Missing: 1})
	noErrors(t, r)
	wantAsset(t, db, "bravo", catalog.StateMissing, "Archive/T/b.JPG")
}

// checked_at and the re-index cut-off are both cut to the millisecond, so a
// file the last pass read in the millisecond the re-index starts is read
// again, not taken as verified by the re-index.
func TestFullReindexRereadsFileCheckedAtItsStart(t *testing.T) {
	s, _, photos := setup(t)
	for _, n := range []string{"a", "b", "c"} {
		put(t, photos, "Archive/T/"+n+".JPG", "content "+n, t0)
	}
	clock := newStepClock(s, time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	rescan(t, s)
	clock.add(999 * time.Microsecond) // the re-index starts in the same millisecond
	n := countHashes(s)
	r := fullRescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 3, Unchanged: 3})
	noErrors(t, r)
	if n.Load() != 3 {
		t.Errorf("full reads = %d; want 3", n.Load())
	}
}

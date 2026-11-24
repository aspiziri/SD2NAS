package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

const missingMsg = "Archive/ is missing, so nothing was changed (is the photos share mounted?)"

// countHashes wraps s.hash and returns the number of full reads so far.
func countHashes(s *Service) *atomic.Int32 {
	var n atomic.Int32
	inner := s.hash
	s.hash = func(ctx context.Context, path string) (string, error) {
		n.Add(1)
		return inner(ctx, path)
	}
	return &n
}

func rescan(t *testing.T, s *Service) ScanResult {
	t.Helper()
	r, err := s.Rescan(t.Context(), TriggerManual, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func asset(t *testing.T, db *catalog.DB, content string) catalog.Asset {
	t.Helper()
	got, err := db.AssetsByHash(t.Context(), []string{sum(content)})
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got[sum(content)]
	if !ok {
		t.Fatalf("no asset for %q", content)
	}
	return a
}

func noAsset(t *testing.T, db *catalog.DB, content string) {
	t.Helper()
	got, err := db.AssetsByHash(t.Context(), []string{sum(content)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("unexpected asset for %q: %+v", content, got)
	}
}

func entry(t *testing.T, db *catalog.DB, rel string) catalog.ArchiveEntry {
	t.Helper()
	e, err := db.ArchiveEntryAt(t.Context(), rel)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func mtimeOf(t *testing.T, p string) int64 {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime().UnixNano()
}

func copyRows(t *testing.T, db *catalog.DB) []catalog.ArchiveCopy {
	t.Helper()
	c, err := db.ArchiveCopies(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func wantCounts(t *testing.T, got ScanResult, want ScanResult) {
	t.Helper()
	g := ScanResult{Listed: got.Listed, Unchanged: got.Unchanged, Added: got.Added, Moved: got.Moved, Changed: got.Changed,
		Reappeared: got.Reappeared, Recovered: got.Recovered, Missing: got.Missing, Copies: got.Copies,
		InStaging: got.InStaging, BusyFolders: got.BusyFolders}
	if !reflect.DeepEqual(g, want) {
		t.Errorf("counts = %+v; want %+v (errors %v, skipped %q)", g, want, got.Errors, got.Skipped)
	}
}

func TestBackfillIndexesArchive(t *testing.T) {
	s, db, photos := setup(t)
	files := []struct {
		rel, content string
		kind         media.Kind
	}{
		{"Archive/2025-05-30 - Trip/a.JPG", "alpha", media.KindImage},
		{"Archive/2025-05-30 - Trip/Capture/b.ARW", "bravo", media.KindRaw},
		{"Archive/2007-09-31 - Random Party/c.JPG", "charlie", media.KindImage},
	}
	for _, f := range files {
		put(t, photos, f.rel, f.content, t0)
	}
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Added: 3, Listed: 3})
	if len(r.Errors) != 0 || r.Skipped != "" || r.Stopped {
		t.Errorf("errors %v skipped %q stopped %v", r.Errors, r.Skipped, r.Stopped)
	}
	for _, f := range files {
		p := filepath.Join(photos, filepath.FromSlash(f.rel))
		a := asset(t, db, f.content)
		if a.State != catalog.StateArchived || a.RelPath != f.rel || a.Kind != f.kind || a.Size != int64(len(f.content)) ||
			a.Mtime != mtimeOf(t, p) || a.FirstIngest != 0 || !a.CaptureTime.IsZero() {
			t.Errorf("%s: %+v", f.rel, a)
		}
		if e := entry(t, db, f.rel); e.CheckedAt.IsZero() {
			t.Errorf("%s: checked_at is zero", f.rel)
		}
		fp, err := media.QuickFingerprint(p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := db.AssetsByFingerprint(t.Context(), fp)
		if err != nil || len(got) != 1 || got[0].SHA256 != a.SHA256 {
			t.Errorf("fingerprint of %s finds %v, %v", f.rel, got, err)
		}
	}
}

func TestRescanSkipsUnchanged(t *testing.T) {
	s, _, photos := setup(t)
	for _, c := range []string{"a", "b", "c"} {
		put(t, photos, "Archive/T/"+c+".JPG", c, t0)
	}
	rescan(t, s)
	n := countHashes(s)
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 3, Unchanged: 3})
	if n.Load() != 0 {
		t.Errorf("hash calls = %d; want 0", n.Load())
	}
}

func TestRescanSkipsPromotedFiles(t *testing.T) {
	s, db, photos := setup(t)
	p := put(t, photos, "Archive/T/a.JPG", "promoted", t0)
	in, err := db.CreateIngest(t.Context(), "/card", "card", "uuid-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	err = db.InsertAsset(t.Context(), catalog.Asset{SHA256: sum("promoted"), Size: int64(len("promoted")), Kind: media.KindImage,
		State: catalog.StateArchived, RelPath: "Archive/T/a.JPG", Mtime: mtimeOf(t, p), FirstIngest: in.ID, UpdatedAt: t0}, "")
	if err != nil {
		t.Fatal(err)
	}
	n := countHashes(s)
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Unchanged: 1})
	if n.Load() != 0 {
		t.Errorf("hash calls = %d; want 0", n.Load())
	}
	if a := asset(t, db, "promoted"); a.FirstIngest != in.ID {
		t.Errorf("first_ingest = %d; want %d", a.FirstIngest, in.ID)
	}
}

func TestPromoteFinishedMidPass(t *testing.T) {
	s, db, photos := setup(t)
	ctx := t.Context()
	f, err := db.CreateStagingFolder(ctx, "T", nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	p := put(t, photos, "Archive/T/x.JPG", "xray", t0) // the promote's rename already happened
	err = db.InsertAsset(ctx, catalog.Asset{SHA256: sum("xray"), Size: 4, Kind: media.KindImage, State: catalog.StateStaged,
		RelPath: "Staging/T/x.JPG", Mtime: mtimeOf(t, p), UpdatedAt: t0}, "")
	if err != nil {
		t.Fatal(err)
	}
	s.beforeRead = func(_ context.Context, rel string) {
		if rel == "Archive/T/x.JPG" {
			if _, err := db.ArchiveFolder(ctx, f.ID, "Staging/T/", "Archive/T/", t0); err != nil {
				t.Error(err)
			}
		}
	}
	n := countHashes(s)
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Unchanged: 1})
	if n.Load() != 0 {
		t.Errorf("hash calls = %d; want 0", n.Load())
	}
	if a := asset(t, db, "xray"); a.State != catalog.StateArchived || a.RelPath != "Archive/T/x.JPG" {
		t.Errorf("asset = %+v", a)
	}
	if c := copyRows(t, db); len(c) != 0 {
		t.Errorf("copy rows = %v", c)
	}
}

func TestNewFileCopiedInByHand(t *testing.T) {
	s, db, photos := setup(t)
	for _, c := range []string{"a", "b", "c"} {
		put(t, photos, "Archive/T/"+c+".JPG", c, t0)
	}
	rescan(t, s)
	put(t, photos, "Archive/T/d.JPG", "d", t0)
	n := countHashes(s)
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 4, Unchanged: 3, Added: 1})
	if n.Load() != 1 {
		t.Errorf("hash calls = %d; want 1", n.Load())
	}
	if a := asset(t, db, "d"); a.RelPath != "Archive/T/d.JPG" {
		t.Errorf("asset = %+v", a)
	}
}

func TestBackfillResumes(t *testing.T) {
	s, db, photos := setup(t)
	contents := []string{"one", "two", "three", "four", "five"}
	for i, c := range contents {
		put(t, photos, "Archive/T/"+string(rune('a'+i))+".JPG", c, t0)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var reads int
	s.beforeRead = func(_ context.Context, _ string) {
		reads++
		if reads == 3 {
			cancel()
		}
	}
	r, err := s.Rescan(ctx, TriggerManual, false)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Stopped {
		t.Errorf("Stopped = false; result %+v", r)
	}
	if r.Added != 2 {
		t.Errorf("Added = %d; want 2", r.Added)
	}
	asset(t, db, "one")
	asset(t, db, "two")
	noAsset(t, db, "three")

	s.beforeRead = nil
	n := countHashes(s)
	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 5, Unchanged: 2, Added: 3})
	if n.Load() != 3 {
		t.Errorf("hash calls = %d; want 3", n.Load())
	}
	for _, c := range contents {
		asset(t, db, c)
	}
}

func TestRescanAfterCloseRefused(t *testing.T) {
	s, _, _ := setup(t)
	s.Close()
	if _, err := s.Rescan(t.Context(), TriggerManual, false); !errors.Is(err, ErrClosed) {
		t.Errorf("Rescan after Close = %v; want ErrClosed", err)
	}
}

func TestCloseStopsPassMidFile(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "a", t0)
	started := make(chan struct{})
	s.hash = func(ctx context.Context, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	type out struct {
		r   ScanResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := s.Rescan(context.Background(), TriggerManual, false)
		done <- out{r, err}
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the hash never started")
	}
	s.Close()
	select {
	case o := <-done:
		if o.err != nil || !o.r.Stopped {
			t.Errorf("Rescan = %+v, %v; want Stopped and no error", o.r, o.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Rescan did not return after Close")
	}
	noAsset(t, db, "a")
}

func TestHashFileStopsWithContext(t *testing.T) {
	p := put(t, t.TempDir(), "big.bin", "some bytes", t0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := hashFile(ctx, p); !errors.Is(err, context.Canceled) {
		t.Errorf("hashFile with a cancelled context = %v", err)
	}
	got, err := hashFile(t.Context(), p)
	if err != nil || got != sum("some bytes") {
		t.Errorf("hashFile = %q, %v", got, err)
	}
	if _, err := hashFile(t.Context(), p+".nope"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("hashFile of a missing file = %v", err)
	}
}

func TestArchiveMissingChangesNothing(t *testing.T) {
	s, db, _ := setup(t)
	err := db.InsertAsset(t.Context(), catalog.Asset{SHA256: sum("a"), Size: 1, Kind: media.KindImage, State: catalog.StateArchived,
		RelPath: "Archive/T/a.JPG", Mtime: 5, UpdatedAt: t0}, "")
	if err != nil {
		t.Fatal(err)
	}
	r := rescan(t, s)
	if r.Skipped != missingMsg {
		t.Errorf("Skipped = %q; want %q", r.Skipped, missingMsg)
	}
	wantCounts(t, r, ScanResult{})
	if a := asset(t, db, "a"); a.State != catalog.StateArchived || a.RelPath != "Archive/T/a.JPG" || a.Mtime != 5 {
		t.Errorf("asset = %+v", a)
	}
}

func TestStagedContentLeftAlone(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/X/a.JPG", "staged-content", t0)
	err := db.InsertAsset(t.Context(), catalog.Asset{SHA256: sum("staged-content"), Size: 14, Kind: media.KindImage,
		State: catalog.StateStaged, RelPath: "Staging/X/a.JPG", Mtime: 7, UpdatedAt: t0}, "")
	if err != nil {
		t.Fatal(err)
	}
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, InStaging: 1})
	if a := asset(t, db, "staged-content"); a.State != catalog.StateStaged || a.RelPath != "Staging/X/a.JPG" {
		t.Errorf("asset = %+v", a)
	}
	if c := copyRows(t, db); len(c) != 0 {
		t.Errorf("copy rows = %v", c)
	}
}

func TestCulledContentRecovered(t *testing.T) {
	s, db, photos := setup(t)
	p := put(t, photos, "Archive/X/a.JPG", "culled-content", t0)
	err := db.InsertAsset(t.Context(), catalog.Asset{SHA256: sum("culled-content"), Size: 14, Kind: media.KindImage,
		State: catalog.StateCulled, UpdatedAt: t0}, "")
	if err != nil {
		t.Fatal(err)
	}
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Recovered: 1})
	if a := asset(t, db, "culled-content"); a.State != catalog.StateArchived || a.RelPath != "Archive/X/a.JPG" || a.Mtime != mtimeOf(t, p) {
		t.Errorf("asset = %+v", a)
	}
}

func TestCopyOfForgottenAssetFingerprinted(t *testing.T) {
	s, db, photos := setup(t)
	p := put(t, photos, "Archive/X/a.JPG", "forgotten", t0)
	err := db.PutArchiveCopy(t.Context(), catalog.ArchiveCopy{RelPath: "Archive/X/a.JPG", SHA256: sum("forgotten"), Size: 9,
		Mtime: mtimeOf(t, p), CheckedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	n := countHashes(s)
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Added: 1})
	if n.Load() != 0 {
		t.Errorf("hash calls = %d; want 0", n.Load())
	}
	if a := asset(t, db, "forgotten"); a.State != catalog.StateArchived || a.RelPath != "Archive/X/a.JPG" {
		t.Errorf("asset = %+v", a)
	}
	fp, err := media.QuickFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := db.AssetsByFingerprint(t.Context(), fp); err != nil || len(got) != 1 {
		t.Errorf("fingerprint finds %v, %v", got, err)
	}
}

func TestFileChangingWhileReadSkipped(t *testing.T) {
	s, db, photos := setup(t)
	p := put(t, photos, "Archive/T/a.JPG", "growing", t0)
	var once atomic.Bool
	s.afterHash = func(path string) {
		if path == p && once.CompareAndSwap(false, true) {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := f.WriteString(" more"); err != nil {
				t.Error(err)
			}
			if err := f.Close(); err != nil {
				t.Error(err)
			}
		}
	}
	r := rescan(t, s)
	want := []string{"Archive/T/a.JPG changed while it was read (still being copied?); the next scan picks it up"}
	if !reflect.DeepEqual(r.Errors, want) {
		t.Errorf("errors = %q; want %q", r.Errors, want)
	}
	wantCounts(t, r, ScanResult{Listed: 1})
	noAsset(t, db, "growing")
	noAsset(t, db, "growing more")

	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Added: 1})
	if len(r.Errors) != 0 {
		t.Errorf("errors = %q", r.Errors)
	}
	asset(t, db, "growing more")
}

func TestTouchedFileOnlyUpdatesMtime(t *testing.T) {
	s, db, photos := setup(t)
	p := put(t, photos, "Archive/T/a.JPG", "same", t0)
	rescan(t, s)
	t1 := t0.Add(time.Hour)
	if err := os.Chtimes(p, t1, t1); err != nil {
		t.Fatal(err)
	}
	n := countHashes(s)
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Unchanged: 1})
	if n.Load() != 1 {
		t.Errorf("hash calls = %d; want 1", n.Load())
	}
	if a := asset(t, db, "same"); a.Mtime != t1.UnixNano() || a.RelPath != "Archive/T/a.JPG" {
		t.Errorf("asset = %+v", a)
	}
	if _, total, _, err := db.ArchiveChanges(t.Context(), 10); err != nil || total != 0 {
		t.Errorf("changes = %d, %v", total, err)
	}
	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Unchanged: 1})
	if n.Load() != 1 {
		t.Errorf("hash calls after the second pass = %d; want 1", n.Load())
	}
}

func TestEditedFileRecordsChange(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "before", t0)
	rescan(t, s)
	put(t, photos, "Archive/T/a.JPG", "after!", t0.Add(time.Hour))
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Changed: 1})
	noAsset(t, db, "before")
	if a := asset(t, db, "after!"); a.RelPath != "Archive/T/a.JPG" || a.State != catalog.StateArchived {
		t.Errorf("asset = %+v", a)
	}
	if ch, total, _, err := db.ArchiveChanges(t.Context(), 10); err != nil || total != 1 || ch[0].OldSHA256 != sum("before") {
		t.Errorf("changes = %+v, %d, %v", ch, total, err)
	}
}

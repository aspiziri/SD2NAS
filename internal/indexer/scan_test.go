package indexer

import (
	"context"
	"errors"
	"io/fs"
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

func TestCopyRowHashNotTrustedForReplacedFile(t *testing.T) {
	s, db, photos := setup(t)
	p := put(t, photos, "Archive/X/a.JPG", "old-content", t0)
	err := db.PutArchiveCopy(t.Context(), catalog.ArchiveCopy{RelPath: "Archive/X/a.JPG", SHA256: sum("old-content"),
		Size: 11, Mtime: mtimeOf(t, p), CheckedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	s.beforeRead = func(_ context.Context, rel string) { // the file is replaced after the listing
		put(t, photos, rel, "replacement-content", t0.Add(time.Hour))
	}
	n := countHashes(s)
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Added: 1})
	if n.Load() != 1 {
		t.Errorf("hash calls = %d; want 1", n.Load())
	}
	noAsset(t, db, "old-content")
	if a := asset(t, db, "replacement-content"); a.RelPath != "Archive/X/a.JPG" || a.Size != 19 {
		t.Errorf("asset = %+v", a)
	}
}

func TestPromoteFinishedAfterHashNeverCopyOfItself(t *testing.T) {
	s, db, photos := setup(t)
	ctx := t.Context()
	f, err := db.CreateStagingFolder(ctx, "T", nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	p := put(t, photos, "Archive/T/x.JPG", "xray", t0)
	err = db.InsertAsset(ctx, catalog.Asset{SHA256: sum("xray"), Size: 4, Kind: media.KindImage, State: catalog.StateStaged,
		RelPath: "Staging/T/x.JPG", Mtime: 7, UpdatedAt: t0}, "")
	if err != nil {
		t.Fatal(err)
	}
	s.afterHash = func(string) { // the promote's catalog write lands after the file was read
		if _, err := db.ArchiveFolder(ctx, f.ID, "Staging/T/", "Archive/T/", t0); err != nil {
			t.Error(err)
		}
	}
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Unchanged: 1})
	if a := asset(t, db, "xray"); a.State != catalog.StateArchived || a.RelPath != "Archive/T/x.JPG" || a.Mtime != mtimeOf(t, p) {
		t.Errorf("asset = %+v", a)
	}
	if c := copyRows(t, db); len(c) != 0 {
		t.Errorf("copy rows = %v", c)
	}
}

// countFingerprints wraps s.fingerprint and returns the number of quick reads so far.
func countFingerprints(s *Service) *atomic.Int32 {
	var n atomic.Int32
	inner := s.fingerprint
	s.fingerprint = func(path string) (string, error) {
		n.Add(1)
		return inner(path)
	}
	return &n
}

// archivedRow records content as archived at rel with no file behind it.
func archivedRow(t *testing.T, db *catalog.DB, rel, content string) {
	t.Helper()
	err := db.InsertAsset(t.Context(), catalog.Asset{SHA256: sum(content), Size: int64(len(content)), Kind: media.KindImage,
		State: catalog.StateArchived, RelPath: rel, Mtime: t0.UnixNano(), UpdatedAt: t0}, "")
	if err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, photos, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(photos, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

func rename(t *testing.T, photos, from, to string) {
	t.Helper()
	dst := filepath.Join(photos, filepath.FromSlash(to))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(photos, filepath.FromSlash(from)), dst); err != nil {
		t.Fatal(err)
	}
}

// wantAsset checks the state and rel_path of the asset holding content.
func wantAsset(t *testing.T, db *catalog.DB, content string, state catalog.AssetState, rel string) {
	t.Helper()
	if a := asset(t, db, content); a.State != state || a.RelPath != rel {
		t.Errorf("asset %q = %s at %s; want %s at %s", content, a.State, a.RelPath, state, rel)
	}
}

func changeEntries(t *testing.T, db *catalog.DB) []catalog.ArchiveChange {
	t.Helper()
	ch, _, _, err := db.ArchiveChanges(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func noErrors(t *testing.T, r ScanResult) {
	t.Helper()
	if len(r.Errors) != 0 || r.Skipped != "" || r.Stopped {
		t.Errorf("errors %q skipped %q stopped %v", r.Errors, r.Skipped, r.Stopped)
	}
}

func TestMoveWithinSession(t *testing.T) {
	s, db, photos := setup(t)
	const S = "Archive/2025-05-30 - Trip/"
	put(t, photos, S+"a.JPG", "alpha", t0)
	put(t, photos, S+"Capture/b.ARW", "bravo", t0)
	rescan(t, s)
	rename(t, photos, S+"Capture/b.ARW", S+"Selects/b.ARW")
	n := countHashes(s)
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 2, Unchanged: 1, Moved: 1})
	noErrors(t, r)
	if n.Load() != 0 {
		t.Errorf("hash calls = %d; want 0", n.Load())
	}
	wantAsset(t, db, "bravo", catalog.StateArchived, S+"Selects/b.ARW")
	if a := asset(t, db, "bravo"); a.Mtime != t0.UnixNano() {
		t.Errorf("mtime = %d; want %d", a.Mtime, t0.UnixNano())
	}
}

func TestSessionRenamedByHand(t *testing.T) {
	s, db, photos := setup(t)
	files := map[string]string{"a.JPG": "alpha", "Capture/b.ARW": "bravo", "Selects/c.JPG": "charlie"}
	for name, c := range files {
		put(t, photos, "Archive/2025-05-30 - Trip/"+name, c, t0)
	}
	rescan(t, s)
	rename(t, photos, "Archive/2025-05-30 - Trip", "Archive/2025-05-30 - Coast Trip")
	n := countHashes(s)
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 3, Moved: 3})
	noErrors(t, r)
	if n.Load() != 0 {
		t.Errorf("hash calls = %d; want 0", n.Load())
	}
	for name, c := range files {
		wantAsset(t, db, c, catalog.StateArchived, "Archive/2025-05-30 - Coast Trip/"+name)
	}
}

func TestMovedAndChangedIsNewPlusMissing(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/Capture/b.ARW", "bravo", t0)
	rescan(t, s)
	remove(t, photos, "Archive/T/Capture/b.ARW")
	put(t, photos, "Archive/T/Selects/b.ARW", "bravo, edited", t0.Add(time.Hour))
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Added: 1, Missing: 1})
	noErrors(t, r)
	wantAsset(t, db, "bravo", catalog.StateMissing, "Archive/T/Capture/b.ARW")
	wantAsset(t, db, "bravo, edited", catalog.StateArchived, "Archive/T/Selects/b.ARW")
}

func TestRescanChangedContent(t *testing.T) {
	s, db, photos := setup(t)
	p := put(t, photos, "Archive/T/a.JPG", "before", t0)
	rescan(t, s)
	oldFP, err := media.QuickFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	put(t, photos, "Archive/T/a.JPG", "after, longer", t0.Add(time.Hour))
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Changed: 1})
	noErrors(t, r)
	noAsset(t, db, "before")
	wantAsset(t, db, "after, longer", catalog.StateArchived, "Archive/T/a.JPG")
	if a, err := db.AssetBySHA(t.Context(), sum("before")); err != nil || a.SHA256 != sum("after, longer") {
		t.Errorf("AssetBySHA(old) = %+v, %v", a, err)
	}
	ch := changeEntries(t, db)
	if len(ch) != 1 || ch[0].RelPath != "Archive/T/a.JPG" || ch[0].OldSHA256 != sum("before") || ch[0].SHA256 != sum("after, longer") ||
		ch[0].OldSize != 6 || ch[0].Size != 13 {
		t.Errorf("changes = %+v", ch)
	}
	if got, err := db.AssetsByFingerprint(t.Context(), oldFP); err != nil || len(got) != 1 || got[0].SHA256 != sum("after, longer") {
		t.Errorf("old fingerprint finds %+v, %v", got, err)
	}
}

func TestChangeReverted(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "version-a", t0)
	rescan(t, s)
	put(t, photos, "Archive/T/a.JPG", "version-b", t0.Add(time.Hour))
	if r := rescan(t, s); r.Changed != 1 {
		t.Fatalf("first change: %+v", r)
	}
	put(t, photos, "Archive/T/a.JPG", "version-a", t0.Add(2*time.Hour))
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Changed: 1})
	noErrors(t, r)
	wantAsset(t, db, "version-a", catalog.StateArchived, "Archive/T/a.JPG")
	if a, err := db.AssetBySHA(t.Context(), sum("version-b")); err != nil || a.SHA256 != sum("version-a") {
		t.Errorf("AssetBySHA(B) = %+v, %v", a, err)
	}
	if ch := changeEntries(t, db); len(ch) != 0 {
		t.Errorf("changes = %+v", ch)
	}
}

func TestChangedToKnownContent(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/p.JPG", "pppp", t0)
	put(t, photos, "Archive/T/q.JPG", "qqqq", t0)
	rescan(t, s)
	put(t, photos, "Archive/T/p.JPG", "qqqq", t0.Add(time.Hour))
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 2, Unchanged: 1, Copies: 1, Missing: 1})
	noErrors(t, r)
	wantAsset(t, db, "pppp", catalog.StateMissing, "Archive/T/p.JPG")
	wantAsset(t, db, "qqqq", catalog.StateArchived, "Archive/T/q.JPG")
	if c := copyRows(t, db); len(c) != 1 || c[0].RelPath != "Archive/T/p.JPG" || c[0].SHA256 != sum("qqqq") {
		t.Errorf("copy rows = %+v", c)
	}
	if ch := changeEntries(t, db); len(ch) != 0 {
		t.Errorf("changes = %+v", ch)
	}
	n, fp := countHashes(s), countFingerprints(s)
	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 2, Unchanged: 2})
	if n.Load() != 0 || fp.Load() != 0 {
		t.Errorf("reads after: %d hashes, %d fingerprints; want none", n.Load(), fp.Load())
	}
}

func TestDuplicateRecordedAsCopy(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "dup", t0)
	put(t, photos, "Archive/T/b.JPG", "dup", t0)
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 2, Added: 1, Copies: 1})
	wantAsset(t, db, "dup", catalog.StateArchived, "Archive/T/a.JPG")
	if c := copyRows(t, db); len(c) != 1 || c[0].RelPath != "Archive/T/b.JPG" || c[0].SHA256 != sum("dup") {
		t.Fatalf("copy rows = %+v", c)
	}

	n, fp := countHashes(s), countFingerprints(s)
	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 2, Unchanged: 2})
	if n.Load() != 0 || fp.Load() != 0 {
		t.Errorf("reads: %d hashes, %d fingerprints; want none", n.Load(), fp.Load())
	}

	remove(t, photos, "Archive/T/a.JPG")
	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Unchanged: 1, Moved: 1})
	noErrors(t, r)
	wantAsset(t, db, "dup", catalog.StateArchived, "Archive/T/b.JPG")
	if c := copyRows(t, db); len(c) != 0 {
		t.Errorf("copy rows after the primary went = %+v", c)
	}

	remove(t, photos, "Archive/T/b.JPG")
	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Missing: 1})
	wantAsset(t, db, "dup", catalog.StateMissing, "Archive/T/b.JPG")

	// Deleting a copy while its primary stays only drops the copy row.
	put(t, photos, "Archive/U/x.JPG", "dup2", t0)
	put(t, photos, "Archive/U/y.JPG", "dup2", t0)
	rescan(t, s)
	remove(t, photos, "Archive/U/y.JPG")
	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Unchanged: 1})
	noErrors(t, r)
	wantAsset(t, db, "dup2", catalog.StateArchived, "Archive/U/x.JPG")
	if c := copyRows(t, db); len(c) != 0 {
		t.Errorf("copy rows after the copy went = %+v", c)
	}
}

func TestPrimaryGoneMovesToCopy(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "original", t0)
	put(t, photos, "Archive/T/c.JPG", "original", t0)
	rescan(t, s)
	put(t, photos, "Archive/T/a.JPG", "edited!!", t0.Add(time.Hour))
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 2, Unchanged: 1, Added: 1, Moved: 1})
	noErrors(t, r)
	wantAsset(t, db, "original", catalog.StateArchived, "Archive/T/c.JPG")
	wantAsset(t, db, "edited!!", catalog.StateArchived, "Archive/T/a.JPG")
	if c := copyRows(t, db); len(c) != 0 {
		t.Errorf("copy rows = %+v", c)
	}
	if ch := changeEntries(t, db); len(ch) != 0 {
		t.Errorf("changes = %+v", ch)
	}
}

// A copy made in the same pass as the edit counts too: the hashing phase
// reads new paths before changed ones.
func TestCopiedThenEditedInPlace(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "original", t0)
	rescan(t, s)
	put(t, photos, "Archive/T/c.JPG", "original", t0)
	put(t, photos, "Archive/T/a.JPG", "edited!!", t0.Add(time.Hour))
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 2, Added: 1, Moved: 1})
	noErrors(t, r)
	wantAsset(t, db, "original", catalog.StateArchived, "Archive/T/c.JPG")
	wantAsset(t, db, "edited!!", catalog.StateArchived, "Archive/T/a.JPG")
	if c := copyRows(t, db); len(c) != 0 {
		t.Errorf("copy rows = %+v", c)
	}
	if ch := changeEntries(t, db); len(ch) != 0 {
		t.Errorf("changes = %+v", ch)
	}
}

// The cost of reading new paths first: when the edited content is also
// duplicated to a new path in the same pass, the new path gets the edit, the
// edited path becomes its copy, and the old content is missing (no change entry).
func TestEditDuplicatedInSamePass(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "original", t0)
	rescan(t, s)
	put(t, photos, "Archive/T/a.JPG", "edited!!", t0.Add(time.Hour))
	put(t, photos, "Archive/T/c.JPG", "edited!!", t0.Add(time.Hour))
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 2, Added: 1, Copies: 1, Missing: 1})
	noErrors(t, r)
	wantAsset(t, db, "edited!!", catalog.StateArchived, "Archive/T/c.JPG")
	wantAsset(t, db, "original", catalog.StateMissing, "Archive/T/a.JPG")
	if c := copyRows(t, db); len(c) != 1 || c[0].RelPath != "Archive/T/a.JPG" || c[0].SHA256 != sum("edited!!") {
		t.Errorf("copy rows = %+v", c)
	}
	if ch := changeEntries(t, db); len(ch) != 0 {
		t.Errorf("changes = %+v", ch)
	}
}

// Content the catalog knows only as an alias (an earlier version of an
// asset changed in place) found at a path of its own becomes an asset again.
func TestEarlierVersionIndexedAgain(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "original", t0)
	rescan(t, s)
	put(t, photos, "Archive/T/a.JPG", "edited!!", t0.Add(time.Hour))
	rescan(t, s)
	put(t, photos, "Archive/T/c.JPG", "original", t0)
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 2, Unchanged: 1, Added: 1})
	noErrors(t, r)
	wantAsset(t, db, "original", catalog.StateArchived, "Archive/T/c.JPG")
	wantAsset(t, db, "edited!!", catalog.StateArchived, "Archive/T/a.JPG")
	if a, err := db.AssetBySHA(t.Context(), sum("original")); err != nil || a.SHA256 != sum("original") {
		t.Errorf("AssetBySHA(original) = %+v, %v; want the original itself", a, err)
	}
	n, fp := countHashes(s), countFingerprints(s)
	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 2, Unchanged: 2})
	if n.Load() != 0 || fp.Load() != 0 {
		t.Errorf("reads after: %d hashes, %d fingerprints; want none", n.Load(), fp.Load())
	}
}

// A revert while a copy of the edit exists: the path gets the original back
// as an asset, and the edit moves to its copy.
func TestRevertWithCopyOfEdit(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "version-a", t0)
	rescan(t, s)
	put(t, photos, "Archive/T/a.JPG", "version-b!", t0.Add(time.Hour))
	rescan(t, s)
	put(t, photos, "Archive/T/d.JPG", "version-b!", t0.Add(time.Hour))
	if r := rescan(t, s); r.Copies != 1 {
		t.Fatalf("copy of the edit: %+v", r)
	}
	put(t, photos, "Archive/T/a.JPG", "version-a", t0.Add(2*time.Hour))
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 2, Unchanged: 1, Added: 1, Moved: 1})
	noErrors(t, r)
	wantAsset(t, db, "version-a", catalog.StateArchived, "Archive/T/a.JPG")
	wantAsset(t, db, "version-b!", catalog.StateArchived, "Archive/T/d.JPG")
	if c := copyRows(t, db); len(c) != 0 {
		t.Errorf("copy rows = %+v", c)
	}
	n, fp := countHashes(s), countFingerprints(s)
	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 2, Unchanged: 2})
	if n.Load() != 0 || fp.Load() != 0 {
		t.Errorf("reads after: %d hashes, %d fingerprints; want none", n.Load(), fp.Load())
	}
}

// A file moved over another archived file, with the pass stopped before its
// checking phase, leaves two archived rows at one path; the next complete
// pass marks the overwritten content missing whichever row it keeps.
func TestMoveOverFileStoppedBeforeCheck(t *testing.T) {
	for _, tc := range []struct{ from, to, gone string }{
		{"b.JPG", "a.JPG", "aaaa"},
		{"a.JPG", "b.JPG", "bbbbbbbb"},
	} {
		t.Run(tc.from+" over "+tc.to, func(t *testing.T) {
			s, db, photos := setup(t)
			put(t, photos, "Archive/T/a.JPG", "aaaa", t0)
			put(t, photos, "Archive/T/b.JPG", "bbbbbbbb", t0)
			put(t, photos, "Archive/T/z.JPG", "zzzz", t0)
			rescan(t, s)
			rename(t, photos, "Archive/T/"+tc.from, "Archive/T/"+tc.to)
			put(t, photos, "Archive/T/z.JPG", "zzzz-changed", t0.Add(time.Hour)) // a later changed path to stop on
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s.beforeRead = func(_ context.Context, rel string) {
				if rel == "Archive/T/z.JPG" {
					cancel()
				}
			}
			r, err := s.Rescan(ctx, TriggerManual, false)
			if err != nil {
				t.Fatal(err)
			}
			if !r.Stopped || r.Moved != 1 {
				t.Fatalf("stopped pass = %+v; want Stopped after the move", r)
			}
			s.beforeRead = nil
			r = rescan(t, s)
			if r.Missing != 1 || len(r.Errors) != 0 {
				t.Errorf("next pass = %+v; want the overwritten content missing", r)
			}
			wantAsset(t, db, tc.gone, catalog.StateMissing, "Archive/T/"+tc.to)
			r = rescan(t, s)
			wantCounts(t, r, ScanResult{Listed: 2, Unchanged: 2})
		})
	}
}

// A change in place hands the old content's fingerprint to the new asset;
// a same-size edit must not let the old bytes pass for the new content.
func TestMoveShortcutNeedsSameMtime(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "version-a", t0)
	rescan(t, s)
	put(t, photos, "Archive/T/a.JPG", "version-b", t0.Add(time.Hour))
	if r := rescan(t, s); r.Changed != 1 {
		t.Fatalf("change: %+v", r)
	}
	remove(t, photos, "Archive/T/a.JPG")
	put(t, photos, "Archive/T/c.JPG", "version-a", t0)
	n := countHashes(s)
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Added: 1, Missing: 1})
	if n.Load() != 1 {
		t.Errorf("hash calls = %d; want 1", n.Load())
	}
	noErrors(t, r)
	wantAsset(t, db, "version-a", catalog.StateArchived, "Archive/T/c.JPG")
	wantAsset(t, db, "version-b", catalog.StateMissing, "Archive/T/a.JPG")
}

func TestGoneFileMarkedMissing(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "alpha", t0)
	put(t, photos, "Archive/T/b.JPG", "bravo", t0)
	rescan(t, s)
	remove(t, photos, "Archive/T/b.JPG")
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Unchanged: 1, Missing: 1})
	noErrors(t, r)
	wantAsset(t, db, "bravo", catalog.StateMissing, "Archive/T/b.JPG")
	m, err := db.MissingFiles(t.Context(), 10)
	if err != nil || len(m) != 1 || m[0].RelPath != "Archive/T/b.JPG" || m[0].Size != 5 {
		t.Errorf("MissingFiles = %+v, %v", m, err)
	}
}

func TestMissingReappears(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "alpha", t0)
	rescan(t, s)
	remove(t, photos, "Archive/T/a.JPG")
	if r := rescan(t, s); r.Missing != 1 {
		t.Fatalf("missing: %+v", r)
	}

	put(t, photos, "Archive/T/a.JPG", "alpha", t0)
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Reappeared: 1})
	noErrors(t, r)
	wantAsset(t, db, "alpha", catalog.StateArchived, "Archive/T/a.JPG")

	remove(t, photos, "Archive/T/a.JPG")
	if r := rescan(t, s); r.Missing != 1 {
		t.Fatalf("missing again: %+v", r)
	}
	put(t, photos, "Archive/U/moved.JPG", "alpha", t0)
	n := countHashes(s)
	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Reappeared: 1})
	noErrors(t, r)
	if n.Load() != 0 {
		t.Errorf("hash calls = %d; want 0", n.Load())
	}
	wantAsset(t, db, "alpha", catalog.StateArchived, "Archive/U/moved.JPG")
}

func TestArchiveVanishesBeforeMissing(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "alpha", t0)
	put(t, photos, "Archive/T/b.JPG", "bravo", t0)
	rescan(t, s)
	remove(t, photos, "Archive/T/a.JPG")
	remove(t, photos, "Archive/T/b.JPG")
	var calls int
	s.beforeMissing = func(string) {
		calls++
		if calls == 1 { // the share drops: on Windows that reads as "file not found"
			if err := os.Rename(filepath.Join(photos, "Archive"), filepath.Join(photos, "Archive-away")); err != nil {
				t.Error(err)
			}
		}
	}
	r := rescan(t, s)
	const want = "Archive/ went missing during the scan, so it was stopped (is the photos share still mounted?)"
	if r.Skipped != want {
		t.Errorf("Skipped = %q; want %q", r.Skipped, want)
	}
	if calls != 1 {
		t.Errorf("beforeMissing calls = %d; want 1", calls)
	}
	wantCounts(t, r, ScanResult{})
	wantAsset(t, db, "alpha", catalog.StateArchived, "Archive/T/a.JPG")
	wantAsset(t, db, "bravo", catalog.StateArchived, "Archive/T/b.JPG")
}

func TestFileBackBeforeMissing(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/T/a.JPG", "alpha", t0)
	rescan(t, s)
	remove(t, photos, "Archive/T/a.JPG")
	s.beforeMissing = func(rel string) { put(t, photos, rel, "alpha", t0) }
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{})
	noErrors(t, r)
	wantAsset(t, db, "alpha", catalog.StateArchived, "Archive/T/a.JPG")
}

func TestStoppedPassMarksNothingMissing(t *testing.T) {
	s, db, photos := setup(t)
	archivedRow(t, db, "Archive/T/gone.JPG", "gone")
	put(t, photos, "Archive/T/a.JPG", "alpha", t0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.beforeRead = func(context.Context, string) { cancel() }
	r, err := s.Rescan(ctx, TriggerManual, false)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Stopped || r.Missing != 0 {
		t.Errorf("result = %+v; want Stopped and nothing missing", r)
	}
	wantAsset(t, db, "gone", catalog.StateArchived, "Archive/T/gone.JPG")
}

func TestSkippedRowsNeverMissing(t *testing.T) {
	s, db, photos := setup(t)
	if err := os.MkdirAll(filepath.Join(photos, "Archive", "S"), 0o755); err != nil {
		t.Fatal(err)
	}
	kept := map[string]string{"Archive/S/Output/x.JPG": "x", "Archive/S/Capture/CaptureOne/y.JPG": "y", "Archive/S/@eaDir/z.JPG": "z"}
	for rel, c := range kept {
		archivedRow(t, db, rel, c)
	}
	archivedRow(t, db, "Archive/S/gone.JPG", "gone")
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Missing: 1})
	noErrors(t, r)
	for rel, c := range kept {
		wantAsset(t, db, c, catalog.StateArchived, rel)
	}
	wantAsset(t, db, "gone", catalog.StateMissing, "Archive/S/gone.JPG")
}

func TestNewSkipRuleKeepsIndexedRows(t *testing.T) {
	s, db, photos := setup(t)
	ctx := t.Context()
	const S = "Archive/S/"
	put(t, photos, S+"Edits/a.JPG", "a-edit", t0)
	put(t, photos, S+"x.JPG", "x", t0)
	rescan(t, s)

	put(t, photos, S+"Edits/b.JPG", "b-edit", t0)
	var set bool
	s.beforeRead = func(_ context.Context, rel string) {
		if !set {
			set = true
			if err := db.SetSetting(ctx, catalog.KeyArchiveSkip, catalog.DefaultArchiveSkip+", Edits/"); err != nil {
				t.Error(err)
			}
		}
	}
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 3, Unchanged: 2, Added: 1})
	if !set {
		t.Fatal("the setting was never changed")
	}
	wantAsset(t, db, "b-edit", catalog.StateArchived, S+"Edits/b.JPG")

	put(t, photos, S+"Edits/c.JPG", "c-edit", t0)
	remove(t, photos, S+"Edits/a.JPG")
	var reads []string
	s.beforeRead = func(_ context.Context, rel string) { reads = append(reads, rel) }
	n, fp := countHashes(s), countFingerprints(s)
	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Unchanged: 1})
	noErrors(t, r)
	if len(reads) != 0 || n.Load() != 0 || fp.Load() != 0 {
		t.Errorf("reads %v, %d hashes, %d fingerprints; want none", reads, n.Load(), fp.Load())
	}
	wantAsset(t, db, "a-edit", catalog.StateArchived, S+"Edits/a.JPG")
	wantAsset(t, db, "b-edit", catalog.StateArchived, S+"Edits/b.JPG")
	noAsset(t, db, "c-edit")

	if err := db.SetSetting(ctx, catalog.KeyArchiveSkip, catalog.DefaultArchiveSkip); err != nil {
		t.Fatal(err)
	}
	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 3, Unchanged: 2, Added: 1, Missing: 1})
	noErrors(t, r)
	wantAsset(t, db, "c-edit", catalog.StateArchived, S+"Edits/c.JPG")
	wantAsset(t, db, "a-edit", catalog.StateMissing, S+"Edits/a.JPG")
	wantAsset(t, db, "b-edit", catalog.StateArchived, S+"Edits/b.JPG")
}

func TestUnreadableFolderNotMissing(t *testing.T) {
	s, db, photos := setup(t)
	put(t, photos, "Archive/A/a.JPG", "alpha", t0)
	put(t, photos, "Archive/B/b.JPG", "bravo", t0)
	put(t, photos, "Archive/B/c.JPG", "charlie", t0)
	rescan(t, s)
	remove(t, photos, "Archive/B/c.JPG")
	realReadDir := s.readDir
	s.readDir = func(p string) ([]os.DirEntry, error) {
		if filepath.Base(p) == "A" {
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission}
		}
		return realReadDir(p)
	}
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Unchanged: 1, Missing: 1})
	if len(r.Errors) != 1 {
		t.Errorf("errors = %q; want one problem", r.Errors)
	}
	wantAsset(t, db, "alpha", catalog.StateArchived, "Archive/A/a.JPG")
	wantAsset(t, db, "charlie", catalog.StateMissing, "Archive/B/c.JPG")
}

func TestPromoteHeldFolderSkipped(t *testing.T) {
	s, db, photos := setup(t)
	locks := newFakeLocks()
	s.locks = locks
	const S = "Archive/2025-05-30 - Trip/"
	put(t, photos, S+"new.JPG", "new", t0)
	archivedRow(t, db, S+"gone.JPG", "gone")
	locks.hold("Archive/2025-05-30 - Trip")
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{BusyFolders: 1})
	noErrors(t, r)
	noAsset(t, db, "new")
	wantAsset(t, db, "gone", catalog.StateArchived, S+"gone.JPG")

	locks.release("Archive/2025-05-30 - Trip")
	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 1, Added: 1, Missing: 1})
	noErrors(t, r)
	wantAsset(t, db, "new", catalog.StateArchived, S+"new.JPG")
	wantAsset(t, db, "gone", catalog.StateMissing, S+"gone.JPG")
}

func TestHalfPromotedFolder(t *testing.T) {
	s, db, photos := setup(t)
	ctx := t.Context()
	locks := newFakeLocks()
	s.locks = locks
	f, err := db.CreateStagingFolder(ctx, "T", nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"x.JPG": "xray", "y.JPG": "yankee"}
	for name, c := range files {
		p := put(t, photos, "Archive/T/"+name, c, t0) // the promote's rename is done, its catalog write is not
		err := db.InsertAsset(ctx, catalog.Asset{SHA256: sum(c), Size: int64(len(c)), Kind: media.KindImage, State: catalog.StateStaged,
			RelPath: "Staging/T/" + name, Mtime: mtimeOf(t, p), UpdatedAt: t0}, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	locks.hold("Archive/T")
	r := rescan(t, s)
	wantCounts(t, r, ScanResult{BusyFolders: 1})
	noErrors(t, r)
	for name, c := range files {
		wantAsset(t, db, c, catalog.StateStaged, "Staging/T/"+name)
	}

	if _, err := db.ArchiveFolder(ctx, f.ID, "Staging/T/", "Archive/T/", t0); err != nil {
		t.Fatal(err)
	}
	locks.release("Archive/T")
	n, fp := countHashes(s), countFingerprints(s)
	r = rescan(t, s)
	wantCounts(t, r, ScanResult{Listed: 2, Unchanged: 2})
	noErrors(t, r)
	if n.Load() != 0 || fp.Load() != 0 {
		t.Errorf("reads: %d hashes, %d fingerprints; want none", n.Load(), fp.Load())
	}
	for name, c := range files {
		wantAsset(t, db, c, catalog.StateArchived, "Archive/T/"+name)
	}
}

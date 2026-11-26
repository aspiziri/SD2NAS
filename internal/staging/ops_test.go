package staging

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

// lockFree fails unless the locks of folders ids can be taken (it releases
// them again).
func (e *opsEnv) lockFree(t *testing.T, ids ...int64) {
	t.Helper()
	release, err := e.locks.TryLock(HolderRescan, ids...)
	if err != nil {
		t.Fatalf("a folder lock is still held: %v", err)
	}
	release()
}

// nameKeyFree fails unless the lock of directory name Staging/<name> can be taken.
func (e *opsEnv) nameKeyFree(t *testing.T, name string) {
	t.Helper()
	release, err := e.locks.TryLockSet(HolderRescan, LockSet{Names: []string{Dir + "/" + name}})
	if err != nil {
		t.Fatalf("the name lock of %q is still held: %v", name, err)
	}
	release()
}

// bumped fails unless the version went up since v.
func (e *opsEnv) bumped(t *testing.T, v uint64) {
	t.Helper()
	if e.version() <= v {
		t.Errorf("version = %d, did not go up from %d", e.version(), v)
	}
}

func (e *opsEnv) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(e.photos, filepath.FromSlash(rel)))
	return err == nil
}

func (e *opsEnv) mustExist(t *testing.T, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if !e.exists(rel) {
			t.Errorf("%s is missing; files: %v", rel, e.files(t))
		}
	}
}

func (e *opsEnv) mustNotExist(t *testing.T, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if e.exists(rel) {
			t.Errorf("%s exists but should not; files: %v", rel, e.files(t))
		}
	}
}

func (e *opsEnv) relPath(t *testing.T, sha string) string {
	t.Helper()
	return e.asset(t, sha).RelPath
}

func TestRenameKeepsDatePrefix(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30", "2025-05-30")
	s1 := e.stage(t, f, "a.JPG", "jpg", opsNow)
	s2 := e.stage(t, f, "Capture/a.ARW", "raw", opsNow)
	v := e.version()
	got, err := e.svc.Rename(t.Context(), f.ID, "  Trip ")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "2025-05-30 - Trip" || got.Files != 2 {
		t.Fatalf("folder = %q with %d files", got.Name, got.Files)
	}
	e.mustExist(t, "Staging/2025-05-30 - Trip/a.JPG", "Staging/2025-05-30 - Trip/Capture/a.ARW")
	e.mustNotExist(t, "Staging/2025-05-30")
	if p := e.relPath(t, s1); p != "Staging/2025-05-30 - Trip/a.JPG" {
		t.Errorf("a.JPG rel_path = %q", p)
	}
	if p := e.relPath(t, s2); p != "Staging/2025-05-30 - Trip/Capture/a.ARW" {
		t.Errorf("a.ARW rel_path = %q", p)
	}
	e.bumped(t, v)
	e.lockFree(t, f.ID)
	e.nameKeyFree(t, got.Name)
}

func TestRenameAsChangesTheDate(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	s1 := e.stage(t, f, "a.JPG", "jpg", opsNow)
	v := e.version()
	got, err := e.svc.RenameAs(t.Context(), f.ID, " 2025-06-02 - Hike ")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "2025-06-02 - Hike" {
		t.Fatalf("folder = %q", got.Name)
	}
	if p := e.relPath(t, s1); p != "Staging/2025-06-02 - Hike/a.JPG" {
		t.Errorf("rel_path = %q", p)
	}
	e.mustNotExist(t, "Staging/2025-05-30 - Trip")
	e.bumped(t, v)
	e.lockFree(t, f.ID)
	e.nameKeyFree(t, got.Name)

	if got, err := e.svc.RenameAs(t.Context(), f.ID, "2025-06-02 - "); err != nil || got.Name != "2025-06-02" {
		t.Errorf("dangling separator: %q, %v", got.Name, err)
	}
	if _, err := e.svc.RenameAs(t.Context(), f.ID, "  "); !errors.Is(err, ErrBadTitle) {
		t.Errorf("empty name: err = %v, want ErrBadTitle", err)
	}
	e.lockFree(t, f.ID)
}

func TestRenameEmptyTitleKeepsDate(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	v := e.version()
	got, err := e.svc.Rename(t.Context(), f.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "2025-05-30" {
		t.Fatalf("name = %q", got.Name)
	}
	e.mustExist(t, "Staging/2025-05-30")
	e.mustNotExist(t, "Staging/2025-05-30 - Trip")
	e.bumped(t, v)
	e.lockFree(t, f.ID)
	e.nameKeyFree(t, "2025-05-30")
}

func TestRenameNoDatePrefix(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "Holiday", "2025-06-01")
	v := e.version()
	got, err := e.svc.Rename(t.Context(), f.ID, "Beach")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "2025-06-01 - Beach" {
		t.Fatalf("name = %q", got.Name)
	}
	e.bumped(t, v)
	g := e.folder(t, "Holiday")
	v = e.version()
	got, err = e.svc.Rename(t.Context(), g.ID, "Beach")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Beach" {
		t.Fatalf("name = %q", got.Name)
	}
	e.bumped(t, v)
	h := e.folder(t, "Other")
	v = e.version()
	if _, err := e.svc.Rename(t.Context(), h.ID, ""); !errors.Is(err, ErrBadTitle) {
		t.Fatalf("undated, untitled: err = %v, want ErrBadTitle", err)
	}
	if e.version() != v {
		t.Error("a failed rename bumped the version")
	}
	e.mustExist(t, "Staging/Other")
	e.lockFree(t, f.ID, g.ID, h.ID)
	e.nameKeyFree(t, "2025-06-01 - Beach")
	e.nameKeyFree(t, "Beach")
}

func TestRenameNameTaken(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30", "2025-05-30")
	s := e.stage(t, f, "a.JPG", "jpg", opsNow)
	other := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	v := e.version()
	if _, err := e.svc.Rename(t.Context(), f.ID, "Trip"); !errors.Is(err, catalog.ErrNameTaken) {
		t.Fatalf("err = %v, want ErrNameTaken", err)
	}
	e.mustExist(t, "Staging/2025-05-30/a.JPG")
	if p := e.relPath(t, s); p != "Staging/2025-05-30/a.JPG" {
		t.Errorf("rel_path = %q", p)
	}
	if e.version() != v {
		t.Error("a failed rename bumped the version")
	}
	// Only an untracked directory by that name.
	if err := os.RemoveAll(filepath.Join(e.photos, Dir, other.Name)); err != nil {
		t.Fatal(err)
	}
	if err := e.db.DeleteStagingFolder(t.Context(), other.ID); err != nil {
		t.Fatal(err)
	}
	e.write(t, "Staging/2025-05-30 - Trip/x.txt", "x")
	if _, err := e.svc.Rename(t.Context(), f.ID, "Trip"); !errors.Is(err, catalog.ErrNameTaken) {
		t.Fatalf("untracked dir: err = %v, want ErrNameTaken", err)
	}
	e.mustExist(t, "Staging/2025-05-30/a.JPG", "Staging/2025-05-30 - Trip/x.txt")
	e.lockFree(t, f.ID)
}

func TestRenameBadTitle(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30", "2025-05-30")
	v := e.version()
	for _, title := range []string{"a/b", "x:y", ".hidden", strings.Repeat("x", 201)} {
		if _, err := e.svc.Rename(t.Context(), f.ID, title); !errors.Is(err, ErrBadTitle) {
			t.Errorf("title %q: err = %v, want ErrBadTitle", title, err)
		}
	}
	// A title that is fine alone but makes the full name too long.
	if _, err := e.svc.Rename(t.Context(), f.ID, strings.Repeat("x", 195)); !errors.Is(err, ErrBadTitle) {
		t.Errorf("long full name: err = %v, want ErrBadTitle", err)
	}
	e.mustExist(t, "Staging/2025-05-30")
	if e.version() != v {
		t.Error("a failed rename bumped the version")
	}
	e.lockFree(t, f.ID)
}

func TestRenameBusy(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30", "2025-05-30")
	release, err := e.locks.TryLock(HolderIngest, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := e.version()
	_, err = e.svc.Rename(t.Context(), f.ID, "Trip")
	var busy *BusyError
	if !errors.As(err, &busy) || busy.Holder != HolderIngest || busy.Name != "2025-05-30" {
		t.Fatalf("err = %v, want a BusyError for the import naming 2025-05-30", err)
	}
	e.mustExist(t, "Staging/2025-05-30")
	if e.version() != v {
		t.Error("a failed rename bumped the version")
	}
	e.nameKeyFree(t, "2025-05-30 - Trip")
	release()
	e.lockFree(t, f.ID)
}

func TestRenameMissingDir(t *testing.T) {
	e := newOpsEnv(t)
	f, err := e.db.CreateStagingFolder(t.Context(), "2025-05-30", []string{"2025-05-30"}, opsNow)
	if err != nil {
		t.Fatal(err)
	}
	v := e.version()
	got, err := e.svc.Rename(t.Context(), f.ID, "Trip")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "2025-05-30 - Trip" {
		t.Fatalf("name = %q", got.Name)
	}
	e.mustNotExist(t, "Staging/2025-05-30", "Staging/2025-05-30 - Trip")
	e.bumped(t, v)
	e.lockFree(t, f.ID)
	e.nameKeyFree(t, got.Name)
}

func TestRenameNonASCII(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30", "2025-05-30")
	s := e.stage(t, f, "a.JPG", "jpg", opsNow)
	v := e.version()
	if _, err := e.svc.Rename(t.Context(), f.ID, "Zürich"); err != nil {
		t.Fatal(err)
	}
	e.bumped(t, v)
	v = e.version()
	if p := e.relPath(t, s); p != "Staging/2025-05-30 - Zürich/a.JPG" {
		t.Fatalf("rel_path = %q", p)
	}
	e.mustExist(t, "Staging/2025-05-30 - Zürich/a.JPG")
	if _, err := e.svc.Rename(t.Context(), f.ID, "Zug"); err != nil {
		t.Fatal(err)
	}
	if p := e.relPath(t, s); p != "Staging/2025-05-30 - Zug/a.JPG" {
		t.Fatalf("rel_path = %q", p)
	}
	e.mustExist(t, "Staging/2025-05-30 - Zug/a.JPG")
	e.mustNotExist(t, "Staging/2025-05-30 - Zürich")
	e.bumped(t, v)
	e.lockFree(t, f.ID)
	e.nameKeyFree(t, "2025-05-30 - Zürich")
	e.nameKeyFree(t, "2025-05-30 - Zug")
}

func TestRenameUnknown(t *testing.T) {
	e := newOpsEnv(t)
	if _, err := e.svc.Rename(t.Context(), 999, "Trip"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRenameCaseOnly(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30 - trip", "2025-05-30")
	s := e.stage(t, f, "a.JPG", "jpg", opsNow)
	v := e.version()
	got, err := e.svc.Rename(t.Context(), f.ID, "Trip")
	if err != nil {
		t.Fatal(err)
	}
	e.bumped(t, v)
	if got.Name != "2025-05-30 - Trip" {
		t.Fatalf("name = %q", got.Name)
	}
	if p := e.relPath(t, s); p != "Staging/2025-05-30 - Trip/a.JPG" {
		t.Fatalf("rel_path = %q", p)
	}
	entries, err := os.ReadDir(filepath.Join(e.photos, Dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "2025-05-30 - Trip" {
		t.Fatalf("Staging/ holds %v, want only the renamed directory", entries)
	}
	e.lockFree(t, f.ID)
	e.nameKeyFree(t, got.Name)
}

func TestRenameTakesTheNameKey(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30", "2025-05-30")
	g := e.folder(t, "2025-05-31", "2025-05-31")
	hold, err := e.locks.TryLockSet(HolderRescan, LockSet{Names: []string{"Staging/2025-05-30 - Trip"}})
	if err != nil {
		t.Fatal(err)
	}
	v := e.version()
	_, err = e.svc.Rename(t.Context(), f.ID, "Trip")
	var busy *BusyError
	if !errors.As(err, &busy) || busy.ID != 0 || busy.Name != "2025-05-30 - Trip" || busy.Holder != HolderRescan {
		t.Fatalf("rename err = %#v, want a rescan BusyError for the name", err)
	}
	_, err = e.svc.Merge(t.Context(), []int64{f.ID, g.ID}, "Trip")
	if !errors.As(err, &busy) || busy.ID != 0 || busy.Name != "2025-05-30 - Trip" || busy.Holder != HolderRescan {
		t.Fatalf("merge err = %#v, want a rescan BusyError for the name", err)
	}
	e.mustExist(t, "Staging/2025-05-30", "Staging/2025-05-31")
	if e.version() != v {
		t.Error("a failed operation bumped the version")
	}
	e.lockFree(t, f.ID, g.ID)
	hold()
	if _, err := e.svc.Rename(t.Context(), f.ID, "Trip"); err != nil {
		t.Fatalf("after release: %v", err)
	}
	e.bumped(t, v)
	e.lockFree(t, f.ID, g.ID)
	e.nameKeyFree(t, "2025-05-30 - Trip")
}

func TestOperationsAfterClose(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30", "2025-05-30")
	g := e.folder(t, "2025-05-31", "2025-05-31")
	sha := e.stage(t, f, "a.JPG", "a", opsNow)
	e.svc.Close()
	v := e.version()
	if _, err := e.svc.Rename(t.Context(), f.ID, "Trip"); !errors.Is(err, ErrClosed) {
		t.Errorf("Rename: err = %v, want ErrClosed", err)
	}
	if _, err := e.svc.Merge(t.Context(), []int64{f.ID, g.ID}, "Trip"); !errors.Is(err, ErrClosed) {
		t.Errorf("Merge: err = %v, want ErrClosed", err)
	}
	if _, err := e.svc.Move(t.Context(), MoveRequest{SHAs: []string{sha}, ToID: g.ID}); !errors.Is(err, ErrClosed) {
		t.Errorf("Move: err = %v, want ErrClosed", err)
	}
	if _, err := e.svc.Cull(t.Context(), []string{sha}); !errors.Is(err, ErrClosed) {
		t.Errorf("Cull: err = %v, want ErrClosed", err)
	}
	if _, err := e.svc.Discard(t.Context(), f.ID); !errors.Is(err, ErrClosed) {
		t.Errorf("Discard: err = %v, want ErrClosed", err)
	}
	e.mustExist(t, "Staging/2025-05-30", "Staging/2025-05-31")
	if e.version() != v {
		t.Error("an operation after Close bumped the version")
	}
	e.lockFree(t, f.ID, g.ID)
}

// mergeFixture is folders A and B of the merge tests.
type mergeFixture struct {
	a, b                   catalog.StagingFolder
	aJPG, aARW, bMP4, bARW string
}

func newMergeFixture(t *testing.T, e *opsEnv) mergeFixture {
	t.Helper()
	m := mergeFixture{a: e.folder(t, "2025-05-30", "2025-05-30"), b: e.folder(t, "2025-05-31", "2025-05-31")}
	m.aJPG = e.stage(t, m.a, "a.JPG", "a-jpg", opsNow)
	m.aARW = e.stage(t, m.a, "Capture/a.ARW", "a-raw", opsNow)
	e.write(t, "Staging/2025-05-30/notes.txt", "notes")
	m.bMP4 = e.stage(t, m.b, "Videos/v.MP4", "b-video", opsNow)
	m.bARW = e.stage(t, m.b, "Capture/a.ARW", "b-raw-different", opsNow)
	e.write(t, "Staging/2025-05-31/.DS_Store", "junk")
	return m
}

func TestMergeMovesFilesAndRemovesSources(t *testing.T) {
	e := newOpsEnv(t)
	m := newMergeFixture(t, e)
	v := e.version()
	got, err := e.svc.Merge(t.Context(), []int64{m.b.ID, m.a.ID}, "Trip")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != m.a.ID || got.Name != "2025-05-30 - Trip" {
		t.Fatalf("result = %d %q", got.ID, got.Name)
	}
	if !slices.Equal(got.Dates, []string{"2025-05-30", "2025-05-31"}) {
		t.Errorf("dates = %v", got.Dates)
	}
	if got.Files != 4 {
		t.Errorf("files = %d, want 4", got.Files)
	}
	want := []string{
		"Staging/2025-05-30 - Trip/Capture/a-1.ARW",
		"Staging/2025-05-30 - Trip/Capture/a.ARW",
		"Staging/2025-05-30 - Trip/Videos/v.MP4",
		"Staging/2025-05-30 - Trip/a.JPG",
		"Staging/2025-05-30 - Trip/notes.txt",
	}
	if files := e.files(t); !slices.Equal(files, want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	e.mustNotExist(t, "Staging/2025-05-31", "Staging/2025-05-30")
	if _, err := e.db.StagingFolderByID(t.Context(), m.b.ID); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("B's row: err = %v, want ErrNotFound", err)
	}
	for sha, rel := range map[string]string{
		m.aJPG: "Staging/2025-05-30 - Trip/a.JPG", m.aARW: "Staging/2025-05-30 - Trip/Capture/a.ARW",
		m.bMP4: "Staging/2025-05-30 - Trip/Videos/v.MP4", m.bARW: "Staging/2025-05-30 - Trip/Capture/a-1.ARW",
	} {
		if p := e.relPath(t, sha); p != rel {
			t.Errorf("rel_path = %q, want %q", p, rel)
		}
	}
	e.bumped(t, v)
	e.lockFree(t, m.a.ID, m.b.ID)
	e.nameKeyFree(t, got.Name)
}

func TestMergePicksFolderWithFinalName(t *testing.T) {
	e := newOpsEnv(t)
	f1 := e.folder(t, "2025-05-30", "2025-05-30")
	f2 := e.folder(t, "2025-05-30 - Dinner", "2025-05-30")
	s1 := e.stage(t, f1, "a.JPG", "one", opsNow)
	s2 := e.stage(t, f2, "b.JPG", "two", opsNow)
	v := e.version()
	got, err := e.svc.Merge(t.Context(), []int64{f1.ID, f2.ID}, "Dinner")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != f2.ID || got.Name != "2025-05-30 - Dinner" {
		t.Fatalf("result = %d %q", got.ID, got.Name)
	}
	if p := e.relPath(t, s1); p != "Staging/2025-05-30 - Dinner/a.JPG" {
		t.Errorf("rel_path = %q", p)
	}
	if p := e.relPath(t, s2); p != "Staging/2025-05-30 - Dinner/b.JPG" {
		t.Errorf("rel_path = %q", p)
	}
	e.mustNotExist(t, "Staging/2025-05-30")
	e.bumped(t, v)
	e.lockFree(t, f1.ID, f2.ID)
	e.nameKeyFree(t, got.Name)
}

func TestMergeAsUsesTheWholeName(t *testing.T) {
	e := newOpsEnv(t)
	m := newMergeFixture(t, e)
	got, err := e.svc.MergeAs(t.Context(), []int64{m.a.ID, m.b.ID}, "2025-05-31 - Trip")
	if err != nil {
		t.Fatal(err)
	}
	// The earliest-dated folder is still the one kept, renamed to the later date.
	if got.ID != m.a.ID || got.Name != "2025-05-31 - Trip" || got.Files != 4 {
		t.Fatalf("result = %d %q %d files", got.ID, got.Name, got.Files)
	}
	if p := e.relPath(t, m.bMP4); p != "Staging/2025-05-31 - Trip/Videos/v.MP4" {
		t.Errorf("rel_path = %q", p)
	}
	e.mustNotExist(t, "Staging/2025-05-30", "Staging/2025-05-31")
	e.lockFree(t, m.a.ID, m.b.ID)
	e.nameKeyFree(t, got.Name)
}

func TestMergeAsKeepsFolderWithThatName(t *testing.T) {
	e := newOpsEnv(t)
	m := newMergeFixture(t, e)
	got, err := e.svc.MergeAs(t.Context(), []int64{m.a.ID, m.b.ID}, "2025-05-31 - ")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != m.b.ID || got.Name != "2025-05-31" {
		t.Fatalf("result = %d %q", got.ID, got.Name)
	}
	e.mustNotExist(t, "Staging/2025-05-30")
}

func TestMergeAsBadName(t *testing.T) {
	e := newOpsEnv(t)
	m := newMergeFixture(t, e)
	if _, err := e.svc.MergeAs(t.Context(), []int64{m.a.ID, m.b.ID}, "a/b"); !errors.Is(err, ErrBadTitle) {
		t.Fatalf("err = %v, want ErrBadTitle", err)
	}
	e.lockFree(t, m.a.ID, m.b.ID)
}

func TestMergeIdenticalFile(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	b := e.folder(t, "2025-05-31", "2025-05-31")
	e.write(t, "Staging/2025-05-30/notes.txt", "same")
	e.write(t, "Staging/2025-05-31/notes.txt", "same")
	v := e.version()
	if _, err := e.svc.Merge(t.Context(), []int64{a.ID, b.ID}, "Trip"); err != nil {
		t.Fatal(err)
	}
	if files := e.files(t); !slices.Equal(files, []string{"Staging/2025-05-30 - Trip/notes.txt"}) {
		t.Fatalf("files = %v", files)
	}
	e.mustNotExist(t, "Staging/2025-05-31")
	e.bumped(t, v)
	e.lockFree(t, a.ID, b.ID)
	e.nameKeyFree(t, "2025-05-30 - Trip")
}

func TestMergeCullsMissingTracked(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	b := e.folder(t, "2025-05-31", "2025-05-31")
	e.stage(t, a, "a.JPG", "kept", opsNow)
	gone := e.stage(t, b, "b.JPG", "gone", opsNow)
	if err := os.Remove(filepath.Join(e.photos, Dir, b.Name, "b.JPG")); err != nil {
		t.Fatal(err)
	}
	v := e.version()
	if _, err := e.svc.Merge(t.Context(), []int64{a.ID, b.ID}, "Trip"); err != nil {
		t.Fatal(err)
	}
	if st := e.asset(t, gone).State; st != catalog.StateCulled {
		t.Fatalf("state = %v, want culled", st)
	}
	if got := e.removedHashes(); !slices.Equal(got, []string{gone}) {
		t.Fatalf("removed = %v, want [%s]", got, gone)
	}
	e.mustNotExist(t, "Staging/2025-05-31")
	e.bumped(t, v)
	e.lockFree(t, a.ID, b.ID)
	e.nameKeyFree(t, "2025-05-30 - Trip")
}

func TestMergeOldestCreated(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	b := e.folder(t, "2025-05-31", "2025-05-31")
	older := opsNow.Add(-72 * time.Hour)
	if err := e.db.SetStagingFolderInfo(t.Context(), b.ID, b.Dates, older); err != nil {
		t.Fatal(err)
	}
	v := e.version()
	got, err := e.svc.Merge(t.Context(), []int64{a.ID, b.ID}, "Trip")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(older) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, older)
	}
	e.bumped(t, v)
	e.lockFree(t, a.ID, b.ID)
	e.nameKeyFree(t, got.Name)
}

func TestMergeNeedsTwo(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	v := e.version()
	defer func() {
		if e.version() != v {
			t.Error("a refused merge bumped the version")
		}
		e.lockFree(t, a.ID)
	}()
	for _, ids := range [][]int64{{a.ID}, {a.ID, a.ID}, nil} {
		if _, err := e.svc.Merge(t.Context(), ids, "Trip"); !errors.Is(err, ErrBadRequest) {
			t.Errorf("ids %v: err = %v, want ErrBadRequest", ids, err)
		}
	}
}

func TestMergeNameTaken(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	b := e.folder(t, "2025-05-31", "2025-05-31")
	e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	sa := e.stage(t, a, "a.JPG", "a", opsNow)
	e.stage(t, b, "b.JPG", "b", opsNow)
	v := e.version()
	if _, err := e.svc.Merge(t.Context(), []int64{a.ID, b.ID}, "Trip"); !errors.Is(err, catalog.ErrNameTaken) {
		t.Fatalf("err = %v, want ErrNameTaken", err)
	}
	e.mustExist(t, "Staging/2025-05-30/a.JPG", "Staging/2025-05-31/b.JPG")
	if p := e.relPath(t, sa); p != "Staging/2025-05-30/a.JPG" {
		t.Errorf("rel_path = %q", p)
	}
	if e.version() != v {
		t.Error("a failed merge bumped the version")
	}
	e.lockFree(t, a.ID, b.ID)
	e.nameKeyFree(t, "2025-05-30 - Trip")
}

func TestMergeBusy(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	b := e.folder(t, "2025-05-31", "2025-05-31")
	e.stage(t, a, "a.JPG", "a", opsNow)
	e.stage(t, b, "b.JPG", "b", opsNow)
	release, err := e.locks.TryLock(HolderIngest, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := e.version()
	_, err = e.svc.Merge(t.Context(), []int64{a.ID, b.ID}, "Trip")
	var busy *BusyError
	if !errors.As(err, &busy) || busy.ID != b.ID || busy.Name != "2025-05-31" || busy.Holder != HolderIngest {
		t.Fatalf("err = %#v, want an import BusyError naming B", err)
	}
	e.mustExist(t, "Staging/2025-05-30/a.JPG", "Staging/2025-05-31/b.JPG")
	if e.version() != v {
		t.Error("a failed merge bumped the version")
	}
	e.lockFree(t, a.ID)
	e.nameKeyFree(t, "2025-05-30 - Trip")
	release()
	e.lockFree(t, a.ID, b.ID)
}

func TestMergeKeepsSourceWithSpecialFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	b := e.folder(t, "2025-05-31", "2025-05-31")
	e.stage(t, a, "a.JPG", "a", opsNow)
	sb := e.stage(t, b, "b.JPG", "b", opsNow)
	link := filepath.Join(e.photos, Dir, b.Name, "link")
	if err := os.Symlink("b.JPG", link); err != nil {
		t.Fatal(err)
	}
	v := e.version()
	if _, err := e.svc.Merge(t.Context(), []int64{a.ID, b.ID}, "Trip"); err != nil {
		t.Fatal(err)
	}
	e.bumped(t, v)
	if p := e.relPath(t, sb); p != "Staging/2025-05-30 - Trip/b.JPG" {
		t.Errorf("rel_path = %q", p)
	}
	e.mustExist(t, "Staging/2025-05-30 - Trip/b.JPG", "Staging/2025-05-31/link")
	row, err := e.db.StagingFolderByID(t.Context(), b.ID)
	if err != nil {
		t.Fatalf("B's row should stay: %v", err)
	}
	if len(row.Dates) != 0 {
		t.Errorf("kept source dates = %v, want none", row.Dates)
	}
	e.lockFree(t, a.ID, b.ID)
	e.nameKeyFree(t, "2025-05-30 - Trip")
}

// moveEnv is folders A (2025-05-30: a.JPG, Capture/a.ARW, b.JPG) and B
// (2025-06-01 - Beach, empty) of the move, cull and discard tests.
type moveEnv struct {
	a, b             catalog.StagingFolder
	aJPG, aARW, bJPG string // hashes
	aJPGc            string // a.JPG's content
}

func newMoveEnv(t *testing.T, e *opsEnv) moveEnv {
	t.Helper()
	m := moveEnv{
		a:     e.folder(t, "2025-05-30", "2025-05-30"),
		b:     e.folder(t, "2025-06-01 - Beach", "2025-06-01"),
		aJPGc: "a-jpg",
	}
	m.aJPG = e.stage(t, m.a, "a.JPG", m.aJPGc, at(10))
	m.aARW = e.stage(t, m.a, "Capture/a.ARW", "a-raw", at(10))
	m.bJPG = e.stage(t, m.a, "b.JPG", "b-jpg", at(10))
	return m
}

func (e *opsEnv) folderRow(t *testing.T, id int64) catalog.StagingFolder {
	t.Helper()
	r, err := e.db.StagingFolderByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *opsEnv) noFolder(t *testing.T, id int64) {
	t.Helper()
	if _, err := e.db.StagingFolderByID(t.Context(), id); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("folder %d: err = %v, want ErrNotFound", id, err)
	}
}

func TestMoveToExistingFolder(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	v := e.version()
	res, err := e.svc.Move(t.Context(), MoveRequest{SHAs: []string{m.aJPG, m.aARW}, ToID: m.b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 2 || len(res.Removed) != 0 || res.Folder.ID != m.b.ID {
		t.Fatalf("result = %+v", res)
	}
	want := []string{
		"Staging/2025-05-30/b.JPG",
		"Staging/2025-06-01 - Beach/Capture/a.ARW",
		"Staging/2025-06-01 - Beach/a.JPG",
	}
	if files := e.files(t); !slices.Equal(files, want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	if p := e.relPath(t, m.aARW); p != "Staging/2025-06-01 - Beach/Capture/a.ARW" {
		t.Errorf("a.ARW rel_path = %q", p)
	}
	if d := e.folderRow(t, m.b.ID).Dates; !slices.Equal(d, []string{"2025-05-30", "2025-06-01"}) {
		t.Errorf("B dates = %v", d)
	}
	if d := e.folderRow(t, m.a.ID).Dates; !slices.Equal(d, []string{"2025-05-30"}) {
		t.Errorf("A dates = %v, want unchanged", d)
	}
	e.bumped(t, v)
	e.lockFree(t, m.a.ID, m.b.ID)
}

func TestMoveDatesUseDayCutoff(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-29", "2025-05-29")
	b := e.folder(t, "2025-06-01 - Beach", "2025-06-01")
	early := e.stage(t, a, "early.JPG", "early", time.Date(2025, 5, 31, 1, 30, 0, 0, time.Local))
	none := e.stage(t, a, "none.JPG", "none", time.Time{})
	v := e.version()
	if _, err := e.svc.Move(t.Context(), MoveRequest{SHAs: []string{early, none}, ToID: b.ID}); err != nil {
		t.Fatal(err)
	}
	if d := e.folderRow(t, b.ID).Dates; !slices.Equal(d, []string{"2025-05-29", "2025-05-30", "2025-06-01"}) {
		t.Errorf("B dates = %v, want the cutoff date 2025-05-30 and the source date 2025-05-29", d)
	}
	e.bumped(t, v)
	e.lockFree(t, a.ID, b.ID)
}

func TestMoveLastFilesRemovesSource(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	e.write(t, "Staging/2025-05-30/.DS_Store", "junk")
	if err := os.MkdirAll(filepath.Join(e.photos, Dir, m.a.Name, "Videos"), 0o777); err != nil {
		t.Fatal(err)
	}
	v := e.version()
	res, err := e.svc.Move(t.Context(), MoveRequest{SHAs: []string{m.aJPG, m.aARW, m.bJPG}, ToID: m.b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 3 || !slices.Equal(res.Removed, []int64{m.a.ID}) {
		t.Fatalf("result = %+v", res)
	}
	e.mustNotExist(t, "Staging/2025-05-30")
	e.noFolder(t, m.a.ID)
	if res.Folder.Files != 3 {
		t.Errorf("destination files = %d, want 3", res.Folder.Files)
	}
	e.bumped(t, v)
	e.lockFree(t, m.a.ID, m.b.ID)
}

func TestMoveToNewFolder(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	older := opsNow.Add(-48 * time.Hour)
	if err := e.db.SetStagingFolderInfo(t.Context(), m.a.ID, m.a.Dates, older); err != nil {
		t.Fatal(err)
	}
	hold, err := e.locks.TryLockSet(HolderRescan, LockSet{Names: []string{"Staging/2025-05-30 - Picks"}})
	if err != nil {
		t.Fatal(err)
	}
	folders, err := e.db.StagingFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	v := e.version()
	_, err = e.svc.Move(t.Context(), MoveRequest{SHAs: []string{m.bJPG}, NewTitle: " Picks "})
	var busy *BusyError
	if !errors.As(err, &busy) || busy.ID != 0 || busy.Name != "2025-05-30 - Picks" || busy.Holder != HolderRescan {
		t.Fatalf("err = %#v, want a rescan BusyError for the name", err)
	}
	e.mustNotExist(t, "Staging/2025-05-30 - Picks")
	e.mustExist(t, "Staging/2025-05-30/b.JPG")
	after, err := e.db.StagingFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(folders) {
		t.Errorf("folder rows = %d, want %d: a row was created", len(after), len(folders))
	}
	if e.version() != v {
		t.Error("a refused move bumped the version")
	}
	e.lockFree(t, m.a.ID, m.b.ID)
	hold()

	res, err := e.svc.Move(t.Context(), MoveRequest{SHAs: []string{m.bJPG}, NewTitle: " Picks "})
	if err != nil {
		t.Fatal(err)
	}
	if res.Folder.Name != "2025-05-30 - Picks" || res.Moved != 1 || res.Folder.Files != 1 || len(res.Removed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if !slices.Equal(res.Folder.Dates, []string{"2025-05-30"}) {
		t.Errorf("dates = %v", res.Folder.Dates)
	}
	if !res.Folder.CreatedAt.Equal(older) {
		t.Errorf("created_at = %v, want A's %v", res.Folder.CreatedAt, older)
	}
	if p := e.relPath(t, m.bJPG); p != "Staging/2025-05-30 - Picks/b.JPG" {
		t.Errorf("rel_path = %q", p)
	}
	e.mustExist(t, "Staging/2025-05-30 - Picks/b.JPG", "Staging/2025-05-30/a.JPG")
	e.bumped(t, v)
	e.lockFree(t, m.a.ID, m.b.ID, res.Folder.ID)
	e.nameKeyFree(t, "2025-05-30 - Picks")
}

func TestMoveToNewFolderNamed(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	v := e.version()
	res, err := e.svc.Move(t.Context(), MoveRequest{SHAs: []string{m.bJPG}, NewName: " 2025-06-02 - Picks "})
	if err != nil {
		t.Fatal(err)
	}
	if res.Folder.Name != "2025-06-02 - Picks" || res.Moved != 1 {
		t.Fatalf("result = %+v", res)
	}
	if p := e.relPath(t, m.bJPG); p != "Staging/2025-06-02 - Picks/b.JPG" {
		t.Errorf("rel_path = %q", p)
	}
	e.bumped(t, v)
	e.lockFree(t, m.a.ID, m.b.ID, res.Folder.ID)
	e.nameKeyFree(t, res.Folder.Name)
}

func TestMoveNewFolderRules(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	taken := e.folder(t, "2025-05-30 - Picks", "2025-05-30")
	sha := []string{m.bJPG}
	v := e.version()
	cases := []struct {
		name string
		req  MoveRequest
		want error
	}{
		{"blank title", MoveRequest{SHAs: sha, NewTitle: "   "}, ErrBadTitle},
		{"bad character", MoveRequest{SHAs: sha, NewTitle: "a/b"}, ErrBadTitle},
		{"too long once dated", MoveRequest{SHAs: sha, NewTitle: strings.Repeat("x", 195)}, ErrBadTitle},
		{"both", MoveRequest{SHAs: sha, ToID: m.b.ID, NewTitle: "Picks"}, ErrBadRequest},
		{"title and name", MoveRequest{SHAs: sha, NewTitle: "Picks", NewName: "2025-06-01 - Picks"}, ErrBadRequest},
		{"bad name", MoveRequest{SHAs: sha, NewName: "2025-06-01 - a/b"}, ErrBadTitle},
		{"blank name", MoveRequest{SHAs: sha, NewName: "  "}, ErrBadTitle},
		{"whole name exists", MoveRequest{SHAs: sha, NewName: "2025-05-30 - Picks"}, catalog.ErrNameTaken},
		{"neither", MoveRequest{SHAs: sha}, ErrBadRequest},
		{"name exists", MoveRequest{SHAs: sha, NewTitle: "Picks"}, catalog.ErrNameTaken},
		{"unknown folder", MoveRequest{SHAs: sha, ToID: 999}, catalog.ErrNotFound},
	}
	for _, c := range cases {
		if _, err := e.svc.Move(t.Context(), c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	e.mustExist(t, "Staging/2025-05-30/b.JPG")
	if p := e.relPath(t, m.bJPG); p != "Staging/2025-05-30/b.JPG" {
		t.Errorf("rel_path = %q", p)
	}
	if e.version() != v {
		t.Error("a refused move bumped the version")
	}
	e.lockFree(t, m.a.ID, m.b.ID, taken.ID)
	e.nameKeyFree(t, "2025-05-30 - Picks")
}

func TestMoveCollision(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	e.write(t, "Staging/2025-06-01 - Beach/a.JPG", "a different a.JPG")
	v := e.version()
	res, err := e.svc.Move(t.Context(), MoveRequest{SHAs: []string{m.aJPG}, ToID: m.b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 1 {
		t.Fatalf("moved = %d", res.Moved)
	}
	if p := e.relPath(t, m.aJPG); p != "Staging/2025-06-01 - Beach/a-1.JPG" {
		t.Errorf("rel_path = %q, want a-1.JPG", p)
	}
	e.mustExist(t, "Staging/2025-06-01 - Beach/a.JPG", "Staging/2025-06-01 - Beach/a-1.JPG")
	e.bumped(t, v)
	e.lockFree(t, m.a.ID, m.b.ID)
}

func TestMoveToSameFolderIsNoop(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	before := e.files(t)
	res, err := e.svc.Move(t.Context(), MoveRequest{SHAs: []string{m.aJPG, m.bJPG}, ToID: m.a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 0 || len(res.Removed) != 0 || res.Folder.ID != m.a.ID {
		t.Fatalf("result = %+v", res)
	}
	if files := e.files(t); !slices.Equal(files, before) {
		t.Errorf("files = %v, want %v", files, before)
	}
	if d := e.folderRow(t, m.a.ID).Dates; !slices.Equal(d, []string{"2025-05-30"}) {
		t.Errorf("dates = %v", d)
	}
	e.lockFree(t, m.a.ID, m.b.ID)
}

func TestMoveRejectsBadHashes(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	archived := shaOf("archived")
	if err := e.db.InsertAsset(t.Context(), catalog.Asset{SHA256: archived, Size: 8, Kind: media.KindImage,
		State: catalog.StateArchived, RelPath: "Archive/2025/x.JPG", UpdatedAt: opsNow}, shaOf("fp:archived")); err != nil {
		t.Fatal(err)
	}
	culled := e.stage(t, m.a, "culled.JPG", "culled", at(10))
	if err := e.db.CullAsset(t.Context(), culled, opsNow); err != nil {
		t.Fatal(err)
	}
	tooMany := make([]string, MaxFiles+1)
	for i := range tooMany {
		tooMany[i] = m.aJPG
	}
	v := e.version()
	for name, shas := range map[string][]string{
		"unknown":  {shaOf("nope")},
		"archived": {archived},
		"culled":   {culled},
		"not hex":  {"xyz"},
		"upper":    {strings.ToUpper(m.aJPG)},
		"empty":    {},
		"too many": tooMany,
		"one bad":  {m.aJPG, "xyz"},
	} {
		if _, err := e.svc.Move(t.Context(), MoveRequest{SHAs: shas, ToID: m.b.ID}); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: err = %v, want ErrBadRequest", name, err)
		}
		if _, err := e.svc.Cull(t.Context(), shas); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: cull err = %v, want ErrBadRequest", name, err)
		}
	}
	e.mustExist(t, "Staging/2025-05-30/a.JPG")
	if e.version() != v {
		t.Error("a refused request bumped the version")
	}
	e.lockFree(t, m.a.ID, m.b.ID)
	res, err := e.svc.Move(t.Context(), MoveRequest{SHAs: []string{m.aJPG, m.aJPG}, ToID: m.b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 1 {
		t.Errorf("a duplicated hash moved %d files, want 1", res.Moved)
	}
	e.bumped(t, v)
	e.lockFree(t, m.a.ID, m.b.ID)
}

func TestMoveBusyDestination(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	release, err := e.locks.TryLock(HolderIngest, m.b.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := e.version()
	_, err = e.svc.Move(t.Context(), MoveRequest{SHAs: []string{m.aJPG}, ToID: m.b.ID})
	var busy *BusyError
	if !errors.As(err, &busy) || busy.Holder != HolderIngest || busy.Name != m.b.Name || busy.ID != m.b.ID {
		t.Fatalf("err = %#v, want an import BusyError naming B", err)
	}
	e.mustExist(t, "Staging/2025-05-30/a.JPG")
	if p := e.relPath(t, m.aJPG); p != "Staging/2025-05-30/a.JPG" {
		t.Errorf("rel_path = %q", p)
	}
	if e.version() != v {
		t.Error("a refused move bumped the version")
	}
	e.lockFree(t, m.a.ID)
	release()
	e.lockFree(t, m.a.ID, m.b.ID)
}

func TestMoveCrossDevice(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	e.svc.rename = xdev
	v := e.version()
	res, err := e.svc.Move(t.Context(), MoveRequest{SHAs: []string{m.aJPG, m.aARW}, ToID: m.b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 2 {
		t.Fatalf("moved = %d", res.Moved)
	}
	want := []string{
		"Staging/2025-05-30/b.JPG",
		"Staging/2025-06-01 - Beach/Capture/a.ARW",
		"Staging/2025-06-01 - Beach/a.JPG",
	}
	if files := e.files(t); !slices.Equal(files, want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	data, err := os.ReadFile(filepath.Join(e.photos, Dir, m.b.Name, "a.JPG"))
	if err != nil || string(data) != m.aJPGc {
		t.Errorf("copied content = %q, %v", data, err)
	}
	if p := e.relPath(t, m.aJPG); p != "Staging/2025-06-01 - Beach/a.JPG" {
		t.Errorf("rel_path = %q", p)
	}
	e.bumped(t, v)
	e.lockFree(t, m.a.ID, m.b.ID)
}

func TestCullDeletesAndRemembers(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	v := e.version()
	res, err := e.svc.Cull(t.Context(), []string{m.aJPG})
	if err != nil {
		t.Fatal(err)
	}
	if res.Culled != 1 || len(res.Removed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	e.mustNotExist(t, "Staging/2025-05-30/a.JPG")
	e.mustExist(t, "Staging/2025-05-30/b.JPG", "Staging/2025-05-30/Capture/a.ARW")
	a := e.asset(t, m.aJPG)
	if a.State != catalog.StateCulled || a.RelPath != "" {
		t.Errorf("asset = %v %q, want culled with no path", a.State, a.RelPath)
	}
	found, err := e.db.AssetsByFingerprint(t.Context(), shaOf("fp:"+m.aJPGc))
	if err != nil || len(found) != 1 || found[0].SHA256 != m.aJPG {
		t.Errorf("fingerprint lookup = %v, %v", found, err)
	}
	if got := e.removedHashes(); !slices.Equal(got, []string{m.aJPG}) {
		t.Errorf("removed = %v", got)
	}
	e.bumped(t, v)
	e.lockFree(t, m.a.ID, m.b.ID)
}

func TestCullLastFileRemovesFolder(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	e.write(t, "Staging/2025-05-30/Thumbs.db", "junk")
	if err := os.MkdirAll(filepath.Join(e.photos, Dir, m.a.Name, "Videos"), 0o777); err != nil {
		t.Fatal(err)
	}
	v := e.version()
	res, err := e.svc.Cull(t.Context(), []string{m.aJPG, m.aARW, m.bJPG})
	if err != nil {
		t.Fatal(err)
	}
	if res.Culled != 3 || !slices.Equal(res.Removed, []int64{m.a.ID}) {
		t.Fatalf("result = %+v", res)
	}
	e.mustNotExist(t, "Staging/2025-05-30")
	e.noFolder(t, m.a.ID)
	e.mustExist(t, "Staging/2025-06-01 - Beach")
	e.bumped(t, v)
	e.lockFree(t, m.a.ID, m.b.ID)
}

func TestCullMissingFile(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	if err := os.Remove(filepath.Join(e.photos, Dir, m.a.Name, "a.JPG")); err != nil {
		t.Fatal(err)
	}
	v := e.version()
	res, err := e.svc.Cull(t.Context(), []string{m.aJPG})
	if err != nil {
		t.Fatal(err)
	}
	if res.Culled != 1 {
		t.Errorf("culled = %d", res.Culled)
	}
	if st := e.asset(t, m.aJPG).State; st != catalog.StateCulled {
		t.Errorf("state = %v, want culled", st)
	}
	e.bumped(t, v)
	e.lockFree(t, m.a.ID, m.b.ID)
}

func TestCullAcrossFolders(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	bFile := e.stage(t, m.b, "c.JPG", "c-jpg", at(10))
	release, err := e.locks.TryLock(HolderIngest, m.b.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := e.version()
	_, err = e.svc.Cull(t.Context(), []string{m.aJPG, bFile})
	var busy *BusyError
	if !errors.As(err, &busy) || busy.ID != m.b.ID || busy.Holder != HolderIngest {
		t.Fatalf("err = %#v, want an import BusyError naming B", err)
	}
	if st := e.asset(t, m.aJPG).State; st != catalog.StateStaged {
		t.Errorf("A's file state = %v, want staged", st)
	}
	e.mustExist(t, "Staging/2025-05-30/a.JPG", "Staging/2025-06-01 - Beach/c.JPG")
	if e.version() != v {
		t.Error("a refused cull bumped the version")
	}
	e.lockFree(t, m.a.ID)
	release()
	res, err := e.svc.Cull(t.Context(), []string{m.aJPG, bFile})
	if err != nil {
		t.Fatal(err)
	}
	if res.Culled != 2 || !slices.Equal(res.Removed, []int64{m.b.ID}) {
		t.Fatalf("result = %+v", res)
	}
	e.mustNotExist(t, "Staging/2025-05-30/a.JPG", "Staging/2025-06-01 - Beach")
	e.bumped(t, v)
	e.lockFree(t, m.a.ID, m.b.ID)
}

func TestDiscardRemovesFolder(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	e.write(t, "Staging/2025-05-30/.DS_Store", "junk")
	if err := os.MkdirAll(filepath.Join(e.photos, Dir, m.a.Name, "Videos", "Empty"), 0o777); err != nil {
		t.Fatal(err)
	}
	v := e.version()
	res, err := e.svc.Discard(t.Context(), m.a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Culled != 3 || !res.Removed || len(res.Kept) != 0 || res.Kept == nil {
		t.Fatalf("result = %+v", res)
	}
	e.mustNotExist(t, "Staging/2025-05-30")
	e.noFolder(t, m.a.ID)
	want := []string{m.aJPG, m.aARW, m.bJPG}
	slices.Sort(want)
	if got := e.removedHashes(); !slices.Equal(got, want) {
		t.Errorf("removed = %v, want %v", got, want)
	}
	if st := e.asset(t, m.aARW).State; st != catalog.StateCulled {
		t.Errorf("state = %v, want culled", st)
	}
	e.mustExist(t, "Staging/2025-06-01 - Beach")
	e.bumped(t, v)
	e.lockFree(t, m.a.ID, m.b.ID)
}

func TestDiscardKeepsUntrackedFiles(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	e.write(t, "Staging/2025-05-30/notes.txt", "notes")
	e.write(t, "Staging/2025-05-30/c.JPG", "untracked jpg")
	v := e.version()
	res, err := e.svc.Discard(t.Context(), m.a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Culled != 3 || res.Removed || !slices.Equal(res.Kept, []string{"c.JPG", "notes.txt"}) {
		t.Fatalf("result = %+v", res)
	}
	e.mustExist(t, "Staging/2025-05-30/c.JPG", "Staging/2025-05-30/notes.txt")
	e.mustNotExist(t, "Staging/2025-05-30/a.JPG", "Staging/2025-05-30/Capture")
	got, err := e.svc.Files(t.Context(), m.a.ID, "")
	if err != nil || got.Folder.Files != 0 {
		t.Errorf("folder files = %d, %v; want the row to stay with 0", got.Folder.Files, err)
	}
	e.bumped(t, v)
	e.lockFree(t, m.a.ID)
}

func TestDiscardBusy(t *testing.T) {
	e := newOpsEnv(t)
	m := newMoveEnv(t, e)
	release, err := e.locks.TryLock(HolderRescan, m.a.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := e.version()
	_, err = e.svc.Discard(t.Context(), m.a.ID)
	var busy *BusyError
	if !errors.As(err, &busy) || busy.Holder != HolderRescan || busy.ID != m.a.ID || busy.Name != m.a.Name {
		t.Fatalf("err = %#v, want a rescan BusyError naming A", err)
	}
	e.mustExist(t, "Staging/2025-05-30/a.JPG", "Staging/2025-05-30/b.JPG", "Staging/2025-05-30/Capture/a.ARW")
	if st := e.asset(t, m.aJPG).State; st != catalog.StateStaged {
		t.Errorf("state = %v, want staged", st)
	}
	if e.version() != v {
		t.Error("a refused discard bumped the version")
	}
	release()
	e.lockFree(t, m.a.ID)
	if _, err := e.svc.Discard(t.Context(), 999); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("unknown folder: err = %v, want ErrNotFound", err)
	}
	if e.version() != v {
		t.Error("a refused discard bumped the version")
	}
}

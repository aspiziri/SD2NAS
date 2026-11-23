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
	e.svc.Close()
	v := e.version()
	if _, err := e.svc.Rename(t.Context(), f.ID, "Trip"); !errors.Is(err, ErrClosed) {
		t.Errorf("Rename: err = %v, want ErrClosed", err)
	}
	if _, err := e.svc.Merge(t.Context(), []int64{f.ID, g.ID}, "Trip"); !errors.Is(err, ErrClosed) {
		t.Errorf("Merge: err = %v, want ErrClosed", err)
	}
	// Task 6 adds Move, Cull and Discard here.
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

package staging

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
	"github.com/aspiziri/SD2NAS/internal/media/mediatest"
)

// jpeg returns a small JPEG whose EXIF DateTimeOriginal is date
// ("2006:01:02 15:04:05") as a string.
func jpeg(t *testing.T, date string) string {
	t.Helper()
	b, err := mediatest.JPEG(mediatest.ExifSpec{DateTimeOriginal: date})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (e *opsEnv) reconcile(t *testing.T) RescanResult {
	t.Helper()
	r, err := e.svc.Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *opsEnv) folderRows(t *testing.T) []catalog.StagingFolder {
	t.Helper()
	rows, err := e.db.StagingFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestReconcileAdoptsUntrackedFolder(t *testing.T) {
	e := newOpsEnv(t)
	const dir = "Staging/2025-06-02 - Beach"
	p := e.write(t, dir+"/IMG_1.JPG", jpeg(t, "2025:06:02 10:00:00"))
	e.write(t, dir+"/notes.txt", "my notes")
	e.write(t, dir+"/.DS_Store", "junk")
	v := e.version()
	r := e.reconcile(t)
	if r.AdoptedFolders != 1 || r.AddedFiles != 1 || len(r.Errors) != 0 {
		t.Fatalf("result = %+v", r)
	}
	rows := e.folderRows(t)
	if len(rows) != 1 || rows[0].Name != "2025-06-02 - Beach" || !slices.Equal(rows[0].Dates, []string{"2025-06-02"}) ||
		!rows[0].CreatedAt.Equal(opsNow) {
		t.Fatalf("rows = %+v", rows)
	}
	fp, err := media.QuickFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	found, err := e.db.AssetsByFingerprint(t.Context(), fp)
	if err != nil || len(found) != 1 {
		t.Fatalf("fingerprint lookup = %v, %v", found, err)
	}
	a := found[0]
	if a.State != catalog.StateStaged || a.RelPath != dir+"/IMG_1.JPG" || a.FirstIngest != 0 ||
		!a.CaptureTime.Equal(time.Date(2025, 6, 2, 10, 0, 0, 0, time.Local)) {
		t.Errorf("asset = %+v", a)
	}
	e.mustExist(t, dir+"/notes.txt", dir+"/.DS_Store")
	if all, err := e.db.AssetsUnder(t.Context(), "Staging/", catalog.StateStaged); err != nil || len(all) != 1 {
		t.Errorf("staged assets = %v, %v", all, err)
	}
	e.bumped(t, v)
	e.lockFree(t, rows[0].ID)
	e.nameKeyFree(t, rows[0].Name)
}

func TestReconcileAddsFileToTrackedFolder(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-06-02 - Beach", "2025-06-02")
	e.stage(t, f, "a.JPG", "tracked", opsNow)
	content := jpeg(t, "2025:06:03 09:00:00")
	e.write(t, "Staging/2025-06-02 - Beach/Sub/b.JPG", content)
	v := e.version()
	r := e.reconcile(t)
	if r.AddedFiles != 1 || r.AdoptedFolders != 0 {
		t.Fatalf("result = %+v", r)
	}
	a := e.asset(t, shaOf(content))
	if a.State != catalog.StateStaged || a.RelPath != "Staging/2025-06-02 - Beach/Sub/b.JPG" {
		t.Errorf("asset = %+v", a)
	}
	rows := e.folderRows(t)
	if len(rows) != 1 || !slices.Equal(rows[0].Dates, []string{"2025-06-02", "2025-06-03"}) {
		t.Errorf("rows = %+v", rows)
	}
	e.bumped(t, v)
	e.lockFree(t, f.ID)
}

func TestReconcileCullsRemovedFiles(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-06-02 - Beach", "2025-06-02")
	gone := e.stage(t, f, "a.JPG", "gone", opsNow)
	e.stage(t, f, "b.JPG", "kept", opsNow)
	if err := os.Remove(filepath.Join(e.photos, Dir, f.Name, "a.JPG")); err != nil {
		t.Fatal(err)
	}
	v := e.version()
	r := e.reconcile(t)
	if r.CulledFiles != 1 || r.RemovedFolders != 0 {
		t.Fatalf("result = %+v", r)
	}
	if a := e.asset(t, gone); a.State != catalog.StateCulled || a.RelPath != "" {
		t.Errorf("asset = %+v", a)
	}
	found, err := e.db.AssetsByFingerprint(t.Context(), shaOf("fp:gone"))
	if err != nil || len(found) != 1 || found[0].SHA256 != gone {
		t.Errorf("fingerprint lookup = %v, %v", found, err)
	}
	if got := e.removedHashes(); !slices.Equal(got, []string{gone}) {
		t.Errorf("removed = %v", got)
	}
	e.bumped(t, v)
	e.lockFree(t, f.ID)
}

func TestReconcileDropsFolderWithoutDir(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-06-02 - Beach", "2025-06-02")
	s1 := e.stage(t, f, "a.JPG", "one", opsNow)
	s2 := e.stage(t, f, "b.JPG", "two", opsNow)
	if err := os.RemoveAll(filepath.Join(e.photos, Dir, f.Name)); err != nil {
		t.Fatal(err)
	}
	v := e.version()
	r := e.reconcile(t)
	if r.CulledFiles != 2 || r.RemovedFolders != 1 {
		t.Fatalf("result = %+v", r)
	}
	if rows := e.folderRows(t); len(rows) != 0 {
		t.Errorf("rows = %+v", rows)
	}
	for _, sha := range []string{s1, s2} {
		if a := e.asset(t, sha); a.State != catalog.StateCulled {
			t.Errorf("asset %s = %v", sha[:8], a.State)
		}
	}
	e.bumped(t, v)
	e.lockFree(t, f.ID)
}

func TestReconcileFollowsMoveBetweenFolders(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-06-02 - A", "2025-06-02")
	b := e.folder(t, "2025-06-03 - B", "2025-06-03")
	sha := e.stage(t, a, "x.JPG", "moved by hand", opsNow)
	e.stage(t, b, "y.JPG", "stays", opsNow)
	if err := os.Rename(filepath.Join(e.photos, Dir, a.Name, "x.JPG"), filepath.Join(e.photos, Dir, b.Name, "x.JPG")); err != nil {
		t.Fatal(err)
	}
	v := e.version()
	r := e.reconcile(t)
	if r.MovedFiles != 1 || r.CulledFiles != 0 || r.AddedFiles != 0 {
		t.Fatalf("result = %+v", r)
	}
	if p := e.relPath(t, sha); p != "Staging/2025-06-03 - B/x.JPG" {
		t.Errorf("rel_path = %q", p)
	}
	if all, err := e.db.AssetsUnder(t.Context(), "Staging/", catalog.StateStaged); err != nil || len(all) != 2 {
		t.Errorf("staged assets = %v, %v", all, err)
	}
	e.bumped(t, v)
	e.lockFree(t, a.ID, b.ID)
}

func TestReconcileRestoresCulledFile(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-06-02 - Beach", "2025-06-02")
	sha := e.stage(t, f, "a.JPG", "culled then back", opsNow)
	if _, err := e.svc.Cull(t.Context(), []string{sha}); err != nil {
		t.Fatal(err)
	}
	if a := e.asset(t, sha); a.State != catalog.StateCulled {
		t.Fatalf("state = %v", a.State)
	}
	e.write(t, "Staging/2025-06-02 - Beach/Back/again.JPG", "culled then back")
	v := e.version()
	r := e.reconcile(t)
	if r.RestoredFiles != 1 || r.AddedFiles != 0 {
		t.Fatalf("result = %+v", r)
	}
	if a := e.asset(t, sha); a.State != catalog.StateStaged || a.RelPath != "Staging/2025-06-02 - Beach/Back/again.JPG" {
		t.Errorf("asset = %+v", a)
	}
	e.bumped(t, v)
	e.lockFree(t, f.ID)
}

func TestReconcileLeavesDuplicates(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-06-02 - Beach", "2025-06-02")
	sha := e.stage(t, f, "a.JPG", "original", opsNow)
	arch := catalog.Asset{SHA256: shaOf("archived"), Size: 8, Kind: media.KindImage, State: catalog.StateArchived,
		RelPath: "Archive/2025/06/arch.JPG", UpdatedAt: opsNow}
	if err := e.db.InsertAsset(t.Context(), arch, shaOf("fp:archived")); err != nil {
		t.Fatal(err)
	}
	e.write(t, "Staging/2025-06-02 - Beach/a copy.JPG", "original")
	e.write(t, "Staging/2025-06-02 - Beach/arch copy.JPG", "archived")
	v := e.version()
	r := e.reconcile(t)
	if r.Duplicates != 2 || r.AddedFiles != 0 || r.MovedFiles != 0 || r.RestoredFiles != 0 {
		t.Fatalf("result = %+v", r)
	}
	if p := e.relPath(t, sha); p != "Staging/2025-06-02 - Beach/a.JPG" {
		t.Errorf("tracked rel_path = %q", p)
	}
	if a := e.asset(t, arch.SHA256); a.State != catalog.StateArchived || a.RelPath != arch.RelPath {
		t.Errorf("archived asset = %+v", a)
	}
	e.mustExist(t, "Staging/2025-06-02 - Beach/a copy.JPG", "Staging/2025-06-02 - Beach/arch copy.JPG")
	if e.version() != v {
		t.Error("leaving duplicates bumped the version")
	}
}

func TestReconcileSkipsBusyFolder(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-06-02 - Beach", "2025-06-02")
	sha := e.stage(t, f, "a.JPG", "tracked", opsNow)
	added := jpeg(t, "2025:06:02 11:00:00")
	e.write(t, "Staging/2025-06-02 - Beach/new.JPG", added)
	if err := os.Remove(filepath.Join(e.photos, Dir, f.Name, "a.JPG")); err != nil {
		t.Fatal(err)
	}
	release, err := e.locks.TryLock(HolderIngest, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := e.version()
	r := e.reconcile(t)
	if r.BusyFolders != 1 || r.AddedFiles != 0 || r.CulledFiles != 0 || r.RemovedFolders != 0 {
		t.Fatalf("result = %+v", r)
	}
	if a := e.asset(t, sha); a.State != catalog.StateStaged {
		t.Errorf("tracked asset = %v, want staged", a.State)
	}
	if _, err := e.db.AssetBySHA(t.Context(), shaOf(added)); err == nil {
		t.Error("the new file was recorded in a busy folder")
	}
	if e.version() != v {
		t.Error("a pass that changed nothing bumped the version")
	}
	release()
	r = e.reconcile(t)
	if r.BusyFolders != 0 || r.AddedFiles != 1 || r.CulledFiles != 1 {
		t.Errorf("next pass = %+v", r)
	}
	e.lockFree(t, f.ID)
}

func TestReconcileSkipsMoveFromBusyFolder(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-06-02 - A", "2025-06-02")
	b := e.folder(t, "2025-06-03 - B", "2025-06-03")
	sha := e.stage(t, a, "x.JPG", "moved by hand", opsNow)
	if err := os.Rename(filepath.Join(e.photos, Dir, a.Name, "x.JPG"), filepath.Join(e.photos, Dir, b.Name, "x.JPG")); err != nil {
		t.Fatal(err)
	}
	release, err := e.locks.TryLock(HolderIngest, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := e.version()
	r := e.reconcile(t)
	if r.BusyFolders != 1 || r.MovedFiles != 0 || r.AddedFiles != 0 || r.CulledFiles != 0 {
		t.Fatalf("result = %+v", r)
	}
	if p := e.relPath(t, sha); p != "Staging/2025-06-02 - A/x.JPG" {
		t.Errorf("rel_path = %q, want it left at A's path this time", p)
	}
	e.mustExist(t, "Staging/2025-06-03 - B/x.JPG")
	if e.version() != v {
		t.Error("a pass that changed nothing bumped the version")
	}
	release()
	r = e.reconcile(t)
	if r.MovedFiles != 1 || r.BusyFolders != 0 {
		t.Errorf("next pass = %+v", r)
	}
	if p := e.relPath(t, sha); p != "Staging/2025-06-03 - B/x.JPG" {
		t.Errorf("rel_path = %q", p)
	}
	e.lockFree(t, a.ID, b.ID)
}

func TestReconcileStagingMissing(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-06-02 - Beach", "2025-06-02")
	sha := e.stage(t, f, "a.JPG", "tracked", opsNow)
	if err := os.RemoveAll(filepath.Join(e.photos, Dir)); err != nil {
		t.Fatal(err)
	}
	v := e.version()
	r := e.reconcile(t)
	if r.Skipped == "" {
		t.Error("Skipped is empty")
	}
	if r.CulledFiles != 0 || r.RemovedFolders != 0 {
		t.Errorf("result = %+v", r)
	}
	if a := e.asset(t, sha); a.State != catalog.StateStaged {
		t.Errorf("asset = %v, want staged", a.State)
	}
	if rows := e.folderRows(t); len(rows) != 1 {
		t.Errorf("rows = %+v", rows)
	}
	if e.version() != v {
		t.Error("version changed")
	}
}

func TestReconcileIgnoresPartialsAndOtherFiles(t *testing.T) {
	e := newOpsEnv(t)
	e.folder(t, "2025-06-02 - Beach", "2025-06-02")
	e.write(t, "Staging/2025-06-02 - Beach/x.JPG.partial", jpeg(t, "2025:06:02 10:00:00"))
	e.write(t, "Staging/2025-06-02 - Beach/readme.txt", "hello")
	e.write(t, "Staging/loose.JPG", jpeg(t, "2025:06:02 10:00:00"))
	v := e.version()
	r := e.reconcile(t)
	if r.AddedFiles != 0 || r.AdoptedFolders != 0 || r.MovedFiles != 0 {
		t.Fatalf("result = %+v", r)
	}
	if all, err := e.db.AssetsUnder(t.Context(), "Staging/", catalog.StateStaged); err != nil || len(all) != 0 {
		t.Errorf("staged assets = %v, %v", all, err)
	}
	if e.version() != v {
		t.Error("version changed")
	}
}

func TestReconcileSkipsFileBeingWritten(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-06-02 - Beach", "2025-06-02")
	content := jpeg(t, "2025:06:02 10:00:00")
	e.write(t, "Staging/2025-06-02 - Beach/copying.JPG", content)
	e.svc.afterHash = func(p string) {
		w, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := w.Write([]byte{0}); err != nil {
			t.Error(err)
		}
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	}
	r := e.reconcile(t)
	if r.AddedFiles != 0 || len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "changed while it was read") {
		t.Fatalf("result = %+v", r)
	}
	if all, err := e.db.AssetsUnder(t.Context(), "Staging/", catalog.StateStaged); err != nil || len(all) != 0 {
		t.Errorf("staged assets = %v, %v", all, err)
	}
	e.svc.afterHash = nil
	r = e.reconcile(t)
	if r.AddedFiles != 1 || len(r.Errors) != 0 {
		t.Errorf("next pass = %+v", r)
	}
	e.lockFree(t, f.ID)
}

func TestReconcileNoChangesNoBump(t *testing.T) {
	e := newOpsEnv(t)
	e.write(t, "Staging/2025-06-02 - Beach/IMG_1.JPG", jpeg(t, "2025:06:02 10:00:00"))
	if r := e.reconcile(t); r.AdoptedFolders != 1 || r.AddedFiles != 1 {
		t.Fatalf("first pass = %+v", r)
	}
	v := e.version()
	r := e.reconcile(t)
	if r.changedAnything() || r.Duplicates != 0 || r.BusyFolders != 0 || len(r.Errors) != 0 {
		t.Errorf("second pass = %+v", r)
	}
	if e.version() != v {
		t.Errorf("version = %d, want %d", e.version(), v)
	}
}

func TestRescanDoesNotAdoptDirectoryBeingRenamed(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30", "2025-05-30")
	e.stage(t, f, "a.JPG", "one", opsNow)
	e.stage(t, f, "Capture/a.ARW", "two", opsNow)
	entered, out := make(chan struct{}), make(chan struct{})
	e.svc.rename = func(o, n string) error {
		if err := os.Rename(o, n); err != nil {
			return err
		}
		close(entered)
		<-out
		return nil
	}
	released := false
	release := func() {
		if !released {
			released = true
			close(out)
		}
	}
	t.Cleanup(release) // runs before the service closes, so a failure never leaves Close waiting
	renamed := make(chan error, 1)
	go func() {
		_, err := e.svc.Rename(t.Context(), f.ID, "Trip")
		renamed <- err
	}()
	waitEntered(t, entered)
	r := e.reconcile(t)
	if r.AdoptedFolders != 0 || r.BusyFolders != 1 {
		t.Fatalf("result = %+v", r)
	}
	for _, row := range e.folderRows(t) {
		if row.Name == "2025-05-30 - Trip" {
			t.Fatalf("the renamed directory was adopted while Rename was in flight: %+v", row)
		}
	}
	release()
	if err := <-renamed; err != nil {
		t.Fatal(err)
	}
	rows := e.folderRows(t)
	if len(rows) != 1 || rows[0].Name != "2025-05-30 - Trip" {
		t.Fatalf("rows = %+v", rows)
	}
	if files, err := e.db.AssetsUnder(t.Context(), prefixOf(rows[0].Name), catalog.StateStaged); err != nil || len(files) != 2 {
		t.Errorf("files = %v, %v", files, err)
	}
	v := e.version()
	r = e.reconcile(t)
	if r.changedAnything() || r.BusyFolders != 0 || e.version() != v {
		t.Errorf("second pass = %+v, version %d -> %d", r, v, e.version())
	}
	e.lockFree(t, rows[0].ID)
	e.nameKeyFree(t, rows[0].Name)
}

func TestStartRescan(t *testing.T) {
	e := newOpsEnv(t)
	e.write(t, "Staging/2025-06-02 - Beach/IMG_1.JPG", jpeg(t, "2025:06:02 10:00:00"))
	hashed, gate := make(chan struct{}), make(chan struct{})
	e.svc.afterHash = func(string) {
		close(hashed)
		<-gate
	}
	var opened bool
	open := func() {
		if !opened {
			opened = true
			close(gate)
		}
	}
	t.Cleanup(open)
	if !e.svc.StartRescan() {
		t.Fatal("StartRescan = false")
	}
	waitEntered(t, hashed)
	if st := e.svc.Status(); !st.Rescanning {
		t.Errorf("status = %+v, want rescanning", st)
	}
	if e.svc.StartRescan() {
		t.Error("a second StartRescan started while one was running")
	}
	open()
	waitFor(t, "the rescan to finish", func() bool { return !e.svc.Status().Rescanning })
	st := e.svc.Status()
	if st.LastRescan == nil || st.LastRescan.AdoptedFolders != 1 || st.LastRescan.AddedFiles != 1 {
		t.Fatalf("status = %+v", st)
	}
	e.mu.Lock()
	changes := slices.Clone(e.changes)
	e.mu.Unlock()
	sawRunning, sawDone := -1, -1
	for i, c := range changes {
		if c.Rescanning && sawRunning < 0 {
			sawRunning = i
		}
		if !c.Rescanning && c.LastRescan != nil {
			sawDone = i
		}
	}
	if sawRunning < 0 || sawDone <= sawRunning {
		t.Errorf("OnChange statuses = %+v", changes)
	}
	e.svc.Close()
	if e.svc.StartRescan() {
		t.Error("StartRescan after Close = true")
	}
}

func TestReconcileStopsWhenStagingVanishesMidPass(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-06-02 - A", "2025-06-02")
	b := e.folder(t, "2025-06-03 - B", "2025-06-03")
	sa := e.stage(t, a, "gone.JPG", "gone a", opsNow)
	sb := e.stage(t, b, "gone.JPG", "gone b", opsNow)
	e.write(t, "Staging/2025-06-02 - A/new.JPG", jpeg(t, "2025:06:02 10:00:00"))
	if err := os.Remove(filepath.Join(e.photos, Dir, a.Name, "gone.JPG")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.photos, Dir, b.Name, "gone.JPG")); err != nil {
		t.Fatal(err)
	}
	// The share drops while the new file is being hashed.
	e.svc.afterHash = func(string) {
		if err := os.RemoveAll(filepath.Join(e.photos, Dir)); err != nil {
			t.Error(err)
		}
	}
	r := e.reconcile(t)
	if r.Skipped == "" || r.CulledFiles != 0 || r.RemovedFolders != 0 {
		t.Fatalf("result = %+v", r)
	}
	for _, sha := range []string{sa, sb} {
		if st := e.asset(t, sha).State; st != catalog.StateStaged {
			t.Errorf("asset %s = %v, want staged", sha[:8], st)
		}
	}
	if rows := e.folderRows(t); len(rows) != 2 {
		t.Errorf("rows = %+v", rows)
	}
	e.lockFree(t, a.ID, b.ID)
}

func TestReconcileIgnoresNASMetadata(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-06-02 - Beach", "2025-06-02")
	img := jpeg(t, "2025:06:02 10:00:00")
	for _, rel := range []string{
		"2025-06-02 - Beach/@eaDir/real.JPG/SYNOPHOTO_THUMB_M.jpg",
		"2025-06-02 - Beach/.@__thumb/a.JPG",
		"2025-06-02 - Beach/.AppleDouble/b.JPG",
		"2025-06-02 - Beach/#recycle/c.JPG",
		"2025-06-02 - Beach/.hidden.JPG",
		"@eaDir/top.JPG",
		"#recycle/top.JPG",
		".@__thumb/top.JPG",
		".AppleDouble/top.JPG",
	} {
		e.write(t, "Staging/"+rel, img+rel)
	}
	e.write(t, "Staging/2025-06-02 - Beach/real.JPG", img)
	r := e.reconcile(t)
	if r.AddedFiles != 1 || r.AdoptedFolders != 0 {
		t.Fatalf("result = %+v", r)
	}
	if all, err := e.db.AssetsUnder(t.Context(), "Staging/", catalog.StateStaged); err != nil || len(all) != 1 ||
		all[0].RelPath != "Staging/2025-06-02 - Beach/real.JPG" {
		t.Errorf("staged assets = %v, %v", all, err)
	}
	if rows := e.folderRows(t); len(rows) != 1 || rows[0].ID != f.ID {
		t.Errorf("rows = %+v", rows)
	}
}

func TestRescanKeepsEveryProblem(t *testing.T) {
	e := newOpsEnv(t)
	e.folder(t, "2025-06-02 - Beach", "2025-06-02")
	img := jpeg(t, "2025:06:02 10:00:00")
	for i := range 25 {
		e.write(t, "Staging/2025-06-02 - Beach/f"+strconv.Itoa(1000+i)+".JPG", img+strconv.Itoa(i))
	}
	var logs strings.Builder
	e.svc.log = slog.New(slog.NewTextHandler(&logs, nil))
	n := 0
	// Every file "changes while read"; the 23rd also stops the service.
	e.svc.afterHash = func(p string) {
		n++
		w, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := w.Write([]byte{0}); err != nil {
			t.Error(err)
		}
		if err := w.Close(); err != nil {
			t.Error(err)
		}
		if n == 23 {
			e.svc.bgCancel()
		}
	}
	if !e.svc.StartRescan() {
		t.Fatal("StartRescan = false")
	}
	waitFor(t, "the rescan to finish", func() bool { return !e.svc.Status().Rescanning })
	r := e.svc.Status().LastRescan
	if r == nil || len(r.Errors) != maxRescanErrors+1 || r.ErrorsOmitted != 3 {
		t.Fatalf("result = %+v", r)
	}
	if last := r.Errors[len(r.Errors)-1]; !strings.Contains(last, "shutting down") {
		t.Errorf("last error = %q, want the stop message", last)
	}
	if got := strings.Count(logs.String(), "staging rescan problem"); got != 23+1 {
		t.Errorf("logged %d problems, want 24:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "err=") {
		t.Error("problems are not logged under the err key")
	}
}

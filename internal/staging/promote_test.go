package staging

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

// waitIdle polls Promotes until nothing runs or waits, and returns the
// last finished job.
func waitIdle(t *testing.T, s *Service) PromoteJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		q := s.Promotes()
		if q.Running == nil && len(q.Queued) == 0 && q.Last != nil {
			return *q.Last
		}
		if time.Now().After(deadline) {
			t.Fatalf("promote queue not idle: %+v", q)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// blockRename makes s.rename, for an old path ending in suffix, close
// entered and wait for release; then it fails with errno when errno isn't
// nil, else renames. Other paths rename at once. Set it before promoting
// and register t.Cleanup(release) after newOpsEnv, so a failing test never
// leaves Close waiting.
func blockRename(s *Service, suffix string, errno error) (entered <-chan struct{}, release func()) {
	in, out := make(chan struct{}), make(chan struct{})
	var onceIn, onceOut sync.Once
	s.rename = func(o, n string) error {
		if strings.HasSuffix(o, suffix) {
			onceIn.Do(func() { close(in) })
			<-out
			if errno != nil {
				return &os.LinkError{Op: "rename", Old: o, New: n, Err: errno}
			}
		}
		return os.Rename(o, n)
	}
	return in, func() { onceOut.Do(func() { close(out) }) }
}

func waitEntered(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("the blocked rename was never reached")
	}
}

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

// archiveKeyFree fails unless the lock of directory name target (a rel_path
// under Archive/) can be taken.
func (e *opsEnv) archiveKeyFree(t *testing.T, target string) {
	t.Helper()
	release, err := e.locks.TryLockSet(HolderRescan, LockSet{Names: []string{target}})
	if err != nil {
		t.Fatalf("the name lock of %q is still held: %v", target, err)
	}
	release()
}

// noRow fails when folder id still has a catalog row.
func (e *opsEnv) noRow(t *testing.T, id int64) {
	t.Helper()
	if _, err := e.db.StagingFolderByID(t.Context(), id); err == nil {
		t.Errorf("staging folder %d still has a row", id)
	}
}

func (e *opsEnv) hasRow(t *testing.T, id int64) {
	t.Helper()
	if _, err := e.db.StagingFolderByID(t.Context(), id); err != nil {
		t.Errorf("staging folder %d lost its row: %v", id, err)
	}
}

// archivedAt fails unless asset sha is archived at rel.
func (e *opsEnv) archivedAt(t *testing.T, sha, rel string) {
	t.Helper()
	a := e.asset(t, sha)
	if a.State != catalog.StateArchived || a.RelPath != rel {
		t.Errorf("asset %.8s = %s at %q, want archived at %q", sha, a.State, a.RelPath, rel)
	}
}

// queueLog collects every queue OnPromote reports.
type queueLog struct {
	mu sync.Mutex
	qs []PromoteQueue
}

func (l *queueLog) all() []PromoteQueue {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.qs)
}

func recordQueues(s *Service) *queueLog {
	l := &queueLog{}
	s.OnPromote(func(q PromoteQueue) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.qs = append(l.qs, q)
	})
	return l
}

func TestPromoteRenamesIntoArchive(t *testing.T) {
	e := newOpsEnv(t)
	log := recordQueues(e.svc)
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	s1 := e.stage(t, f, "a.JPG", "jpg", opsNow)
	s2 := e.stage(t, f, "Capture/a.ARW", "rawdata", opsNow)
	s3 := e.stage(t, f, "Videos/v.MP4", "video!", opsNow)
	e.write(t, "Staging/2025-05-30 - Trip/notes.txt", "notes")
	v := e.version()
	job, err := e.svc.Promote(t.Context(), []int64{f.ID}, "")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != PromoteQueued && job.State != PromoteRunning {
		t.Errorf("new job state = %q", job.State)
	}
	if job.FilesTotal != 3 || len(job.Folders) != 1 || job.Folders[0].Target != "Archive/2025-05-30 - Trip" {
		t.Fatalf("job = %+v", job)
	}
	done := waitIdle(t, e.svc)
	if done.State != PromoteDone || done.FilesDone != 3 || done.BytesDone != int64(len("jpg")+len("rawdata")+len("video!")) {
		t.Fatalf("finished job = %+v", done)
	}
	if fo := done.Folders[0]; fo.State != PromoteDone || fo.Done != 3 || fo.Merged || fo.Copied || fo.Error != "" {
		t.Errorf("folder = %+v", fo)
	}
	base := "Archive/2025-05-30 - Trip/"
	e.mustExist(t, base+"a.JPG", base+"Capture/a.ARW", base+"Videos/v.MP4", base+"notes.txt")
	for _, rel := range e.files(t) {
		if strings.HasPrefix(rel, Dir+"/") {
			t.Errorf("%s is still in Staging", rel)
		}
	}
	e.archivedAt(t, s1, base+"a.JPG")
	e.archivedAt(t, s2, base+"Capture/a.ARW")
	e.archivedAt(t, s3, base+"Videos/v.MP4")
	e.noRow(t, f.ID)
	e.lockFree(t, f.ID)
	e.archiveKeyFree(t, "Archive/2025-05-30 - Trip")
	e.bumped(t, v)
	qs := log.all()
	if len(qs) == 0 {
		t.Fatal("OnPromote was never called")
	}
	sawJob := false
	for _, q := range qs {
		if q.Running != nil || len(q.Queued) > 0 {
			sawJob = true
		}
	}
	if last := qs[len(qs)-1]; !sawJob || last.Running != nil || last.Last == nil || last.Last.State != PromoteDone {
		t.Errorf("OnPromote: saw a job %v, last = %+v", sawJob, qs[len(qs)-1])
	}
}

func TestPromotePattern(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	sha := e.stage(t, f, "a.JPG", "jpg", opsNow)
	if _, err := e.svc.Promote(t.Context(), []int64{f.ID}, "{year}/{month}/{name}"); err != nil {
		t.Fatal(err)
	}
	if j := waitIdle(t, e.svc); j.State != PromoteDone {
		t.Fatalf("job = %+v", j)
	}
	e.mustExist(t, "Archive/2025/05/2025-05-30 - Trip/a.JPG")
	e.archivedAt(t, sha, "Archive/2025/05/2025-05-30 - Trip/a.JPG")
	e.noRow(t, f.ID)
	e.lockFree(t, f.ID)
	e.archiveKeyFree(t, "Archive/2025/05/2025-05-30 - Trip")
}

func TestPromoteMergesIntoExistingTarget(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	s1 := e.stage(t, f, "Capture/a.ARW", "staged raw", opsNow)
	s2 := e.stage(t, f, "b.JPG", "staged jpg", opsNow)
	base := "Archive/2025-05-30 - Trip/"
	e.write(t, base+"Capture/a.ARW", "archive raw")
	e.write(t, base+"Selects/x.ARW", "selected")
	e.write(t, base+"2025-05-30 - Trip.cosessiondb", "session")
	v := e.version()
	if _, err := e.svc.Promote(t.Context(), []int64{f.ID}, ""); err != nil {
		t.Fatal(err)
	}
	j := waitIdle(t, e.svc)
	if j.State != PromoteDone || !j.Folders[0].Merged || j.Folders[0].State != PromoteDone || j.FilesDone != 2 {
		t.Fatalf("job = %+v", j)
	}
	e.archivedAt(t, s1, base+"Capture/a-1.ARW")
	e.archivedAt(t, s2, base+"b.JPG")
	for rel, want := range map[string]string{
		base + "Capture/a.ARW":                 "archive raw",
		base + "Selects/x.ARW":                 "selected",
		base + "2025-05-30 - Trip.cosessiondb": "session",
		base + "Capture/a-1.ARW":               "staged raw",
	} {
		if got := readFile(t, filepath.Join(e.photos, filepath.FromSlash(rel))); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	e.mustNotExist(t, "Staging/2025-05-30 - Trip")
	e.noRow(t, f.ID)
	e.lockFree(t, f.ID)
	e.archiveKeyFree(t, "Archive/2025-05-30 - Trip")
	e.bumped(t, v)
}

func TestPromoteTargetCreatedWhileQueued(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-01 - A", "2025-05-01")
	b := e.folder(t, "2025-05-02 - B", "2025-05-02")
	e.stage(t, a, "a.JPG", "a", opsNow)
	sb := e.stage(t, b, "b.JPG", "b", opsNow)
	entered, release := blockRename(e.svc, "2025-05-01 - A", nil)
	t.Cleanup(release)
	if _, err := e.svc.Promote(t.Context(), []int64{a.ID}, ""); err != nil {
		t.Fatal(err)
	}
	waitEntered(t, entered)
	if _, err := e.svc.Promote(t.Context(), []int64{b.ID}, "{name}"); err != nil {
		t.Fatal(err)
	}
	e.write(t, "Archive/2025-05-02 - B/unrelated.txt", "mine")
	release()
	j := waitIdle(t, e.svc)
	if j.ID != 2 || j.State != PromoteDone || !j.Folders[0].Merged || j.Folders[0].State != PromoteDone {
		t.Fatalf("job = %+v", j)
	}
	e.archivedAt(t, sb, "Archive/2025-05-02 - B/b.JPG")
	e.mustExist(t, "Archive/2025-05-02 - B/unrelated.txt")
	e.mustNotExist(t, "Staging/2025-05-02 - B")
	for _, f := range []catalog.StagingFolder{a, b} {
		e.noRow(t, f.ID)
		e.lockFree(t, f.ID)
		e.archiveKeyFree(t, "Archive/"+f.Name)
	}
}

func TestPromoteIdenticalInTarget(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	sha := e.stage(t, f, "a.JPG", "same content", opsNow)
	e.write(t, "Archive/2025-05-30 - Trip/a.JPG", "same content")
	if _, err := e.svc.Promote(t.Context(), []int64{f.ID}, ""); err != nil {
		t.Fatal(err)
	}
	j := waitIdle(t, e.svc)
	if j.State != PromoteDone || !j.Folders[0].Merged {
		t.Fatalf("job = %+v", j)
	}
	e.mustNotExist(t, "Staging/2025-05-30 - Trip")
	e.archivedAt(t, sha, "Archive/2025-05-30 - Trip/a.JPG")
	if got := e.files(t); !slices.Equal(got, []string{"Archive/2025-05-30 - Trip/a.JPG"}) {
		t.Errorf("files = %v", got)
	}
	e.noRow(t, f.ID)
	e.lockFree(t, f.ID)
	e.archiveKeyFree(t, "Archive/2025-05-30 - Trip")
}

func TestPromoteCrossDevice(t *testing.T) {
	e := newOpsEnv(t)
	e.svc.rename = func(o, n string) error { return xdev(o, n) }
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	s1 := e.stage(t, f, "a.JPG", "jpg", opsNow)
	s2 := e.stage(t, f, "Capture/a.ARW", "rawdata", opsNow)
	mtime := time.Date(2025, 5, 30, 10, 11, 12, 0, time.UTC)
	for _, rel := range []string{"a.JPG", "Capture/a.ARW"} {
		if err := os.Chtimes(filepath.Join(e.photos, Dir, "2025-05-30 - Trip", filepath.FromSlash(rel)), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	v := e.version()
	if _, err := e.svc.Promote(t.Context(), []int64{f.ID}, ""); err != nil {
		t.Fatal(err)
	}
	j := waitIdle(t, e.svc)
	if j.State != PromoteDone || !j.Folders[0].Copied || j.Folders[0].Merged || j.FilesDone != 2 {
		t.Fatalf("job = %+v", j)
	}
	base := "Archive/2025-05-30 - Trip/"
	for rel, want := range map[string]string{"a.JPG": "jpg", "Capture/a.ARW": "rawdata"} {
		p := filepath.Join(e.photos, filepath.FromSlash(base+rel))
		if got := readFile(t, p); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
		if st, err := os.Stat(p); err != nil || !st.ModTime().Equal(mtime) {
			t.Errorf("%s mtime = %v (%v), want %v", rel, st, err, mtime)
		}
	}
	e.mustNotExist(t, "Staging/2025-05-30 - Trip")
	e.archivedAt(t, s1, base+"a.JPG")
	e.archivedAt(t, s2, base+"Capture/a.ARW")
	e.noRow(t, f.ID)
	e.lockFree(t, f.ID)
	e.archiveKeyFree(t, "Archive/2025-05-30 - Trip")
	e.bumped(t, v)
}

func TestPromoteCrossDeviceVerifyFails(t *testing.T) {
	e := newOpsEnv(t)
	e.svc.rename = func(o, n string) error { return xdev(o, n) }
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	sha := e.stage(t, f, "a.JPG", "recorded content", opsNow)
	e.write(t, "Staging/2025-05-30 - Trip/a.JPG", "changed on disk")
	v := e.version()
	if _, err := e.svc.Promote(t.Context(), []int64{f.ID}, ""); err != nil {
		t.Fatal(err)
	}
	j := waitIdle(t, e.svc)
	if j.State != PromoteFailed || j.Folders[0].State != PromoteFailed || !strings.Contains(j.Folders[0].Error, "does not match") {
		t.Fatalf("job = %+v", j)
	}
	if got := readFile(t, filepath.Join(e.photos, Dir, "2025-05-30 - Trip", "a.JPG")); got != "changed on disk" {
		t.Errorf("the source = %q", got)
	}
	if got := e.files(t); !slices.Equal(got, []string{"Staging/2025-05-30 - Trip/a.JPG"}) {
		t.Errorf("files = %v: the copy must be gone and the source kept", got)
	}
	if a := e.asset(t, sha); a.State != catalog.StateStaged || a.RelPath != "Staging/2025-05-30 - Trip/a.JPG" {
		t.Errorf("asset = %+v", a)
	}
	e.hasRow(t, f.ID)
	e.lockFree(t, f.ID)
	e.archiveKeyFree(t, "Archive/2025-05-30 - Trip")
	e.bumped(t, v)
}

func TestPromoteCullsMissingTracked(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	keep := e.stage(t, f, "a.JPG", "keep", opsNow)
	gone := e.stage(t, f, "b.JPG", "gone", opsNow)
	if err := os.Remove(filepath.Join(e.photos, Dir, "2025-05-30 - Trip", "b.JPG")); err != nil {
		t.Fatal(err)
	}
	v := e.version()
	if _, err := e.svc.Promote(t.Context(), []int64{f.ID}, ""); err != nil {
		t.Fatal(err)
	}
	j := waitIdle(t, e.svc)
	if j.State != PromoteDone || j.FilesTotal != 2 || j.FilesDone != 1 {
		t.Fatalf("job = %+v", j)
	}
	if a := e.asset(t, gone); a.State != catalog.StateCulled {
		t.Errorf("missing file state = %s, want culled", a.State)
	}
	if got := e.removedHashes(); !slices.Equal(got, []string{gone}) {
		t.Errorf("removed = %v, want [%s]", got, gone)
	}
	e.archivedAt(t, keep, "Archive/2025-05-30 - Trip/a.JPG")
	e.noRow(t, f.ID)
	e.lockFree(t, f.ID)
	e.archiveKeyFree(t, "Archive/2025-05-30 - Trip")
	e.bumped(t, v)
}

func TestPromoteBadPattern(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	e.stage(t, f, "a.JPG", "jpg", opsNow)
	if _, err := e.svc.Promote(t.Context(), []int64{f.ID}, "{day}"); !errors.Is(err, ErrBadPattern) {
		t.Fatalf("err = %v, want ErrBadPattern", err)
	}
	q := e.svc.Promotes()
	if q.Running != nil || len(q.Queued) != 0 || q.Last != nil {
		t.Errorf("queue = %+v", q)
	}
	e.lockFree(t, f.ID)
	e.archiveKeyFree(t, "Archive/2025-05-30 - Trip")
	// No id was used: the next job is 1.
	job, err := e.svc.Promote(t.Context(), []int64{f.ID}, "")
	if err != nil || job.ID != 1 {
		t.Fatalf("next job = %+v, %v", job, err)
	}
	waitIdle(t, e.svc)
	e.lockFree(t, f.ID)
}

func TestPromoteBusy(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	e.stage(t, f, "a.JPG", "jpg", opsNow)
	release, err := e.locks.TryLock(HolderIngest, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Promote(t.Context(), []int64{f.ID}, "")
	var busy *BusyError
	if !errors.As(err, &busy) || busy.Holder != HolderIngest || busy.Name != f.Name || busy.ID != f.ID {
		t.Fatalf("err = %v, want a BusyError for the import naming %s", err, f.Name)
	}
	if q := e.svc.Promotes(); q.Running != nil || len(q.Queued) != 0 || q.Last != nil {
		t.Errorf("queue = %+v", q)
	}
	e.archiveKeyFree(t, "Archive/2025-05-30 - Trip")
	release()
	e.lockFree(t, f.ID)
	e.mustExist(t, "Staging/2025-05-30 - Trip/a.JPG")
}

func TestPromoteQueueFIFO(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-01 - A", "2025-05-01")
	b := e.folder(t, "2025-05-02 - B", "2025-05-02")
	c := e.folder(t, "2025-05-03 - C", "2025-05-03")
	for _, f := range []catalog.StagingFolder{a, b, c} {
		e.stage(t, f, "x.JPG", "x"+f.Name, opsNow)
	}
	entered, release := blockRename(e.svc, "2025-05-01 - A", nil)
	t.Cleanup(release)
	inner := e.svc.rename
	var mu sync.Mutex
	var order []string
	e.svc.rename = func(o, n string) error {
		mu.Lock()
		order = append(order, filepath.Base(o))
		mu.Unlock()
		return inner(o, n)
	}
	j1, err := e.svc.Promote(t.Context(), []int64{a.ID}, "")
	if err != nil || j1.ID != 1 {
		t.Fatalf("job 1 = %+v, %v", j1, err)
	}
	waitEntered(t, entered)
	j2, err := e.svc.Promote(t.Context(), []int64{b.ID}, "")
	if err != nil || j2.ID != 2 || j2.State != PromoteQueued {
		t.Fatalf("job 2 = %+v, %v", j2, err)
	}
	j3, err := e.svc.Promote(t.Context(), []int64{c.ID}, "")
	if err != nil || j3.ID != 3 || j3.State != PromoteQueued {
		t.Fatalf("job 3 = %+v, %v", j3, err)
	}
	q := e.svc.Promotes()
	if q.Running == nil || q.Running.ID != 1 || q.Running.State != PromoteRunning || len(q.Queued) != 2 || q.Queued[0].ID != 2 || q.Queued[1].ID != 3 {
		t.Fatalf("queue = %+v", q)
	}
	release()
	if last := waitIdle(t, e.svc); last.ID != 3 || last.State != PromoteDone {
		t.Fatalf("last = %+v", last)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"2025-05-01 - A", "2025-05-02 - B", "2025-05-03 - C"}; !slices.Equal(order, want) {
		t.Errorf("renames = %v, want %v", order, want)
	}
	for _, f := range []catalog.StagingFolder{a, b, c} {
		e.noRow(t, f.ID)
		e.lockFree(t, f.ID)
		e.archiveKeyFree(t, "Archive/"+f.Name)
	}
}

func TestPromoteEnqueueLocksAtOnce(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-01 - A", "2025-05-01")
	b := e.folder(t, "2025-05-02 - B", "2025-05-02")
	e.stage(t, a, "a.JPG", "a", opsNow)
	e.stage(t, b, "b.JPG", "b", opsNow)
	entered, release := blockRename(e.svc, "2025-05-01 - A", nil)
	t.Cleanup(release)
	if _, err := e.svc.Promote(t.Context(), []int64{a.ID}, ""); err != nil {
		t.Fatal(err)
	}
	waitEntered(t, entered)
	if _, err := e.svc.Promote(t.Context(), []int64{b.ID}, ""); err != nil {
		t.Fatal(err)
	}
	wantBusy := func(what string, err error, f catalog.StagingFolder, queued bool) {
		t.Helper()
		var busy *BusyError
		if !errors.As(err, &busy) || busy.ID != f.ID || busy.Name != f.Name || busy.Holder != HolderPromote || busy.Queued != queued {
			t.Errorf("%s: err = %v (%+v), want a promote BusyError for %s, queued %v", what, err, busy, f.Name, queued)
		}
	}
	_, err := e.svc.Rename(t.Context(), b.ID, "x")
	wantBusy("Rename(B)", err, b, true)
	_, err = e.svc.Discard(t.Context(), b.ID)
	wantBusy("Discard(B)", err, b, true)
	_, err = e.svc.Promote(t.Context(), []int64{b.ID}, "")
	wantBusy("Promote(B)", err, b, true)
	_, err = e.svc.Rename(t.Context(), a.ID, "x")
	wantBusy("Rename(A)", err, a, false)
	if _, err := e.locks.TryLockSet(HolderRescan, LockSet{Names: []string{"Archive/" + b.Name}}); !errors.Is(err, ErrBusy) {
		t.Errorf("B's target name is not locked: %v", err)
	}
	release()
	waitIdle(t, e.svc)
	for _, f := range []catalog.StagingFolder{a, b} {
		e.lockFree(t, f.ID)
		e.archiveKeyFree(t, "Archive/"+f.Name)
	}
}

func TestPromoteCancelQueued(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-01 - A", "2025-05-01")
	b := e.folder(t, "2025-05-02 - B", "2025-05-02")
	e.stage(t, a, "a.JPG", "a", opsNow)
	sb := e.stage(t, b, "b.JPG", "b", opsNow)
	entered, release := blockRename(e.svc, "2025-05-01 - A", nil)
	t.Cleanup(release)
	if _, err := e.svc.Promote(t.Context(), []int64{a.ID}, ""); err != nil {
		t.Fatal(err)
	}
	waitEntered(t, entered)
	if _, err := e.svc.Promote(t.Context(), []int64{b.ID}, ""); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.CancelPromote(2)
	if err != nil || got.ID != 2 || got.State != PromoteCancelled {
		t.Fatalf("CancelPromote(2) = %+v, %v", got, err)
	}
	e.lockFree(t, b.ID)
	e.archiveKeyFree(t, "Archive/"+b.Name)
	if q := e.svc.Promotes(); len(q.Queued) != 0 || q.Queued == nil {
		t.Errorf("queued = %#v, want an empty non-nil slice", q.Queued)
	}
	if _, err := e.svc.CancelPromote(1); !errors.Is(err, ErrJobStarted) {
		t.Errorf("CancelPromote(running) = %v", err)
	}
	if _, err := e.svc.CancelPromote(2); !errors.Is(err, ErrJobStarted) {
		t.Errorf("CancelPromote(again) = %v", err)
	}
	if _, err := e.svc.CancelPromote(99); !errors.Is(err, ErrNoSuchJob) {
		t.Errorf("CancelPromote(99) = %v", err)
	}
	release()
	if last := waitIdle(t, e.svc); last.ID != 1 || last.State != PromoteDone {
		t.Fatalf("last = %+v", last)
	}
	e.mustExist(t, "Staging/2025-05-02 - B/b.JPG")
	e.mustNotExist(t, "Archive/2025-05-02 - B")
	e.hasRow(t, b.ID)
	if a := e.asset(t, sb); a.State != catalog.StateStaged || a.RelPath != "Staging/2025-05-02 - B/b.JPG" {
		t.Errorf("asset = %+v", a)
	}
	e.lockFree(t, a.ID, b.ID)
}

func TestPromoteReleasesEachFolder(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-01 - A", "2025-05-01")
	b := e.folder(t, "2025-05-02 - B", "2025-05-02")
	sa := e.stage(t, a, "a.JPG", "a", opsNow)
	sb := e.stage(t, b, "b.JPG", "b", opsNow)
	entered, release := blockRename(e.svc, "2025-05-02 - B", nil)
	t.Cleanup(release)
	if _, err := e.svc.Promote(t.Context(), []int64{a.ID, b.ID}, ""); err != nil {
		t.Fatal(err)
	}
	waitEntered(t, entered)
	// A was finished before B's rename began.
	e.lockFree(t, a.ID)
	e.archiveKeyFree(t, "Archive/"+a.Name)
	if _, err := e.locks.TryLock(HolderRescan, b.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("B's lock is free while B is being moved: %v", err)
	}
	if q := e.svc.Promotes(); q.Running == nil || q.Running.Folders[0].State != PromoteDone || q.Running.Folders[1].State != PromoteMoving {
		t.Errorf("running = %+v", q.Running)
	}
	e.archivedAt(t, sa, "Archive/"+a.Name+"/a.JPG")
	release()
	if j := waitIdle(t, e.svc); j.State != PromoteDone || j.FilesDone != 2 {
		t.Fatalf("job = %+v", j)
	}
	e.archivedAt(t, sb, "Archive/"+b.Name+"/b.JPG")
	e.lockFree(t, a.ID, b.ID)
	e.archiveKeyFree(t, "Archive/"+b.Name)
}

func TestPromoteStopsOnClose(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	e.stage(t, f, "a.JPG", "a", opsNow)
	e.stage(t, f, "Capture/a.ARW", "b", opsNow)
	entered, release := blockRename(e.svc, "2025-05-30 - Trip", syscall.EXDEV)
	t.Cleanup(release)
	if _, err := e.svc.Promote(t.Context(), []int64{f.ID}, ""); err != nil {
		t.Fatal(err)
	}
	waitEntered(t, entered)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		e.svc.Close()
	}()
	waitFor(t, "Close to cancel the context", func() bool { return e.svc.bgCtx.Err() != nil })
	release()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	j := e.svc.Promotes().Last
	if j == nil || j.State != PromoteFailed || j.Folders[0].State != PromoteFailed || !strings.Contains(j.Folders[0].Error, "shutting down") {
		t.Fatalf("last = %+v", j)
	}
	if got := e.files(t); !slices.Equal(got, []string{"Staging/2025-05-30 - Trip/Capture/a.ARW", "Staging/2025-05-30 - Trip/a.JPG"}) {
		t.Errorf("files = %v", got)
	}
	if entries, err := os.ReadDir(filepath.Join(e.photos, ArchiveDir)); err != nil || len(entries) != 0 {
		t.Errorf("Archive/ = %v, %v; want it empty", entries, err)
	}
	e.hasRow(t, f.ID)
	e.lockFree(t, f.ID)
	e.archiveKeyFree(t, "Archive/"+f.Name)
	if _, err := e.svc.Promote(t.Context(), []int64{f.ID}, ""); !errors.Is(err, ErrClosed) {
		t.Errorf("Promote after Close = %v, want ErrClosed", err)
	}
}

// syncBuffer is a log sink the test and the service's goroutines share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestPromoteCloseDropsQueue(t *testing.T) {
	e := newOpsEnv(t)
	var buf syncBuffer
	svc := NewService(Config{DB: e.db, PhotosRoot: e.photos, Locks: e.locks, Log: slog.New(slog.NewTextHandler(&buf, nil))})
	t.Cleanup(svc.Close)
	a := e.folder(t, "2025-05-01 - A", "2025-05-01")
	b := e.folder(t, "2025-05-02 - B", "2025-05-02")
	e.stage(t, a, "a.JPG", "a", opsNow)
	e.stage(t, b, "b.JPG", "b", opsNow)
	entered, release := blockRename(svc, "2025-05-01 - A", nil)
	t.Cleanup(release)
	if _, err := svc.Promote(t.Context(), []int64{a.ID}, ""); err != nil {
		t.Fatal(err)
	}
	waitEntered(t, entered)
	if _, err := svc.Promote(t.Context(), []int64{b.ID}, ""); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		svc.Close()
	}()
	waitFor(t, "Close to cancel the context", func() bool { return svc.bgCtx.Err() != nil })
	release()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	if q := svc.Promotes(); len(q.Queued) != 0 {
		t.Errorf("queued = %+v", q.Queued)
	}
	e.mustExist(t, "Staging/2025-05-02 - B/b.JPG")
	e.mustNotExist(t, "Archive/2025-05-02 - B")
	e.hasRow(t, b.ID)
	e.lockFree(t, b.ID)
	e.archiveKeyFree(t, "Archive/"+b.Name)
	if !strings.Contains(buf.String(), "dropped queued promotes") {
		t.Errorf("log = %s", buf.String())
	}
}

func TestPromoteProgressThrottled(t *testing.T) {
	e := newOpsEnv(t)
	log := recordQueues(e.svc)
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	for i := range 60 {
		e.stage(t, f, fmt.Sprintf("f%02d.JPG", i), fmt.Sprintf("content %d", i), opsNow)
	}
	e.write(t, "Archive/2025-05-30 - Trip/other.txt", "existing")
	if _, err := e.svc.Promote(t.Context(), []int64{f.ID}, ""); err != nil {
		t.Fatal(err)
	}
	j := waitIdle(t, e.svc)
	if j.State != PromoteDone || !j.Folders[0].Merged || j.FilesDone != 60 {
		t.Fatalf("job = %+v", j)
	}
	qs := log.all()
	if len(qs) == 0 || len(qs) > 8 {
		t.Fatalf("OnPromote was called %d times, want 1 to 8", len(qs))
	}
	if last := qs[len(qs)-1]; last.Last == nil || last.Last.FilesDone != 60 || last.Last.State != PromoteDone || last.Running != nil {
		t.Errorf("last report = %+v", last)
	}
	e.lockFree(t, f.ID)
	e.archiveKeyFree(t, "Archive/2025-05-30 - Trip")
}

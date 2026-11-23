package ingest

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
	"github.com/aspiziri/SD2NAS/internal/staging"
)

type runResult struct {
	out catalog.Ingest
	err error
}

// runAsync runs the queued ingest in on e's runner in the background.
func runAsync(ctx context.Context, e *env, in catalog.Ingest, p *Progress) <-chan runResult {
	done := make(chan runResult, 1)
	go func() {
		out, err := e.r.Run(ctx, in, p)
		done <- runResult{out, err}
	}()
	return done
}

func waitRun(t *testing.T, done <-chan runResult) catalog.Ingest {
	t.Helper()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.out
	case <-time.After(10 * time.Second):
		t.Fatal("import did not finish")
		return catalog.Ingest{}
	}
}

func waitPhase(t *testing.T, p *Progress, phase string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for p.Snapshot().Phase != phase {
		if time.Now().After(deadline) {
			t.Fatalf("phase is %q, want %q", p.Snapshot().Phase, phase)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// queued creates the ingest row for card.
func queued(t *testing.T, e *env, card string) catalog.Ingest {
	t.Helper()
	in, err := e.cat.CreateIngest(t.Context(), card, filepath.Base(card), "", testNow)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestRunHoldsFolderLocks(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	started, release := gate(e)
	defer release()
	done := runAsync(t.Context(), e, queued(t, e, card), NewProgress())
	waitStarted(t, started)

	folders, err := e.cat.StagingFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 {
		t.Fatalf("%d staging folders, want 1", len(folders))
	}
	_, err = e.r.locks.TryLock(staging.HolderOperation, folders[0].ID)
	var be *staging.BusyError
	if !errors.As(err, &be) || be.Holder != staging.HolderIngest {
		t.Fatalf("TryLock err = %v, want a BusyError held by ingest", err)
	}

	release()
	if out := waitRun(t, done); out.Status != catalog.StatusDone {
		t.Fatalf("status = %s (%s), want done", out.Status, out.Error)
	}
	unlock, err := e.r.locks.TryLock(staging.HolderOperation, folders[0].ID)
	if err != nil {
		t.Fatalf("the folder is still locked after the import: %v", err)
	}
	unlock()
}

// waitBlockedInLock waits until some goroutine is inside Locks.Lock, so the
// tests below know the import reached its wait (and will re-look the folder
// up) before they change the folder.
func waitBlockedInLock(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), "staging.(*Locks).Lock(") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the import never waited for the folder lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// blockedImport holds the 2025-05-30 folder, starts an import of the Sony
// card and returns once it waits for the folder. copied is closed when a
// copy starts.
type blockedImport struct {
	e      *env
	folder catalog.StagingFolder
	unlock func()
	copied chan struct{}
	done   <-chan runResult
	cancel context.CancelFunc
}

func startBlocked(t *testing.T) *blockedImport {
	t.Helper()
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	f, err := e.cat.StagingFolderForDate(t.Context(), "2025-05-30", testNow)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := e.r.locks.TryLock(staging.HolderOperation, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	copied := make(chan struct{})
	var once sync.Once
	e.r.copyFile = func(ctx context.Context, src, dst string) (string, error) {
		once.Do(func() { close(copied) })
		return media.CopyHashed(ctx, src, dst)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	p := NewProgress()
	b := &blockedImport{e: e, folder: f, unlock: unlock, copied: copied, cancel: cancel}
	b.done = runAsync(ctx, e, queued(t, e, card), p)
	waitPhase(t, p, PhaseCopying)
	waitBlockedInLock(t)
	select {
	case <-copied:
		t.Fatal("a copy started while the folder was locked")
	default:
	}
	return b
}

func TestRunWaitsForBusyFolder(t *testing.T) {
	b := startBlocked(t)
	b.unlock()
	out := waitRun(t, b.done)
	if out.Status != catalog.StatusDone || out.Counts.New != 2 {
		t.Fatalf("status %s, counts %+v, want done with 2 new", out.Status, out.Counts)
	}
	got := b.e.staged(t)
	if len(got) != 2 {
		t.Fatalf("staged %v, want 2 files", got)
	}
	for _, p := range got {
		if !strings.HasPrefix(p, "Staging/2025-05-30/") {
			t.Errorf("%s is not under Staging/2025-05-30/", p)
		}
	}
}

func TestRunFollowsFolderRenamedWhileWaiting(t *testing.T) {
	b := startBlocked(t)
	err := b.e.cat.RenameStagingFolder(t.Context(), b.folder.ID, "2025-05-30 - Trip",
		"Staging/2025-05-30/", "Staging/2025-05-30 - Trip/", testNow)
	if err != nil {
		t.Fatal(err)
	}
	b.unlock()
	out := waitRun(t, b.done)
	if out.Status != catalog.StatusDone || out.Counts.New != 2 {
		t.Fatalf("status %s, counts %+v, want done with 2 new", out.Status, out.Counts)
	}
	got := b.e.staged(t)
	if len(got) != 2 {
		t.Fatalf("staged %v, want 2 files", got)
	}
	for _, p := range got {
		if !strings.HasPrefix(p, "Staging/2025-05-30 - Trip/") {
			t.Errorf("%s is not under Staging/2025-05-30 - Trip/", p)
		}
	}
	for _, f := range b.e.files(t, out.ID) {
		if f.Outcome != catalog.OutcomeNew {
			continue
		}
		a, err := b.e.cat.AssetBySHA(t.Context(), f.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(a.RelPath, "Staging/2025-05-30 - Trip/") {
			t.Errorf("asset RelPath %s is not under the renamed folder", a.RelPath)
		}
	}
}

func TestRunFolderDeletedWhileWaiting(t *testing.T) {
	b := startBlocked(t)
	if err := b.e.cat.DeleteStagingFolder(t.Context(), b.folder.ID); err != nil {
		t.Fatal(err)
	}
	b.unlock()
	out := waitRun(t, b.done)
	if out.Status != catalog.StatusDone || out.Counts.New != 2 {
		t.Fatalf("status %s, counts %+v, want done with 2 new", out.Status, out.Counts)
	}
	f, err := b.e.cat.StagingFolderForDate(t.Context(), "2025-05-30", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if f.ID == b.folder.ID {
		t.Fatalf("the folder row was not recreated: id %d", f.ID)
	}
}

func TestRunCancelledWhileWaiting(t *testing.T) {
	b := startBlocked(t)
	b.cancel()
	out := waitRun(t, b.done)
	if out.Status != catalog.StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled", out.Status, out.Error)
	}
	if got := b.e.staged(t); len(got) != 0 {
		t.Fatalf("staged %v, want nothing", got)
	}
	// The cancelled import must not have taken or freed the test's lock.
	_, err := b.e.r.locks.TryLock(staging.HolderIngest, b.folder.ID)
	var be *staging.BusyError
	if !errors.As(err, &be) || be.Holder != staging.HolderOperation {
		t.Fatalf("TryLock err = %v, want the test's operation lock still held", err)
	}
	b.unlock()
}

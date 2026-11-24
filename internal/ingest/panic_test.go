package ingest

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
	"github.com/aspiziri/SD2NAS/internal/staging"
)

// submitCard submits card to m and waits for the job to finish.
func submitCard(t *testing.T, m *Manager, card string) catalog.Ingest {
	t.Helper()
	j, err := m.Submit(t.Context(), Request{Path: card, Label: "A"})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, j)
	return j.Result()
}

func TestWorkerSurvivesPanic(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	var calls atomic.Int32
	e.r.copyFile = func(ctx context.Context, src, dst string) (string, error) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		return media.CopyHashed(ctx, src, dst)
	}
	m := NewManager(e.r, e.cards)
	m.Start()

	out := submitCard(t, m, card)
	const want = "internal error: boom"
	if out.Status != catalog.StatusFailed || out.Error != want || out.SafeToFormat {
		t.Errorf("result = status %q, error %q, safe %v; want failed, %q, false", out.Status, out.Error, out.SafeToFormat, want)
	}
	level, msg := Describe(out)
	if level != LevelAlert || !strings.HasSuffix(msg, "import failed: internal error: boom. Card left mounted.") {
		t.Errorf("Describe = %q, %q", level, msg)
	}
	row, err := e.cat.Ingest(t.Context(), out.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != catalog.StatusFailed || row.Error != want {
		t.Errorf("catalog row = status %q, error %q", row.Status, row.Error)
	}

	again := submitCard(t, m, card)
	if again.Status != catalog.StatusDone {
		t.Errorf("second import = status %q, error %q; want done", again.Status, again.Error)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Errorf("Close = %v", err)
	}
}

func TestPanicReleasesFolderLocks(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	e.r.copyFile = func(context.Context, string, string) (string, error) {
		panic("boom")
	}
	m := newManager(t, e)

	out := submitCard(t, m, card)
	if out.Status != catalog.StatusFailed {
		t.Fatalf("status = %q, want failed", out.Status)
	}
	folders, err := e.cat.StagingFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 {
		t.Fatalf("staging folders = %+v, want the one the run created", folders)
	}
	release, err := e.r.locks.TryLock(staging.HolderOperation, folders[0].ID)
	if err != nil {
		t.Fatalf("folder %d still locked after the panic: %v", folders[0].ID, err)
	}
	release()
}

func TestClassifyPanicFailsFile(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	e.r.fingerprint = func(p string) (string, error) {
		if strings.HasSuffix(p, ".ARW") {
			panic("bad raw")
		}
		return media.QuickFingerprint(p)
	}
	m := newManager(t, e)

	out := submitCard(t, m, card)
	if out.Status != catalog.StatusDone || out.Counts.Failed != 1 || out.SafeToFormat {
		t.Errorf("result = status %q, error %q, counts %+v, safe %v; want done, 1 failed, not safe",
			out.Status, out.Error, out.Counts, out.SafeToFormat)
	}
	row := e.files(t, out.ID)[cardARW]
	if row.Outcome != catalog.OutcomeFailed || row.Error != "internal error: bad raw" {
		t.Errorf("ARW row = %+v", row)
	}
}

func TestPanicOnSecondFileKeepsCounts(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	var calls atomic.Int32
	e.r.copyFile = func(ctx context.Context, src, dst string) (string, error) {
		if calls.Add(1) == 2 {
			panic("boom")
		}
		return media.CopyHashed(ctx, src, dst)
	}
	m := newManager(t, e)

	out := submitCard(t, m, card)
	if out.Status != catalog.StatusFailed || out.Error != "internal error: boom" {
		t.Fatalf("result = status %q, error %q; want failed, internal error: boom", out.Status, out.Error)
	}
	if out.Counts.New != 1 {
		t.Errorf("Counts.New = %d, want 1 (the file copied before the panic)", out.Counts.New)
	}
	level, msg := Describe(out)
	if level != LevelAlert || !strings.Contains(msg, " 1 new copied before it stopped.") {
		t.Errorf("Describe = %q, %q; want an alert containing %q", level, msg, " 1 new copied before it stopped.")
	}
	row, err := e.cat.Ingest(t.Context(), out.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Counts.New != 1 {
		t.Errorf("catalog row Counts.New = %d, want 1", row.Counts.New)
	}
}

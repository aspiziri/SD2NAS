package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
	mt "github.com/aspiziri/SD2NAS/internal/media/mediatest"
)

func waitDone(t *testing.T, j *Job) {
	t.Helper()
	select {
	case <-j.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("job %d did not finish", j.ID())
	}
}

func newManager(t *testing.T, e *env) *Manager {
	t.Helper()
	m := NewManager(e.r, e.cards)
	m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	return m
}

// gate makes every copy announce its source on started, then wait for
// release (or cancellation) before copying. release is safe to call twice.
func gate(e *env) (started <-chan string, release func()) {
	st := make(chan string, 16)
	rel := make(chan struct{})
	var once sync.Once
	e.r.copyFile = func(ctx context.Context, src, dst string) (string, error) {
		st <- src
		select {
		case <-rel:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return media.CopyHashed(ctx, src, dst)
	}
	return st, func() { once.Do(func() { close(rel) }) }
}

func waitStarted(t *testing.T, started <-chan string) string {
	t.Helper()
	select {
	case s := <-started:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("no copy started")
		return ""
	}
}

func TestValidateCardPath(t *testing.T) {
	cards := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(cards, "SONY"), 0o777); err != nil {
		t.Fatal(err)
	}
	writeFile(t, cards, "f.txt", []byte("x"))
	good := filepath.Join(cards, "SONY")
	got, err := ValidateCardPath(cards, good)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(good)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	bad := map[string]string{
		"root":     cards,
		"outside":  filepath.Join(cards, "..", filepath.Base(outside)),
		"relative": "SONY",
		"missing":  filepath.Join(cards, "missing"),
		"file":     filepath.Join(cards, "f.txt"),
	}
	if err := os.Symlink(outside, filepath.Join(cards, "link")); err == nil {
		bad["symlink"] = filepath.Join(cards, "link")
	}
	for name, p := range bad {
		_, err := ValidateCardPath(cards, p)
		if err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if name == "missing" && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("missing: %v is not ErrNotExist", err)
		}
	}
}

func TestManagerRunsOneAtATime(t *testing.T) {
	e := newEnv(t)
	a := e.fixtureCard(t, "SONY_A7IV")
	b := e.fixtureCard(t, "GOPRO")
	started, release := gate(e)
	t.Cleanup(release)
	m := newManager(t, e)
	ctx := t.Context()

	ja, err := m.Submit(ctx, Request{Path: a})
	if err != nil {
		t.Fatal(err)
	}
	first := waitStarted(t, started)
	jb, err := m.Submit(ctx, Request{Path: b})
	if err != nil {
		t.Fatal(err)
	}
	if jb.Result().Status != catalog.StatusQueued {
		t.Errorf("B status %q", jb.Result().Status)
	}
	running, queued := m.Current()
	if running != ja || len(queued) != 1 || queued[0] != jb {
		t.Errorf("Current = %v, %v", running, queued)
	}
	again, err := m.Submit(ctx, Request{Path: a})
	if err != nil {
		t.Fatal(err)
	}
	if again != ja {
		t.Error("resubmitting A gave a different job")
	}
	rows, err := e.cat.Ingests(ctx, 10, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("ingests %d, %v", len(rows), err)
	}
	release()
	waitDone(t, ja)
	waitDone(t, jb)
	for _, j := range []*Job{ja, jb} {
		if j.Result().Status != catalog.StatusDone {
			t.Errorf("job %d: %q %q", j.ID(), j.Result().Status, j.Result().Error)
		}
	}
	got := []string{first}
	for len(started) > 0 {
		got = append(got, <-started)
	}
	if len(got) != 3 {
		t.Fatalf("copies: %v", got)
	}
	// A's two files come first, B's single file last.
	for i, s := range got {
		inB := strings.Contains(s, filepath.Base(b))
		if (i == 2) != inB {
			t.Errorf("copy %d (%s): in B = %v", i, s, inB)
		}
	}
}

func TestManagerQueueFull(t *testing.T) {
	e := newEnv(t)
	sony := e.fixtureCard(t, "SONY_A7IV")
	started, release := gate(e)
	t.Cleanup(release)
	m := newManager(t, e)
	ctx := t.Context()
	var paths []string
	for i := 1; i <= 9; i++ {
		c := filepath.Join(e.cards, fmt.Sprintf("C%d", i))
		writeJPEG(t, c, "DCIM/A.JPG", mt.ExifSpec{})
		paths = append(paths, c)
	}
	if _, err := m.Submit(ctx, Request{Path: sony}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, started)
	for _, p := range paths[:MaxQueued] {
		if _, err := m.Submit(ctx, Request{Path: p}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Submit(ctx, Request{Path: paths[MaxQueued]}); !errors.Is(err, ErrQueueFull) {
		t.Errorf("got %v, want ErrQueueFull", err)
	}
}

func TestManagerCloseCancelsRunning(t *testing.T) {
	e := newEnv(t)
	a := e.fixtureCard(t, "SONY_A7IV")
	b := e.fixtureCard(t, "GOPRO")
	started, release := gate(e)
	t.Cleanup(release)
	m := NewManager(e.r, e.cards)
	m.Start()
	ctx := t.Context()
	ja, err := m.Submit(ctx, Request{Path: a})
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, started)
	jb, err := m.Submit(ctx, Request{Path: b})
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Close(cctx); err != nil {
		t.Fatal(err)
	}
	waitDone(t, ja)
	waitDone(t, jb)
	ra, rb := ja.Result(), jb.Result()
	if ra.Status != catalog.StatusCancelled || ra.SafeToFormat {
		t.Errorf("A: %q safe=%v", ra.Status, ra.SafeToFormat)
	}
	if rb.Status != catalog.StatusCancelled || rb.Error != "SD2NAS stopped before this import started" || rb.FinishedAt.IsZero() {
		t.Errorf("B: %+v", rb)
	}
	for _, r := range []catalog.Ingest{ra, rb} {
		row, err := e.cat.Ingest(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status != r.Status || row.Error != r.Error || row.SafeToFormat != r.SafeToFormat || !row.FinishedAt.Equal(r.FinishedAt) {
			t.Errorf("catalog %+v disagrees with result %+v", row, r)
		}
	}
	if s := e.staged(t); s != nil {
		t.Errorf("staged leftovers: %v", s)
	}
	if _, err := m.Submit(ctx, Request{Path: a}); !errors.Is(err, ErrClosed) {
		t.Errorf("submit after close: %v", err)
	}
	if err := m.Close(cctx); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestManagerRecover(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	q, err := e.cat.CreateIngest(ctx, "/cards/Q", "Q", "", testNow)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.cat.CreateIngest(ctx, "/cards/R", "R", "", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.StartIngest(ctx, r.ID, testNow); err != nil {
		t.Fatal(err)
	}
	stg := writeFile(t, e.photos, "Staging/2025-05-30/x.JPG.partial", []byte("x"))
	arc := writeFile(t, e.photos, "Archive/y.JPG.partial", []byte("y"))
	m := NewManager(e.r, e.cards)
	if err := m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{q.ID, r.ID} {
		row, err := e.cat.Ingest(ctx, id)
		if err != nil || row.Status != catalog.StatusFailed {
			t.Errorf("ingest %d: %q, %v", id, row.Status, err)
		}
	}
	if _, err := os.Stat(stg); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("staging partial remains: %v", err)
	}
	if _, err := os.Stat(arc); err != nil {
		t.Errorf("archive partial: %v", err)
	}
}

func TestManagerSubmitBadPath(t *testing.T) {
	e := newEnv(t)
	m := newManager(t, e)
	_, err := m.Submit(t.Context(), Request{Path: filepath.Dir(e.cards)})
	if !errors.Is(err, ErrBadCardPath) {
		t.Fatalf("got %v", err)
	}
	rows, err := e.cat.Ingests(t.Context(), 10, 0)
	if err != nil || len(rows) != 0 {
		t.Errorf("rows %d, %v", len(rows), err)
	}
}

func TestManagerDefaultLabel(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "GOPRO")
	m := newManager(t, e)
	j, err := m.Submit(t.Context(), Request{Path: card})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, j)
	if got := j.Result().CardLabel; got != filepath.Base(card) {
		t.Errorf("label %q", got)
	}
	c2 := filepath.Join(e.cards, "OTHER")
	writeJPEG(t, c2, "DCIM/A.JPG", mt.ExifSpec{})
	j2, err := m.Submit(t.Context(), Request{Path: c2, Label: "  CARD  "})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, j2)
	if got := j2.Result().CardLabel; got != "CARD" {
		t.Errorf("label %q", got)
	}
}

func TestJobProgress(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "GOPRO")
	m := newManager(t, e)
	j, err := m.Submit(t.Context(), Request{Path: card})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, j)
	if p := j.Progress().Phase; p != PhaseDone {
		t.Errorf("phase %q", p)
	}
}

func TestManagerExclusive(t *testing.T) {
	e := newEnv(t)
	m := newManager(t, e)
	ctx := t.Context()

	release1, err := m.Exclusive()
	if err != nil {
		t.Fatalf("Exclusive while idle: %v", err)
	}
	release2, err := m.Exclusive()
	if err != nil {
		t.Fatalf("second Exclusive (holds nest): %v", err)
	}
	j, err := m.Submit(ctx, Request{Path: e.fixtureCard(t, "SONY_A7IV")})
	if err != nil {
		t.Fatal(err)
	}
	// Negative check: a held job must not start.
	select {
	case <-j.Done():
		t.Fatal("the job finished while Exclusive was held")
	case <-time.After(200 * time.Millisecond):
	}
	if running, queued := m.Current(); running != nil || len(queued) != 1 {
		t.Fatalf("Current = %v, %d queued; want none running, 1 queued", running, len(queued))
	}
	if _, err := m.Exclusive(); !errors.Is(err, ErrBusy) {
		t.Fatalf("Exclusive with a queued job: err = %v, want ErrBusy", err)
	}
	release1()
	release1() // twice is harmless
	select {
	case <-j.Done():
		t.Fatal("the job finished while one hold was still held")
	case <-time.After(100 * time.Millisecond):
	}
	release2()
	waitDone(t, j)

	started, release := gate(e)
	t.Cleanup(release)
	j2, err := m.Submit(ctx, Request{Path: e.fixtureCard(t, "GOPRO")})
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, started)
	if _, err := m.Exclusive(); !errors.Is(err, ErrBusy) {
		t.Fatalf("Exclusive while running: err = %v, want ErrBusy", err)
	}
	release()
	waitDone(t, j2)
	release3, err := m.Exclusive()
	if err != nil {
		t.Fatalf("Exclusive when idle again: %v", err)
	}
	release3()
}

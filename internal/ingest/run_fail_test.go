package ingest

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

// runCtx is env.run with a caller-supplied context.
func (e *env) runCtx(t *testing.T, ctx context.Context, card string) catalog.Ingest {
	t.Helper()
	in, err := e.cat.CreateIngest(t.Context(), card, filepath.Base(card), "1234-ABCD", testNow)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.r.Run(ctx, in, NewProgress())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func wantNoPartials(t *testing.T, e *env) {
	t.Helper()
	for _, p := range e.staged(t) {
		if strings.HasSuffix(p, media.PartialSuffix) {
			t.Errorf("partial left behind: %s", p)
		}
	}
}

func TestRunFailedFileNotSafe(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	e.r.fingerprint = func(p string) (string, error) {
		if strings.HasSuffix(p, ".ARW") {
			return "", errors.New("read error")
		}
		return media.QuickFingerprint(p)
	}
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, New: 1, Failed: 1}, false)
	row := e.files(t, out.ID)[cardARW]
	if row.Outcome != catalog.OutcomeFailed || !strings.Contains(row.Error, "read error") {
		t.Errorf("ARW row = %+v", row)
	}
	wantStaged(t, e, sonyJPEG)
}

func TestRunZeroByteFileFails(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	const empty = "DCIM/100MSDCF/DSC00002.JPG"
	writeFile(t, card, empty, nil)
	var mu sync.Mutex
	var called []string
	e.r.fingerprint = func(p string) (string, error) {
		mu.Lock()
		called = append(called, p)
		mu.Unlock()
		return media.QuickFingerprint(p)
	}
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 3, New: 2, Failed: 1}, false)
	row := e.files(t, out.ID)[empty]
	if row.Outcome != catalog.OutcomeFailed || row.Error != "empty file" {
		t.Errorf("row = %+v", row)
	}
	for _, p := range called {
		if strings.HasSuffix(p, "DSC00002.JPG") {
			t.Errorf("fingerprint called for the empty file")
		}
	}
}

func TestRunCopyFailureLeavesNothing(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	e.r.copyFile = func(ctx context.Context, src, dst string) (string, error) {
		if strings.HasSuffix(src, ".ARW") {
			return "", errors.New("I/O error")
		}
		return media.CopyHashed(ctx, src, dst)
	}
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, New: 1, Failed: 1}, false)
	wantStaged(t, e, sonyJPEG)
	wantNoPartials(t, e)

	e.r.copyFile = media.CopyHashed
	out = e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, New: 1, InStaging: 1}, true)
	wantStaged(t, e, sonyJPEG, sonyARW)
}

func TestRunCardRemovedMidCopy(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	calls := 0
	defer func() {
		if calls != 2 {
			t.Errorf("copyFile calls = %d, want 2 (a missing source must not be retried)", calls)
		}
	}()
	e.r.copyFile = func(ctx context.Context, src, dst string) (string, error) {
		calls++
		if calls == 2 {
			if err := os.RemoveAll(card); err != nil {
				t.Error(err)
			}
		}
		return media.CopyHashed(ctx, src, dst)
	}
	out := e.run(t, card)
	if out.Status != catalog.StatusFailed || !strings.HasPrefix(out.Error, "card removed during import") {
		t.Fatalf("status %q error %q", out.Status, out.Error)
	}
	if out.Counts != (catalog.Counts{Seen: 2, New: 1, Failed: 1}) || out.SafeToFormat {
		t.Errorf("counts %+v safe %v", out.Counts, out.SafeToFormat)
	}
	if out.FinishedAt.IsZero() {
		t.Error("FinishedAt is zero")
	}
	wantStaged(t, e, sonyARW)
	wantNoPartials(t, e)
}

func TestRunCancelledMidCopy(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.r.copyFile = func(_ context.Context, src, dst string) (string, error) {
		cancel()
		return media.CopyHashed(ctx, src, dst)
	}
	out := e.runCtx(t, ctx, card)
	if out.Status != catalog.StatusCancelled || out.Counts.New != 0 || out.SafeToFormat {
		t.Errorf("status %q counts %+v safe %v", out.Status, out.Counts, out.SafeToFormat)
	}
	wantStaged(t, e)
	got, err := e.cat.Ingest(t.Context(), out.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != catalog.StatusCancelled || got.FinishedAt.IsZero() {
		t.Errorf("stored row %+v", got)
	}
}

func TestRunCancelledBeforeCopy(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	e.r.copyFile = func(context.Context, string, string) (string, error) {
		t.Error("copyFile called")
		return "", errors.New("unexpected")
	}
	out := e.runCtx(t, ctx, card)
	if out.Status != catalog.StatusCancelled {
		t.Errorf("status %q", out.Status)
	}
	wantStaged(t, e)
}

func TestRunRetriesTakenName(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	first := true
	e.r.copyFile = func(ctx context.Context, src, dst string) (string, error) {
		if first {
			first = false
			if err := os.WriteFile(dst, []byte("someone else"), 0o666); err != nil {
				t.Error(err)
			}
		}
		return media.CopyHashed(ctx, src, dst)
	}
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, New: 2}, true)
	const alt = "Staging/2025-05-30/Capture/2025-05-31 01.30.12-1.ARW"
	wantStaged(t, e, sonyJPEG, alt, sonyARW) // "-" sorts before "."
	b, err := os.ReadFile(filepath.Join(e.photos, filepath.FromSlash(sonyARW)))
	if err != nil || string(b) != "someone else" {
		t.Errorf("intruder = %q, %v", b, err)
	}
	if mustHash(t, filepath.Join(e.photos, filepath.FromSlash(alt))) != mustHash(t, cardPath(card, cardARW)) {
		t.Error("renamed ARW does not hold the card's bytes")
	}
}

func TestRunRetriesStalePartial(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	first := true
	e.r.copyFile = func(ctx context.Context, src, dst string) (string, error) {
		if first {
			first = false
			if err := os.WriteFile(dst+media.PartialSuffix, []byte("junk"), 0o666); err != nil {
				t.Error(err)
			}
		}
		return media.CopyHashed(ctx, src, dst)
	}
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, New: 2}, true)
	wantStaged(t, e, sonyJPEG, sonyARW)
}

func TestRunRetriesMissingDestDir(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	first := true
	e.r.copyFile = func(ctx context.Context, src, dst string) (string, error) {
		if first {
			first = false
			if err := os.RemoveAll(filepath.Dir(dst)); err != nil {
				t.Error(err)
			}
		}
		return media.CopyHashed(ctx, src, dst)
	}
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, New: 2}, true)
	wantStaged(t, e, sonyJPEG, sonyARW)
}

func TestRunGivesUpAfterThreeAttempts(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	arwCalls := 0
	e.r.copyFile = func(ctx context.Context, src, dst string) (string, error) {
		if strings.HasSuffix(src, ".ARW") {
			arwCalls++
			if err := os.WriteFile(dst, []byte("someone else"), 0o666); err != nil {
				t.Error(err)
			}
		}
		return media.CopyHashed(ctx, src, dst)
	}
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, New: 1, Failed: 1}, false)
	fs := e.files(t, out.ID)
	if r := fs[cardARW]; r.Outcome != catalog.OutcomeFailed || !strings.Contains(r.Error, "gave up after 3 attempts") {
		t.Errorf("ARW row = %+v", r)
	}
	if r := fs[cardJPG]; r.Outcome != catalog.OutcomeNew || r.RelPath != sonyJPEG {
		t.Errorf("JPG row = %+v", r)
	}
	if arwCalls != 3 {
		t.Errorf("ARW copy calls = %d, want 3", arwCalls)
	}
}

func TestRunCleansStalePartials(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	writeFile(t, e.photos, sonyJPEG+media.PartialSuffix, []byte("junk"))
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, New: 2}, true)
	wantStaged(t, e, sonyJPEG, sonyARW)
	if mustHash(t, filepath.Join(e.photos, filepath.FromSlash(sonyJPEG))) != mustHash(t, cardPath(card, cardJPG)) {
		t.Error("staged JPG differs from the card's")
	}
}

func TestRunAdoptsUnrecordedCopy(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	data, err := os.ReadFile(cardPath(card, cardJPG))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, e.photos, sonyJPEG, data)
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, New: 1, InStaging: 1}, true)
	fs := e.files(t, out.ID)
	if r := fs[cardJPG]; r.Outcome != catalog.OutcomeInStaging || r.RelPath != sonyJPEG {
		t.Errorf("JPG row = %+v", r)
	}
	if fs[cardARW].Outcome != catalog.OutcomeNew {
		t.Errorf("ARW row = %+v", fs[cardARW])
	}
	wantStaged(t, e, sonyJPEG, sonyARW)
	a, err := e.cat.AssetBySHA(t.Context(), mustHash(t, cardPath(card, cardJPG)))
	if err != nil {
		t.Fatal(err)
	}
	if a.State != catalog.StateStaged || a.RelPath != sonyJPEG || a.FirstIngest != out.ID {
		t.Errorf("asset = %+v", a)
	}
}

func TestRunLogsCancelledCopyError(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	e.r.cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	card := e.fixtureCard(t, "SONY_A7IV")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.r.copyFile = func(context.Context, string, string) (string, error) {
		cancel()
		return "", errors.Join(context.Canceled, errors.New("remove partial: boom"))
	}
	out := e.runCtx(t, ctx, card)
	if out.Status != catalog.StatusCancelled {
		t.Fatalf("status %q", out.Status)
	}
	if log := buf.String(); !strings.Contains(log, "copy cancelled") || !strings.Contains(log, "remove partial: boom") {
		t.Errorf("log does not show the cancelled copy's error: %s", log)
	}
}

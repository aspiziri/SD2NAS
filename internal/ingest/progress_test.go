package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/media"
	"github.com/aspiziri/SD2NAS/internal/staging"
)

func TestProgress(t *testing.T) {
	if got := NewProgress().Snapshot().Phase; got != PhaseQueued {
		t.Fatalf("new progress phase = %q, want %q", got, PhaseQueued)
	}
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	in, err := e.cat.CreateIngest(t.Context(), card, filepath.Base(card), "1234-ABCD", testNow)
	if err != nil {
		t.Fatal(err)
	}
	p := NewProgress()
	out, err := e.r.Run(t.Context(), in, p)
	if err != nil {
		t.Fatal(err)
	}
	s := p.Snapshot()
	if s.Phase != PhaseDone || s.FilesDone != 2 || s.FilesTotal != 2 {
		t.Errorf("snapshot = %+v, want phase done, 2 of 2 files", s)
	}
	if s.BytesDone != s.BytesTotal || s.BytesTotal == 0 {
		t.Errorf("bytes %d of %d, want equal and non-zero", s.BytesDone, s.BytesTotal)
	}
	if s.Counts != out.Counts {
		t.Errorf("snapshot counts %+v, row counts %+v", s.Counts, out.Counts)
	}
}

// twoVideos writes a 10-byte and a 20-byte video on a fresh card; the scan
// visits a.MP4 before b.MP4.
func twoVideos(t *testing.T, e *env) (card, second string) {
	t.Helper()
	card = filepath.Join(e.cards, "CARD")
	writeFile(t, card, "DCIM/a.MP4", []byte("0123456789"))
	second = writeFile(t, card, "DCIM/b.MP4", []byte("0123456789abcdefghij"))
	return card, second
}

func TestProgressBytesDuringCopy(t *testing.T) {
	e := newEnv(t)
	card, second := twoVideos(t, e)
	reported := make(chan struct{})
	release := make(chan struct{})
	e.r.copyFile = func(ctx context.Context, src, dst string, progress func(int64)) (string, error) {
		if src == second {
			progress(5)
			close(reported)
			<-release
		}
		return media.CopyHashedProgress(ctx, src, dst, progress)
	}
	p := NewProgress()
	done := runAsync(t.Context(), e, queued(t, e, card), p)
	defer func() { // never leave the run blocked if an assertion fails
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	select {
	case <-reported:
	case <-time.After(10 * time.Second):
		t.Fatal("the second copy never started")
	}
	s := p.Snapshot()
	if s.Current != "DCIM/b.MP4" || s.CurrentSize != 20 || s.CurrentDone != 5 || s.BytesDone != 15 || s.FilesDone != 1 {
		t.Errorf("snapshot during the copy = %+v, want current DCIM/b.MP4 20 bytes, 5 done, 15 bytes and 1 file done", s)
	}
	close(release)
	if out := waitRun(t, done); out.Counts.New != 2 {
		t.Fatalf("counts %+v, want 2 new", out.Counts)
	}
	s = p.Snapshot()
	if s.BytesDone != 30 || s.Current != "" || s.CurrentSize != 0 || s.CurrentDone != 0 {
		t.Errorf("snapshot after the copy = %+v, want 30 bytes done and no current file", s)
	}
}

func TestProgressBytesAfterFailure(t *testing.T) {
	e := newEnv(t)
	card, second := twoVideos(t, e)
	e.r.copyFile = func(ctx context.Context, src, dst string, progress func(int64)) (string, error) {
		if src == second {
			progress(7)
			return "", errors.New("I/O error")
		}
		return media.CopyHashedProgress(ctx, src, dst, progress)
	}
	p := NewProgress()
	out, err := e.r.Run(t.Context(), queued(t, e, card), p)
	if err != nil {
		t.Fatal(err)
	}
	if out.Counts.Failed != 1 {
		t.Fatalf("counts %+v, want 1 failed", out.Counts)
	}
	s := p.Snapshot()
	if s.BytesDone != 30 || s.FilesDone != 2 || s.Current != "" || s.CurrentSize != 0 || s.CurrentDone != 0 {
		t.Errorf("snapshot = %+v, want 30 bytes, 2 files and no current file", s)
	}
}

func TestProgressBytesRetry(t *testing.T) {
	e := newEnv(t)
	card, second := twoVideos(t, e)
	p := NewProgress()
	attempts := 0
	e.r.copyFile = func(ctx context.Context, src, dst string, progress func(int64)) (string, error) {
		if src != second {
			return media.CopyHashedProgress(ctx, src, dst, progress)
		}
		attempts++
		if attempts == 1 {
			progress(18)
			if s := p.Snapshot(); s.CurrentDone != 18 || s.BytesDone != 28 {
				t.Errorf("first attempt at 18: %+v, want CurrentDone 18 and BytesDone 28", s)
			}
			writeFile(t, filepath.Dir(dst), filepath.Base(dst), []byte("someone else's"))
			return "", fmt.Errorf("%s: %w", dst, fs.ErrExist)
		}
		progress(3)
		if s := p.Snapshot(); s.CurrentDone != 3 || s.BytesDone != 13 || s.CurrentDone > s.CurrentSize {
			t.Errorf("retry at 3: %+v, want CurrentDone 3 and BytesDone 13", s)
		}
		return media.CopyHashedProgress(ctx, src, dst, progress)
	}
	out, err := e.r.Run(t.Context(), queued(t, e, card), p)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || out.Counts.New != 2 {
		t.Errorf("attempts %d, counts %+v, want 2 attempts and 2 new", attempts, out.Counts)
	}
	if s := p.Snapshot(); s.BytesDone != 30 {
		t.Errorf("BytesDone = %d, want 30", s.BytesDone)
	}
}

func TestProgressWaitingForFolder(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	f, err := e.cat.StagingFolderForDate(t.Context(), "2025-05-30", testNow)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := e.r.locks.TryLock(staging.HolderPromote, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	p := NewProgress()
	done := runAsync(t.Context(), e, queued(t, e, card), p)
	deadline := time.Now().Add(10 * time.Second)
	for p.Snapshot().WaitingFolder == "" {
		if time.Now().After(deadline) {
			t.Fatalf("the import never reported waiting: %+v", p.Snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s := p.Snapshot(); s.WaitingFolder != f.Name || s.WaitingHolder != "promote" {
		t.Errorf("waiting for %q held by %q, want %q held by promote", s.WaitingFolder, s.WaitingHolder, f.Name)
	}
	unlock()
	if out := waitRun(t, done); out.Counts.New != 2 {
		t.Fatalf("counts %+v, want 2 new", out.Counts)
	}
	if s := p.Snapshot(); s.WaitingFolder != "" || s.WaitingHolder != "" {
		t.Errorf("still waiting after the release: %+v", s)
	}
}

func TestProgressSnapshotJSON(t *testing.T) {
	raw, err := json.Marshal(ProgressSnapshot{Current: "DCIM/a.MP4", CurrentSize: 20, CurrentDone: 5, WaitingFolder: "2025-05-30", WaitingHolder: "promote"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"current": "DCIM/a.MP4", "current_size": 20.0, "current_done": 5.0, "waiting_folder": "2025-05-30", "waiting_holder": "promote"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	raw, err = json.Marshal(ProgressSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"current", "current_size", "current_done", "waiting_folder", "waiting_holder"} {
		if strings.Contains(string(raw), `"`+k+`"`) {
			t.Errorf("a zero snapshot has key %s: %s", k, raw)
		}
	}
}

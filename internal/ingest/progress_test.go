package ingest

import (
	"path/filepath"
	"testing"
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

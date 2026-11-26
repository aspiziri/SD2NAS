package ingest

import (
	"sync"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

// Phases of a running ingest.
const (
	PhaseQueued      = "queued"
	PhaseScanning    = "scanning"
	PhaseClassifying = "classifying"
	PhaseCopying     = "copying"
	PhaseFinishing   = "finishing"
	PhaseDone        = "done"
)

// ProgressSnapshot is a copy of a running ingest's live state.
type ProgressSnapshot struct {
	Phase      string         `json:"phase"`
	FilesTotal int            `json:"files_total"`
	FilesDone  int            `json:"files_done"`
	BytesTotal int64          `json:"bytes_total"`
	BytesDone  int64          `json:"bytes_done"`
	Counts     catalog.Counts `json:"counts"`

	// The file being copied (card path, its size, bytes copied so far) while
	// the copy runs.
	Current     string `json:"current,omitempty"`
	CurrentSize int64  `json:"current_size,omitempty"`
	CurrentDone int64  `json:"current_done,omitempty"`
	// The staging folder the import waits for, and what holds it (a
	// staging.Holder), while it waits.
	WaitingFolder string `json:"waiting_folder,omitempty"`
	WaitingHolder string `json:"waiting_holder,omitempty"`
}

// Progress is a running ingest's live state, safe for concurrent use.
type Progress struct {
	mu sync.Mutex
	s  ProgressSnapshot
}

// NewProgress returns a Progress in the queued phase.
func NewProgress() *Progress {
	return &Progress{s: ProgressSnapshot{Phase: PhaseQueued}}
}

// Snapshot returns the current state.
func (p *Progress) Snapshot() ProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.s
}

func (p *Progress) update(fn func(*ProgressSnapshot)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(&p.s)
}

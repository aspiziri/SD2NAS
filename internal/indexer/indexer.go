// Package indexer keeps the catalog in line with Archive/ (§9): the first
// pass is the backfill, later ones the nightly rescan; it also backs up the catalog.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

const (
	ArchiveDir = "Archive" // must equal staging.ArchiveDir and catalog.ArchiveRoot without its "/" (internal/api test)
	StagingDir = "Staging" // must equal staging.Dir
)

// maxErrors is how many problems a ScanResult lists; the rest are counted.
const maxErrors = 20

// Locks reports archive directories a promote is writing into; the shared
// *staging.Locks satisfies it (Decision 12).
type Locks interface{ NameHeld(rel string) bool }

type Config struct {
	DB         *catalog.DB
	PhotosRoot string
	Locks      Locks            // nil: nothing is ever held
	Busy       func() bool      // an import is running or queued, or a promote runs: wait between files; nil: never
	Now        func() time.Time // nil: time.Now
	Log        *slog.Logger     // nil: slog.Default()
}

const (
	TriggerManual  = "manual"
	TriggerNightly = "nightly"
	PhaseListing   = "listing"
	PhaseHashing   = "hashing"
	PhaseChecking  = "checking"
)

// ScanResult is what one pass did; it is stored as JSON in archive_scans
// and sent as Status.Last.
type ScanResult struct {
	Trigger       string    `json:"trigger"`
	Full          bool      `json:"full,omitempty"` // a re-index (Decision 22)
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	Listed        int       `json:"listed"`       // media files found
	Unchanged     int       `json:"unchanged"`    // not read (size and modified time matched), or read with the same content
	Added         int       `json:"added"`        // new content indexed
	Moved         int       `json:"moved"`        // known content at a new path
	Changed       int       `json:"changed"`      // same path, new content ("changed since indexed")
	Reappeared    int       `json:"reappeared"`   // missing content found again
	Recovered     int       `json:"recovered"`    // culled content found in the archive, now archived
	Missing       int       `json:"missing"`      // newly marked missing
	Copies        int       `json:"copies"`       // new duplicate paths of archived content
	InStaging     int       `json:"in_staging"`   // content still staged (a promote in flight): left alone
	BusyFolders   int       `json:"busy_folders"` // skipped: a promote held them
	Errors        []string  `json:"errors"`       // at most 20, never null
	ErrorsOmitted int       `json:"errors_omitted,omitempty"`
	Skipped       string    `json:"skipped,omitempty"` // why nothing (more) was done
	Stopped       bool      `json:"stopped,omitempty"` // interrupted: the next pass carries on

	log *slog.Logger
}

// errorf records a problem: every one is logged, the first maxErrors are
// listed in the result and the rest are only counted (ErrorsOmitted).
func (r *ScanResult) errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if r.log != nil {
		r.log.Warn("archive scan problem", "err", msg)
	}
	if len(r.Errors) < maxErrors {
		r.Errors = append(r.Errors, msg)
		return
	}
	r.ErrorsOmitted++
}

// finalError records why the pass stopped. It is listed even past the cap.
func (r *ScanResult) finalError(msg string) {
	if r.log != nil {
		r.log.Warn("archive scan problem", "err", msg)
	}
	r.Errors = append(r.Errors, msg)
}

// Service is the archive indexer.
type Service struct {
	db         *catalog.DB
	photosRoot string
	locks      Locks
	busy       func() bool
	now        func() time.Time
	log        *slog.Logger
	rules      media.Rules

	// Seams for tests; a blocking hook must also select on its ctx.
	readDir       func(string) ([]os.DirEntry, error)
	hash          func(context.Context, string) (string, error)
	fingerprint   func(string) (string, error)
	beforeRead    func(ctx context.Context, rel string)
	afterHash     func(path string)
	beforeMissing func(rel string)

	bgCtx    context.Context
	bgCancel context.CancelFunc
	wg       sync.WaitGroup

	pass sync.Mutex // held by the running pass

	mu     sync.Mutex // guards closed
	closed bool
}

// New builds the service. It starts nothing yet.
func New(cfg Config) (*Service, error) {
	if cfg.DB == nil {
		return nil, errors.New("indexer: no catalog")
	}
	if cfg.PhotosRoot == "" {
		return nil, errors.New("indexer: no photos root")
	}
	s := &Service{
		db: cfg.DB, photosRoot: cfg.PhotosRoot, locks: cfg.Locks, busy: cfg.Busy,
		now: cfg.Now, log: cfg.Log, rules: media.DefaultRules(),
		readDir:     os.ReadDir,
		fingerprint: media.QuickFingerprint,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.hash = func(ctx context.Context, path string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return media.HashFile(path)
	}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	return s, nil
}

// Close stops a pass between files (and mid-file) and the nightly loop, and
// waits for them. It is safe to call more than once.
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.bgCancel()
	s.wg.Wait()
}

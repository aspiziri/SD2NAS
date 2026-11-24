// Package indexer keeps the catalog in line with Archive/ (§9): the first
// pass is the backfill, later ones the nightly rescan; it also backs up the catalog.
package indexer

import (
	"context"
	"encoding/json"
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

// Status is the indexer.progress event data (Decision 19). Task 11 adds
// LastBackup, BackingUp and NextScan.
type Status struct {
	Version     uint64      `json:"version"`      // bumps after each pass, backup, accept and settings change
	Running     *Progress   `json:"running"`      // nil when no pass runs
	Last        *ScanResult `json:"last"`         // the last finished pass; nil before the first
	PendingFull bool        `json:"pending_full"` // a re-index didn't finish; the next pass continues it
	Changed     int         `json:"changed"`      // "changed since indexed" entries
	Missing     int         `json:"missing"`      // missing assets
}

// Progress is a running pass.
type Progress struct {
	Trigger     string    `json:"trigger"`
	Full        bool      `json:"full"`  // a re-index
	Phase       string    `json:"phase"` // "" until the pass starts listing, then listing | hashing | checking
	StartedAt   time.Time `json:"started_at"`
	Listed      int       `json:"listed"`
	ToHash      int       `json:"to_hash"` // files to read (changed + new); 0 while listing
	Hashed      int       `json:"hashed"`
	BytesToHash int64     `json:"bytes_to_hash"`
	BytesHashed int64     `json:"bytes_hashed"`
	Folder      string    `json:"folder"`    // the session folder being listed or read
	Current     string    `json:"current"`   // the file being read; "" while listing or checking
	PausedBy    string    `json:"paused_by"` // "" | "user" (Pause) | "busy" (an import or promote runs)
}

// publishEvery is the shortest gap between unforced progress frames (<= 4/s).
const publishEvery = 250 * time.Millisecond

const msgInterrupted = "SD2NAS stopped before this scan finished"

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

	// mu guards closed and the status. notify runs under it and only
	// publishes: never call a Service method from it.
	mu         sync.Mutex
	closed     bool
	status     Status
	notify     func(Status)
	lastPub    time.Time // real clock
	userPaused bool      // the owner paused the running pass; reset when a pass ends

	wake      chan struct{} // Resume wakes a waiting pass (buffer 1)
	idleEvery time.Duration // how often a waiting pass looks at busy again
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
		wake:        make(chan struct{}, 1),
		idleEvery:   2 * time.Second,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.hash = hashFile
	if err := s.loadState(context.Background()); err != nil {
		return nil, err
	}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	return s, nil
}

// loadState closes passes left unfinished by a stop or crash (the history
// prune relies on every row being finished), then loads the last pass and
// the counts.
func (s *Service) loadState(ctx context.Context) error {
	open, err := s.db.UnfinishedArchiveScans(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	for _, o := range open {
		r := ScanResult{Trigger: o.Kind, Full: o.Reread, StartedAt: o.StartedAt, FinishedAt: now,
			Stopped: true, Errors: []string{msgInterrupted}}
		b, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("indexer: encode interrupted scan %d: %w", o.ID, err)
		}
		if err := s.db.FinishArchiveScan(ctx, o.ID, now, false, string(b)); err != nil {
			return err
		}
	}
	last, err := s.db.LastArchiveScan(ctx)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
	case err != nil:
		return err
	default:
		var r ScanResult
		if err := json.Unmarshal([]byte(last.Result), &r); err != nil {
			s.log.Error("last archive scan is unreadable", "scan", last.ID, "err", err)
		} else {
			if r.Errors == nil {
				r.Errors = []string{}
			}
			s.status.Last = &r
		}
	}
	if err := s.refreshPendingFull(ctx); err != nil {
		return err
	}
	return s.refreshCounts(ctx)
}

// refreshPendingFull reloads PendingFull. Call it without s.mu.
func (s *Service) refreshPendingFull(ctx context.Context) error {
	t, err := s.db.PendingReread(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.status.PendingFull = !t.IsZero()
	s.mu.Unlock()
	return nil
}

// refreshCounts reloads Changed and Missing. Call it without s.mu.
func (s *Service) refreshCounts(ctx context.Context) error {
	c, err := s.db.ArchiveCounts(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.status.Changed, s.status.Missing = c.Changed, c.Missing
	s.mu.Unlock()
	return nil
}

// Status returns the current status.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *Service) statusLocked() Status {
	st := s.status
	if st.Running != nil {
		p := *st.Running
		st.Running = &p
	}
	if st.Last != nil {
		l := *st.Last
		st.Last = &l
	}
	return st
}

// OnChange sets the function that receives every published Status. Call it
// before serving; fn runs under s.mu and must only publish.
func (s *Service) OnChange(fn func(Status)) {
	s.mu.Lock()
	s.notify = fn
	s.mu.Unlock()
}

// reportLocked publishes the status: always when force, otherwise at most
// every publishEvery. s.mu is held.
func (s *Service) reportLocked(force bool) {
	if s.notify == nil {
		return
	}
	if !force && time.Since(s.lastPub) < publishEvery {
		return
	}
	s.lastPub = time.Now()
	s.notify(s.statusLocked())
}

// progress applies fn to the running pass (nothing when none runs) and reports.
func (s *Service) progress(force bool, fn func(*Progress)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Running == nil {
		return
	}
	fn(s.status.Running)
	s.reportLocked(force)
}

// StartRescan starts a manual pass in the background. It is false when a
// pass is running or the service is closed. full asks for a re-index (Decision 22).
func (s *Service) StartRescan(full bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.pass.TryLock() {
		return false
	}
	s.wg.Add(1)
	s.status.Running = &Progress{Trigger: TriggerManual, Full: full, StartedAt: s.now()}
	s.reportLocked(true)
	go func() {
		defer s.wg.Done()
		defer s.pass.Unlock()
		if _, err := s.runPass(s.bgCtx, TriggerManual, full); err != nil {
			s.log.Error("archive scan failed", "err", err)
		}
	}()
	return true
}

// ErrClosed is what Rescan returns once the service has been closed.
var ErrClosed = errors.New("indexer: shutting down")

// Rescan runs one pass now and waits for it (tests); it waits for a pass
// that is already running, stops when ctx or Close does, and is ErrClosed
// after Close. err is non-nil only for catalog failures; cancellation sets
// Stopped. full asks for a re-index (Decision 22).
func (s *Service) Rescan(ctx context.Context, trigger string, full bool) (ScanResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ScanResult{}, ErrClosed
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()

	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.bgCtx, cancel)
	defer cancel()
	defer stop()

	s.pass.Lock()
	defer s.pass.Unlock()
	return s.runPass(ctx, trigger, full)
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

// Package notices queues the notices SD2NAS raises on its own for the
// Unraid host: a failed nightly catalog backup, files the archive scan
// marked missing, and the daily stale-staging reminder. It polls the
// indexer status and the staging listing from its own goroutine, so it
// adds no callback to either service (the HANDOFF lock-order rules).
package notices

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/indexer"
	"github.com/aspiziri/SD2NAS/internal/staging"
)

const (
	CheckEvery = 30 * time.Second
	MaxAge     = 24 * time.Hour // older archive events are not notified (Decision 7)
)

// ErrClosed is what Pending and Take return once the service has been closed.
var ErrClosed = errors.New("notices: shutting down")

// maxListed is how many folder names the stale-staging text spells out.
const maxListed = 5

type Config struct {
	DB           *catalog.DB
	Archive      func() indexer.Status                          // nil: no archive notices
	Staging      func(context.Context) (staging.Listing, error) // nil: no stale-staging notice
	StagingReady func() bool                                    // the startup staging reconcile has finished; nil: always ready
	Now          func() time.Time                               // nil: time.Now
	Log          *slog.Logger                                   // nil: slog.Default()
	Every        time.Duration                                  // 0 (or less): CheckEvery
}

// Service checks for notices every Config.Every once started.
type Service struct {
	cfg   Config
	log   *slog.Logger
	now   func() time.Time
	every time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex // guards closed and started, and orders wg.Add before Close's Wait
	closed  bool
	started bool
	wg      sync.WaitGroup

	// Touched only by the loop goroutine (and by tests calling check).
	known        map[string]time.Time // keys stored or found, with their event time
	stagingDay   string               // the local day whose stale-staging check is done
	stagingRetry time.Time            // no stale-staging check before this
	adds         int                  // add calls, for tests

	nudge  chan struct{} // cap 1; SettingsChanged sends without blocking
	reread atomic.Bool   // set by SettingsChanged (any goroutine)
	at     time.Duration // staging_notice_at; -1 until read

	missingSeen  int       // Status.Missing at the last missing check; -1 before the first
	lastFinished time.Time // Status.Last.FinishedAt at that check
	watermark    time.Time // rows marked missing at or before it are notified or too old

	// test-only counters, like adds
	settingsReads  int // DB.Settings calls made by noticeAt
	missingQueries int // MissingSince calls made by checkMissing
}

// New returns a stopped service. Its only error is a nil DB.
func New(cfg Config) (*Service, error) {
	if cfg.DB == nil {
		return nil, errors.New("notices: no catalog")
	}
	s := &Service{cfg: cfg, log: cfg.Log, now: cfg.Now, every: cfg.Every, known: map[string]time.Time{},
		at: -1, missingSeen: -1, nudge: make(chan struct{}, 1)}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.every <= 0 {
		s.every = CheckEvery
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

// Start checks now and then every Every (and at once after
// SettingsChanged). It does nothing after Close or a second call.
func (s *Service) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.started {
		return
	}
	s.started = true
	s.wg.Add(1)
	go s.loop()
}

func (s *Service) loop() {
	defer s.wg.Done()
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		s.check(s.ctx)
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		case <-s.nudge:
		}
	}
}

// SettingsChanged makes the loop re-read staging_notice_at and check now.
// It never blocks (PUT /api/settings calls it).
func (s *Service) SettingsChanged() {
	s.reread.Store(true)
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// noticeAt returns staging_notice_at, read once and again after
// SettingsChanged or a failed read. A failed read logs and keeps the last
// value (the default before any read succeeded).
func (s *Service) noticeAt(ctx context.Context) time.Duration {
	if s.at >= 0 && !s.reread.Swap(false) {
		return s.at
	}
	s.settingsReads++
	set, err := s.cfg.DB.Settings(ctx)
	if err != nil {
		s.reread.Store(true) // try again at the next check
		if s.at < 0 {
			s.at = catalog.DefaultSettings().StagingNoticeAt
		}
		if ctx.Err() == nil { // not shutting down
			s.log.Error("could not read staging_notice_at; using the last value", "at", hhmm(s.at), "err", err)
		}
		return s.at
	}
	s.at = set.StagingNoticeAt
	return s.at
}

// hhmm formats a time since midnight as HH:MM, as Settings.Strings does.
func hhmm(d time.Duration) string {
	return fmt.Sprintf("%02d:%02d", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// Close stops the loop and waits for it and for any Pending or Take. It is
// safe to call more than once.
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
}

// enter registers a caller that Close waits for; false after Close.
func (s *Service) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.wg.Add(1)
	return true
}

// Pending lists the notices not taken yet (catalog.PendingNotifications).
// It is ErrClosed after Close.
func (s *Service) Pending(ctx context.Context) ([]catalog.Notification, error) {
	if !s.enter() {
		return nil, ErrClosed
	}
	defer s.wg.Done()
	return s.cfg.DB.PendingNotifications(ctx)
}

// Take marks the pending notices taken now and returns them
// (catalog.TakeNotifications). It is ErrClosed after Close.
func (s *Service) Take(ctx context.Context) ([]catalog.Notification, error) {
	if !s.enter() {
		return nil, ErrClosed
	}
	defer s.wg.Done()
	return s.cfg.DB.TakeNotifications(ctx, s.now())
}

func (s *Service) check(ctx context.Context) {
	s.checkArchive(ctx)
	s.checkStaging(ctx)
}

func (s *Service) checkArchive(ctx context.Context) {
	if s.cfg.Archive == nil {
		return
	}
	st, now := s.cfg.Archive(), s.now()
	for key, at := range s.known {
		if !fresh(now, at) { // no longer notified, so no longer looked up
			delete(s.known, key)
		}
	}
	if b := st.LastBackup; b != nil && b.Error != "" && fresh(now, b.At) {
		s.addOnce(ctx, "backup:"+b.At.UTC().Format(time.RFC3339Nano), b.At, catalog.NoticeAlert,
			"Nightly catalog backup failed: "+strings.TrimRight(b.Error, ". ")+". Older backups were kept.")
	}
	s.checkMissing(ctx, st, now)
}

// checkMissing notices the files marked missing since the watermark: the
// newest row of the last missing notice (so a restart repeats nothing), or
// MaxAge ago if that is later. It counts rows, not Status.Last.Missing, so
// two passes before one check, or a pass that crashed, are counted too. It
// queries only when the indexer's missing count or last pass changed.
func (s *Service) checkMissing(ctx context.Context, st indexer.Status, now time.Time) {
	if st.Running != nil {
		return // a pass may still be marking rows
	}
	var finished time.Time
	if st.Last != nil {
		finished = st.Last.FinishedAt
	}
	if !s.watermark.IsZero() && st.Missing == s.missingSeen && finished.Equal(s.lastFinished) {
		return
	}
	if s.watermark.IsZero() {
		w, err := s.cfg.DB.LatestNotificationTime(ctx, "missing:")
		if err != nil {
			if ctx.Err() == nil { // not shutting down
				s.log.Error("could not read the last missing-files notice", "err", err)
			}
			return
		}
		s.watermark = w
		if floor := now.Add(-MaxAge); w.Before(floor) {
			s.watermark = floor
		}
	}
	s.missingQueries++
	n, latest, err := s.cfg.DB.MissingSince(ctx, s.watermark)
	if err != nil {
		if ctx.Err() == nil { // not shutting down
			s.log.Error("could not count files marked missing", "err", err)
		}
		return
	}
	if n > 0 {
		if err := s.add(ctx, "missing:"+latest.UTC().Format(time.RFC3339Nano), catalog.NoticeAlert, missingText(n)); err != nil {
			return // add logged it; the next check retries
		}
		s.watermark = latest
	}
	s.missingSeen, s.lastFinished = st.Missing, finished
}

// addOnce adds a notice unless this service already stored or found its
// key, so an event that stays in the status doesn't cost a write
// transaction every 30 s (PF13). add logs a failure, and the next check
// retries it. at is the event's time, after which the key is dropped
// from memory once it is no longer fresh.
func (s *Service) addOnce(ctx context.Context, key string, at time.Time, level, msg string) {
	if _, ok := s.known[key]; ok {
		return
	}
	if err := s.add(ctx, key, level, msg); err == nil {
		s.known[key] = at
	}
}

// fresh: t is set and at most MaxAge before now.
func fresh(now, t time.Time) bool { return !t.IsZero() && now.Sub(t) <= MaxAge }

// add stores a notice created now. A key already stored is not an error.
func (s *Service) add(ctx context.Context, key, level, msg string) error {
	s.adds++
	added, err := s.cfg.DB.AddNotification(ctx, catalog.Notification{Key: key, Level: level, Message: msg, CreatedAt: s.now()})
	if err != nil {
		s.log.Error("could not queue a notice", "key", key, "err", err)
		return err
	}
	if added {
		s.log.Info("notice queued", "key", key, "level", level)
	}
	return nil
}

func (s *Service) checkStaging(ctx context.Context) {
	if s.cfg.Staging == nil {
		return
	}
	now := s.now() // time.Now is in time.Local, the container's TZ
	day := now.Format("2006-01-02")
	if day == s.stagingDay {
		return
	}
	at := s.noticeAt(ctx)
	y, m, d := now.Date()
	due := time.Date(y, m, d, int(at/time.Hour), int(at%time.Hour/time.Minute), 0, 0, now.Location())
	if now.Before(due) || now.Before(s.stagingRetry) {
		return
	}
	if s.cfg.StagingReady != nil && !s.cfg.StagingReady() {
		return // the startup reconcile may still adopt folders (PF21)
	}
	key := "staging:" + day
	has, err := s.cfg.DB.HasNotification(ctx, key)
	if err != nil {
		s.retryStaging(now, err)
		return
	}
	if has {
		s.stagingDay = day
		return
	}
	l, err := s.cfg.Staging(ctx)
	if err != nil {
		s.retryStaging(now, err)
		return
	}
	var stale []staging.Folder
	for _, f := range l.Folders {
		if f.Stale {
			stale = append(stale, f)
		}
	}
	if l.StaleAfterDays > 0 && len(stale) > 0 {
		if err := s.add(ctx, key, catalog.NoticeWarning, staleText(l.StaleAfterDays, stale)); err != nil {
			s.retryStaging(now, err)
			return
		}
	}
	s.stagingDay = day
}

// retryStaging logs err and tries again in an hour.
func (s *Service) retryStaging(now time.Time, err error) {
	s.log.Error("stale-staging check failed; retrying in an hour", "err", err)
	s.stagingRetry = now.Add(time.Hour)
}

func missingText(n int) string {
	files := "files"
	if n == 1 {
		files = "file"
	}
	return fmt.Sprintf("The archive scan marked %d %s missing from Archive/. See the Archive index page.", n, files)
}

// staleText words the stale-staging notice for folders (at least one),
// oldest first (AgeDays descending, then name). days is staging_warn_days.
func staleText(days int, folders []staging.Folder) string {
	fs := slices.Clone(folders)
	slices.SortFunc(fs, func(a, b staging.Folder) int {
		if c := cmp.Compare(b.AgeDays, a.AgeDays); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	names := make([]string, len(fs))
	for i, f := range fs {
		names[i] = f.Name
	}
	var list string
	switch n := len(names); {
	case n > maxListed:
		list = fmt.Sprintf("%s and %d more", strings.Join(names[:maxListed], ", "), n-maxListed)
	case n == 1:
		list = names[0]
	default:
		list = strings.Join(names[:n-1], ", ") + " and " + names[n-1]
	}
	subject := "1 folder has"
	if len(names) != 1 {
		subject = fmt.Sprintf("%d folders have", len(names))
	}
	unit := "days"
	if days == 1 {
		unit = "day"
	}
	return fmt.Sprintf("%s been in Staging for %d %s or more: %s.", subject, days, unit, list)
}

package indexer

import (
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

// nextRun is the first time of day at (since midnight) strictly after now,
// in now's location (time.Local: the container's TZ). time.Date normalises
// a time that falls in a DST gap.
func nextRun(now time.Time, at time.Duration) time.Time {
	h, m := int(at/time.Hour), int(at%time.Hour/time.Minute)
	y, mo, d := now.Date()
	t := time.Date(y, mo, d, h, m, 0, 0, now.Location())
	if !t.After(now) {
		t = time.Date(y, mo, d+1, h, m, 0, 0, now.Location())
	}
	return t
}

// Start starts the nightly loop. It does nothing after Close or a second call.
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

// SettingsChanged makes the nightly loop read the schedule again now. It
// does not block.
func (s *Service) SettingsChanged() {
	select {
	case s.reschedule <- struct{}{}:
	default: // one is already pending
	}
	s.mu.Lock()
	s.status.Version++
	s.reportLocked(true)
	s.mu.Unlock()
}

func (s *Service) setNextScan(next time.Time) {
	s.mu.Lock()
	s.status.NextScan = next
	s.status.Version++
	s.reportLocked(true)
	s.mu.Unlock()
}

func (s *Service) loop() {
	defer s.wg.Done()
	for {
		at := catalog.DefaultSettings().ArchiveRescanAt
		if set, err := s.db.Settings(s.bgCtx); err != nil {
			if s.bgCtx.Err() != nil {
				return
			}
			s.log.Error("read the rescan time; using the default", "err", err)
		} else {
			at = set.ArchiveRescanAt
		}
		next := nextRun(s.now(), at)
		s.setNextScan(next)
		select {
		case <-s.bgCtx.Done():
			return
		case <-s.reschedule:
			continue
		case <-s.after(next.Sub(s.now())):
		}
		if s.now().Before(next) { // the wall clock was set back during the wait: wait for next again
			continue
		}
		s.nightly()
	}
}

// nightly backs up the catalog first, whatever a pass is doing (Decision 16),
// then starts the nightly pass in the background (so a paused or long pass
// never keeps the loop from re-arming) unless one is already running, paused
// or not. A pending re-index is continued by runPass.
func (s *Service) nightly() {
	s.backupNow(s.bgCtx)
	if s.bgCtx.Err() != nil {
		return
	}
	if !s.startPass(TriggerNightly, false) {
		s.log.Info("nightly rescan skipped: a scan is already running")
	}
}

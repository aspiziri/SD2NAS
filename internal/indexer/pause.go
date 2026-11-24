package indexer

import (
	"errors"
	"time"
)

// ErrNotRunning is what Pause and Resume return when no pass is running.
var ErrNotRunning = errors.New("indexer: no scan is running")

// Pause holds the running pass before its next file (in memory: a restart
// forgets it). A file being read is finished first. Pausing a paused pass
// changes nothing.
func (s *Service) Pause() (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Status{}, ErrClosed
	}
	if s.status.Running == nil {
		return Status{}, ErrNotRunning
	}
	if !s.userPaused {
		s.userPaused = true
		s.status.Running.PausedBy = "user"
		s.reportLocked(true)
	}
	return s.statusLocked(), nil
}

// Resume lets a pass paused with Pause go on. Resuming a pass that is not
// paused changes nothing.
func (s *Service) Resume() (Status, error) {
	busy := s.busy != nil && s.busy() // without s.mu: the callback takes other services' locks
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Status{}, ErrClosed
	}
	if s.status.Running == nil {
		return Status{}, ErrNotRunning
	}
	if s.userPaused {
		s.userPaused = false
		by := ""
		if busy {
			by = "busy" // the pass would show it a moment later anyway
		}
		s.status.Running.PausedBy = by
		s.reportLocked(true)
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return s.statusLocked(), nil
}

func (s *Service) isUserPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userPaused
}

// setPausedBy shows why the pass waits ("" when it doesn't); a change is
// published at once.
func (s *Service) setPausedBy(by string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Running == nil || s.status.Running.PausedBy == by {
		return
	}
	s.status.Running.PausedBy = by
	s.reportLocked(true)
}

// wait returns when the pass may go on: not paused by the owner, and no
// import or promote busy (Decisions 13 and 21). busy is called without s.mu:
// main's Busy takes the ingest manager's and the staging service's locks.
func (sc *scan) wait() error {
	for {
		by := ""
		if sc.s.isUserPaused() {
			by = "user"
		} else if sc.s.busy != nil && sc.s.busy() {
			by = "busy"
		}
		sc.s.setPausedBy(by)
		if by == "" || sc.ctx.Err() != nil {
			return sc.ctx.Err()
		}
		select {
		case <-sc.ctx.Done():
			return sc.ctx.Err()
		case <-sc.s.wake: // Resume
		case <-time.After(sc.s.idleEvery):
		}
	}
}

package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

// Stats is the archive index page: the Status and the numbers behind it.
type Stats struct {
	Status
	Files          int                    `json:"files"`
	Bytes          int64                  `json:"bytes"`
	Copies         int                    `json:"copies"`
	Imported       int                    `json:"imported"`
	MissingFiles   []catalog.MissingFile  `json:"missing_files"` // <= 50, never null
	Recent         []catalog.RecentFolder `json:"recent"`        // <= 5, never null
	RescanAt       string                 `json:"rescan_at"`     // "03:00"
	BackupDir      string                 `json:"backup_dir"`    // effective
	SkipRules      []string               `json:"skip_rules"`    // effective archive_skip entries, never null
	ArchivePath    string                 `json:"archive_path"`  // OS path of Archive/
	ArchivePresent bool                   `json:"archive_present"`
}

// ChangeList is the "changed since indexed" list.
type ChangeList struct {
	Version  uint64                  `json:"version"`
	Changes  []catalog.ArchiveChange `json:"changes"` // <= 500, never null
	Total    int                     `json:"total"`
	LatestID int64                   `json:"latest_id"`
}

const (
	maxMissingShown = 50
	maxRecentShown  = 5
	maxChangesShown = 500
)

// Stats returns the numbers for the archive index page. It works after Close.
func (s *Service) Stats(ctx context.Context) (Stats, error) {
	st := Stats{Status: s.Status()} // first: its version is the one the page is told
	c, err := s.db.ArchiveCounts(ctx)
	if err != nil {
		return Stats{}, err
	}
	st.Files, st.Bytes, st.Copies, st.Imported = c.Files, c.Bytes, c.Copies, c.Imported
	st.Changed, st.Missing = c.Changed, c.Missing
	if st.MissingFiles, err = s.db.MissingFiles(ctx, maxMissingShown); err != nil {
		return Stats{}, err
	}
	if st.Recent, err = s.db.RecentArchiveFolders(ctx, maxRecentShown); err != nil {
		return Stats{}, err
	}
	if st.MissingFiles == nil {
		st.MissingFiles = []catalog.MissingFile{}
	}
	if st.Recent == nil {
		st.Recent = []catalog.RecentFolder{}
	}
	set, err := s.db.Settings(ctx)
	if err != nil {
		return Stats{}, err
	}
	st.RescanAt = set.Strings()[catalog.KeyArchiveRescanAt]
	if st.SkipRules, err = catalog.ParseArchiveSkip(set.ArchiveSkip); err != nil {
		return Stats{}, err
	}
	if st.SkipRules == nil {
		st.SkipRules = []string{}
	}
	st.BackupDir = s.BackupDir(set)
	st.ArchivePath = filepath.Join(s.photosRoot, ArchiveDir)
	_, statErr := os.Stat(st.ArchivePath)
	st.ArchivePresent = statErr == nil
	return st, nil
}

// Changes returns the "changed since indexed" list (at most 500, oldest
// first). It works after Close.
func (s *Service) Changes(ctx context.Context) (ChangeList, error) {
	l := ChangeList{Version: s.Status().Version} // first, as in Stats
	var err error
	if l.Changes, l.Total, l.LatestID, err = s.db.ArchiveChanges(ctx, maxChangesShown); err != nil {
		return ChangeList{}, err
	}
	if l.Changes == nil {
		l.Changes = []catalog.ArchiveChange{}
	}
	return l, nil
}

// Accept clears the changes with id <= upTo and returns how many. It is
// ErrClosed after Close.
func (s *Service) Accept(ctx context.Context, upTo int64) (int, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrClosed
	}
	s.wg.Add(1) // Close waits, so the catalog is not closed under the write
	s.mu.Unlock()
	defer s.wg.Done()

	if upTo < 1 {
		return 0, fmt.Errorf("indexer: accept up to %d: want a positive id", upTo)
	}
	n, err := s.db.AcceptArchiveChanges(ctx, upTo)
	if err != nil {
		return 0, err
	}
	err = s.refreshCounts(ctx)
	s.mu.Lock()
	s.status.Version++
	s.reportLocked(true)
	s.mu.Unlock()
	if err != nil {
		return n, fmt.Errorf("indexer: count changes after accept: %w", err)
	}
	return n, nil
}

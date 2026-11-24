package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

// BackupKeep is how many catalog backups the folder keeps.
const BackupKeep = 14

// BackupResult is what one nightly backup did (Status.LastBackup).
type BackupResult struct {
	Path    string    `json:"path"` // the file written or attempted
	At      time.Time `json:"at"`
	Bytes   int64     `json:"bytes"`
	Removed []string  `json:"removed"` // old backups deleted (file names), never null
	Error   string    `json:"error,omitempty"`
}

// ErrBackupInside is CheckBackupDir's refusal.
var ErrBackupInside = errors.New("pick a folder outside Archive/ and Staging/")

var backupName = regexp.MustCompile(`^catalog-[0-9]{4}-[0-9]{2}-[0-9]{2}\.db$`)

// BackupDir is the effective backup folder: the setting, or <photos>/Management/SD2NAS.
func (s *Service) BackupDir(set catalog.Settings) string {
	if set.BackupDir != "" {
		return set.BackupDir
	}
	return filepath.Join(s.photosRoot, "Management", "SD2NAS")
}

// CheckBackupDir returns ErrBackupInside when the absolute dir is Archive/ or
// Staging/ under photosRoot, or anything below them. A relative dir gives
// nil: the catalog rejects it.
func CheckBackupDir(photosRoot, dir string) error {
	if !filepath.IsAbs(dir) {
		return nil
	}
	rel, err := filepath.Rel(filepath.Clean(photosRoot), filepath.Clean(dir))
	if err != nil || !filepath.IsLocal(rel) {
		return nil // not under the photos root
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	for _, name := range []string{ArchiveDir, StagingDir} {
		if strings.EqualFold(first, name) { // the share may be case-insensitive
			return ErrBackupInside
		}
	}
	return nil
}

// backupNow runs one backup, records it and publishes the status.
func (s *Service) backupNow(ctx context.Context) {
	s.mu.Lock()
	s.status.BackingUp = true
	s.reportLocked(true)
	s.mu.Unlock()

	res := s.backup(ctx)

	dbctx := context.WithoutCancel(ctx)
	if b, err := json.Marshal(res); err != nil {
		s.log.Error("encode the catalog backup result", "err", err)
	} else if err := s.db.RecordArchiveBackup(dbctx, res.At, res.Error == "", string(b)); err != nil {
		s.log.Error("record the catalog backup", "err", err)
	}

	s.mu.Lock()
	s.status.LastBackup = res
	s.status.BackingUp = false
	s.status.Version++
	s.reportLocked(true)
	s.mu.Unlock()
}

func (s *Service) backup(ctx context.Context) *BackupResult {
	at := s.now()
	res := &BackupResult{At: at, Removed: []string{}}
	set, err := s.db.Settings(ctx)
	if err != nil {
		res.Error = err.Error()
		s.log.Error("catalog backup failed", "err", err)
		return res
	}
	dir := s.BackupDir(set)
	res.Path = filepath.Join(dir, "catalog-"+at.Format("2006-01-02")+".db")
	if err := s.writeBackup(ctx, dir, res.Path); err != nil {
		res.Error = err.Error()
		s.log.Error("catalog backup failed", "path", res.Path, "err", err)
		return res // nothing is pruned after a failure
	}
	if info, err := os.Stat(res.Path); err != nil {
		s.log.Warn("catalog backup written but its size is unknown", "path", res.Path, "err", err)
	} else {
		res.Bytes = info.Size()
	}
	removed, err := pruneBackups(dir, BackupKeep)
	res.Removed = append(res.Removed, removed...)
	if err != nil {
		res.Error = "backup written, but old backups could not all be removed: " + err.Error()
		s.log.Error("prune catalog backups", "dir", dir, "err", err)
	}
	s.log.Info("catalog backup", "path", res.Path, "bytes", res.Bytes, "removed", len(removed))
	return res
}

func (s *Service) writeBackup(ctx context.Context, dir, path string) error {
	if err := CheckBackupDir(s.photosRoot, dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	tmp := path + ".partial"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := s.db.Backup(ctx, tmp); err != nil {
		return errors.Join(err, removeIfPresent(tmp))
	}
	if err := syncFile(tmp); err != nil {
		return errors.Join(err, removeIfPresent(tmp))
	}
	if err := os.Rename(tmp, path); err != nil { // replaces a same-day backup
		return errors.Join(err, removeIfPresent(tmp))
	}
	return media.SyncDir(dir)
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// syncFile fsyncs path through a read-write handle: Windows' FlushFileBuffers
// refuses a read-only one.
func syncFile(path string) (err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return f.Sync()
}

// pruneBackups keeps the newest keep catalog-YYYY-MM-DD.db files in dir
// (the date sorts as text) and deletes the rest; nothing else is touched.
func pruneBackups(dir string, keep int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && backupName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	var removed []string
	var errs []error
	for _, n := range names[:max(0, len(names)-keep)] {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, n)
	}
	return removed, errors.Join(errs...)
}

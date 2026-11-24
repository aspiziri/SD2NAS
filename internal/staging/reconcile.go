package staging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

// maxRescanErrors bounds RescanResult.Errors.
const maxRescanErrors = 20

// errorf records a problem: every one is logged, the first maxRescanErrors
// are listed in the result and the rest are only counted (ErrorsOmitted).
func (r *RescanResult) errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if r.log != nil {
		r.log.Warn("staging rescan problem", "err", msg)
	}
	if len(r.Errors) < maxRescanErrors {
		r.Errors = append(r.Errors, msg)
		return
	}
	r.ErrorsOmitted++
}

// finalError records why the pass stopped. It is listed even past the cap.
func (r *RescanResult) finalError(msg string) {
	if r.log != nil {
		r.log.Warn("staging rescan problem", "err", msg)
	}
	r.Errors = append(r.Errors, msg)
}

// errStagingVanished: Staging/ disappeared mid-pass (the share dropped).
var errStagingVanished = errors.New("staging: Staging/ vanished during the rescan")

// stagingPresent returns errStagingVanished when Staging/ is gone, so a
// dropped share is never read as "everything was deleted".
func (s *Service) stagingPresent() error {
	if _, err := os.Stat(s.abs(Dir)); errors.Is(err, fs.ErrNotExist) {
		return errStagingVanished
	} else if err != nil {
		return err
	}
	return nil
}

// metadataNames are NAS and OS housekeeping directories that never hold the
// user's photos. Same idea as ingest's skipDir (internal/ingest/scan.go,
// which also skips every dot-name); staging can't import ingest.
var metadataNames = []string{"@eaDir", "@Recycle", "@Recently-Snapshot", "#recycle", "#snapshot",
	"$RECYCLE.BIN", "System Volume Information"}

// skipName reports a file or directory name the rescan never looks into.
func skipName(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	for _, m := range metadataNames {
		if strings.EqualFold(name, m) {
			return true
		}
	}
	return false
}

// skipRel reports a slash path inside a folder with any skipped element.
func skipRel(rel string) bool {
	for _, el := range strings.Split(rel, "/") {
		if skipName(el) {
			return true
		}
	}
	return false
}

func (r *RescanResult) changedAnything() bool {
	return r.AdoptedFolders+r.AddedFiles+r.MovedFiles+r.RestoredFiles+r.CulledFiles+r.RemovedFolders > 0
}

// Reconcile brings the catalog in line with Staging/ on disk (§8, Decision
// 8): it adopts directories without a row, records media files the catalog
// doesn't know (or follows them when they were moved by hand), culls
// tracked files that are gone, and drops rows whose directory is gone.
// Busy folders are skipped. When Staging/ itself is missing it changes
// nothing: an unmounted share must not cull everything.
func (s *Service) Reconcile(ctx context.Context) (RescanResult, error) {
	r := RescanResult{StartedAt: s.now(), Errors: []string{}}
	r.log = s.log
	err := s.reconcile(ctx, &r)
	if errors.Is(err, errStagingVanished) {
		r.Skipped = Dir + "/ went missing during the rescan, so it was stopped (is the photos share still mounted?)"
		err = nil
	}
	r.FinishedAt = s.now()
	if r.changedAnything() {
		s.changed()
	}
	return r, err
}

func (s *Service) reconcile(ctx context.Context, r *RescanResult) error {
	rows, err := s.db.StagingFolders(ctx)
	if err != nil {
		return err
	}
	if _, err := os.Stat(s.abs(Dir)); errors.Is(err, fs.ErrNotExist) {
		if len(rows) > 0 {
			r.Skipped = Dir + "/ is missing, so nothing was changed (is the photos share mounted?)"
		}
		return nil
	} else if err != nil {
		return err
	}
	onDisk, err := s.diskFolders()
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, row := range rows {
		known[row.Name] = true
	}
	busyNames := 0
	for _, name := range slices.Sorted(maps.Keys(onDisk)) {
		if known[name] || skipName(name) {
			continue
		}
		// An operation renaming or creating this directory holds its name
		// until the catalog knows it (pre-flight D2): leave it for next time.
		release, err := s.locks.TryLockSet(HolderRescan, LockSet{Names: []string{Dir + "/" + name}})
		if errors.Is(err, ErrBusy) {
			busyNames++
			continue
		}
		if err != nil {
			return err
		}
		var dates []string
		if d, _ := SplitName(name); d != "" {
			dates = []string{d}
		}
		row, err := s.db.CreateStagingFolder(ctx, name, dates, s.now())
		release()
		if errors.Is(err, catalog.ErrNameTaken) {
			continue // an import or an operation recorded it meanwhile
		}
		if err != nil {
			return err
		}
		r.AdoptedFolders++
		rows = append(rows, row)
	}
	settings, err := s.db.Settings(ctx)
	if err != nil {
		return err
	}
	busy := map[int64]bool{}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.reconcileAdditions(ctx, row, settings.DayStartsAt, r, busy); err != nil {
			return err
		}
	}
	for _, row := range rows {
		if err := s.reconcileRemovals(ctx, row, r, busy); err != nil {
			return err
		}
	}
	if err := s.reconcileOrphans(ctx, r); err != nil {
		return err
	}
	// A folder being renamed is busy twice (its row and its new name); take
	// the larger of the two counts so it is one.
	r.BusyFolders = max(len(busy), busyNames)
	return nil
}

// lockForRescan takes id's lock, or records it as busy and returns nil.
func (s *Service) lockForRescan(id int64, busy map[int64]bool) (func(), error) {
	release, err := s.locks.TryLock(HolderRescan, id)
	if errors.Is(err, ErrBusy) {
		busy[id] = true
		return nil, nil
	}
	return release, err
}

func (s *Service) reconcileAdditions(ctx context.Context, row catalog.StagingFolder, cutoff time.Duration, r *RescanResult, busy map[int64]bool) error {
	release, err := s.lockForRescan(row.ID, busy)
	if err != nil || release == nil {
		return err
	}
	defer release()
	row, err = s.db.StagingFolderByID(ctx, row.ID)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := safeName(row.Name); err != nil {
		r.errorf("%v", err)
		return nil
	}
	prefix := prefixOf(row.Name)
	files, err := folderFiles(s.folderDir(row.Name))
	if err != nil {
		r.errorf("%v", err)
		return nil
	}
	assets, err := s.db.AssetsUnder(ctx, prefix, catalog.StateStaged)
	if err != nil {
		return err
	}
	tracked := map[string]bool{}
	for _, a := range assets {
		tracked[strings.TrimPrefix(a.RelPath, prefix)] = true
	}
	var dates []string
	for _, rel := range files {
		if tracked[rel] {
			continue
		}
		if skipRel(rel) {
			continue // NAS metadata and dot-files: not photos
		}
		kind, ok := s.rules.KindOf(rel)
		if !ok {
			continue // not a photo or video: left alone
		}
		date, err := s.adoptFile(ctx, row, prefix+rel, kind, cutoff, r, busy)
		if err != nil {
			return err
		}
		if date != "" {
			dates = append(dates, date)
		}
	}
	if len(dates) == 0 {
		return nil
	}
	return s.db.SetStagingFolderInfo(ctx, row.ID, unionDates([]catalog.StagingFolder{row}, dates...), time.Time{})
}

// adoptFile records the untracked media file at relPath and returns the
// folder date it adds ("" for none). Problems with the file itself go into
// r; only catalog failures and cancellation are returned.
func (s *Service) adoptFile(ctx context.Context, row catalog.StagingFolder, relPath string, kind media.Kind,
	cutoff time.Duration, r *RescanResult, busy map[int64]bool) (string, error) {
	p := s.abs(relPath)
	before, err := os.Stat(p)
	if err != nil {
		r.errorf("%s: %v", relPath, err)
		return "", nil
	}
	sum, err := hashFile(ctx, p)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		r.errorf("%s: %v", relPath, err)
		return "", nil
	}
	if s.afterHash != nil {
		s.afterHash(p)
	}
	fp, err := media.QuickFingerprint(p)
	if err != nil {
		r.errorf("%s: %v", relPath, err)
		return "", nil
	}
	after, err := os.Stat(p)
	if err != nil {
		r.errorf("%s: %v", relPath, err)
		return "", nil
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		r.errorf("%s changed while it was read (still being copied?); the next rescan adds it", relPath)
		return "", nil
	}
	existing, err := s.db.AssetBySHA(ctx, sum)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		c, cerr := media.CaptureTime(p, kind)
		if cerr != nil {
			s.log.Warn("rescanned file has no capture time", "path", relPath, "err", cerr)
		}
		a := catalog.Asset{SHA256: sum, Size: after.Size(), Kind: kind, CaptureTime: c.Time, TZOffset: c.Offset,
			State: catalog.StateStaged, RelPath: relPath, Mtime: after.ModTime().UnixNano(), UpdatedAt: s.now()}
		if err := s.db.InsertAsset(ctx, a, fp); err != nil {
			return "", err
		}
		r.AddedFiles++
		if c.Time.IsZero() {
			return "", nil
		}
		return media.FolderDate(c.Time, cutoff), nil
	case err != nil:
		return "", err
	case existing.SHA256 != sum: // an alias: an archived file's content before an edit
		r.Duplicates++
		return "", nil
	}
	date := ""
	if !existing.CaptureTime.IsZero() {
		date = media.FolderDate(existing.CaptureTime, cutoff)
	}
	switch existing.State {
	case catalog.StateStaged:
		if _, err := os.Lstat(s.abs(existing.RelPath)); !errors.Is(err, fs.ErrNotExist) {
			r.Duplicates++ // the tracked copy is still there (or can't be checked)
			return "", nil
		}
		release, err := s.lockOwner(ctx, existing.RelPath, row.ID, busy)
		if err != nil || release == nil {
			return "", err // nil, nil: its folder is busy; the next rescan follows it
		}
		defer release()
		if err := s.db.MoveAsset(ctx, sum, relPath, s.now()); err != nil {
			return "", err
		}
		r.MovedFiles++
	case catalog.StateCulled, catalog.StateMissing:
		if err := s.db.RestageAsset(ctx, sum, relPath, after.ModTime().UnixNano(), s.now()); err != nil {
			return "", err
		}
		if err := s.db.AddFingerprint(ctx, fp, sum); err != nil {
			return "", err
		}
		r.RestoredFiles++
	default: // archived: a copy of an archive file, left alone
		r.Duplicates++
		return "", nil
	}
	return date, nil
}

// lockOwner takes the lock of the staging folder holding relPath unless
// that is self or has no row (then it returns a no-op release). It returns
// nil, nil when that folder is busy.
func (s *Service) lockOwner(ctx context.Context, relPath string, self int64, busy map[int64]bool) (func(), error) {
	name, _, ok := strings.Cut(strings.TrimPrefix(relPath, Dir+"/"), "/")
	if !strings.HasPrefix(relPath, Dir+"/") || !ok {
		return func() {}, nil
	}
	all, err := s.db.StagingFolders(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		if r.Name == name && r.ID != self {
			return s.lockForRescan(r.ID, busy)
		}
	}
	return func() {}, nil
}

func (s *Service) reconcileRemovals(ctx context.Context, row catalog.StagingFolder, r *RescanResult, busy map[int64]bool) error {
	release, err := s.lockForRescan(row.ID, busy)
	if err != nil || release == nil {
		return err
	}
	defer release()
	row, err = s.db.StagingFolderByID(ctx, row.ID)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := safeName(row.Name); err != nil {
		r.errorf("%v", err) // the additions pass reported it too; skipped either way
		return nil
	}
	assets, err := s.db.AssetsUnder(ctx, prefixOf(row.Name), catalog.StateStaged)
	if err != nil {
		return err
	}
	gone := map[string]catalog.Asset{}
	for _, a := range assets {
		switch _, err := os.Lstat(s.abs(a.RelPath)); {
		case errors.Is(err, fs.ErrNotExist):
			gone[a.RelPath] = a
		case err != nil:
			r.errorf("%s: %v", a.RelPath, err)
		}
	}
	if len(gone) > 0 {
		if err := s.stagingPresent(); err != nil {
			return err
		}
	}
	if err := s.cullGone(ctx, gone); err != nil {
		return err
	}
	r.CulledFiles += len(gone)
	if _, err := os.Lstat(s.folderDir(row.Name)); errors.Is(err, fs.ErrNotExist) && len(gone) == len(assets) {
		if err := s.stagingPresent(); err != nil {
			return err
		}
		if err := s.db.DeleteStagingFolder(ctx, row.ID); err != nil {
			return err
		}
		r.RemovedFolders++
	}
	return nil
}

// reconcileOrphans culls staged files below Staging/ that no folder row
// owns and that are gone from disk.
func (s *Service) reconcileOrphans(ctx context.Context, r *RescanResult) error {
	rows, err := s.db.StagingFolders(ctx)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, row := range rows {
		names[row.Name] = true
	}
	assets, err := s.db.AssetsUnder(ctx, Dir+"/", catalog.StateStaged)
	if err != nil {
		return err
	}
	gone := map[string]catalog.Asset{}
	for _, a := range assets {
		if name, _, ok := strings.Cut(strings.TrimPrefix(a.RelPath, Dir+"/"), "/"); ok && names[name] {
			continue
		}
		if _, err := os.Lstat(s.abs(a.RelPath)); errors.Is(err, fs.ErrNotExist) {
			gone[a.RelPath] = a
		}
	}
	if len(gone) > 0 {
		if err := s.stagingPresent(); err != nil {
			return err
		}
	}
	if err := s.cullGone(ctx, gone); err != nil {
		return err
	}
	r.CulledFiles += len(gone)
	return nil
}

// StartRescan runs Reconcile in the background unless one is running or
// the service is closing, and reports whether it started one. Status
// tracks it (Rescanning, then LastRescan).
func (s *Service) StartRescan() bool {
	s.mu.Lock()
	if s.closed || s.status.Rescanning {
		s.mu.Unlock()
		return false
	}
	s.status.Rescanning = true
	s.notifyLocked()
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		r, err := s.Reconcile(s.bgCtx)
		switch {
		case errors.Is(err, context.Canceled):
			s.log.Info("staging rescan stopped: shutting down")
			r.finalError("stopped: SD2NAS is shutting down")
		case err != nil:
			s.log.Error("staging rescan failed", "err", err)
			r.finalError(err.Error())
		default:
			s.log.Info("staging rescan", "adopted_folders", r.AdoptedFolders, "added", r.AddedFiles, "moved", r.MovedFiles,
				"restored", r.RestoredFiles, "culled", r.CulledFiles, "removed_folders", r.RemovedFolders,
				"duplicates", r.Duplicates, "busy", r.BusyFolders, "problems", len(r.Errors), "skipped", r.Skipped)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.status.Rescanning = false
		s.status.LastRescan = &r
		s.notifyLocked()
	}()
	return true
}

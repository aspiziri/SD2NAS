package staging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

// prefixOf is the rel_path prefix of the files in folder name.
func prefixOf(name string) string { return Dir + "/" + name + "/" }

// rows reads folders ids, refusing names that aren't one path element.
func (s *Service) rows(ctx context.Context, ids []int64) ([]catalog.StagingFolder, error) {
	out := make([]catalog.StagingFolder, 0, len(ids))
	for _, id := range ids {
		row, err := s.db.StagingFolderByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := safeName(row.Name); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// busyNamed fills in a *BusyError's display name: the folder's name from
// rows, or for a directory-name key the name without "Staging/". (Task 7
// adds whether a promote holding it is still queued.) Other errors pass.
func (s *Service) busyNamed(err error, rows []catalog.StagingFolder) error {
	var busy *BusyError
	if !errors.As(err, &busy) {
		return err
	}
	if busy.ID == 0 {
		busy.Name = strings.TrimPrefix(busy.Name, Dir+"/")
	}
	for _, r := range rows {
		if busy.ID != 0 && r.ID == busy.ID {
			busy.Name = r.Name
		}
	}
	return err
}

// lockRows takes the locks of rows for one operation, or returns the
// *BusyError of the first busy one with its name filled in.
func (s *Service) lockRows(h Holder, rows ...catalog.StagingFolder) (func(), error) {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	release, err := s.locks.TryLock(h, ids...)
	return release, s.busyNamed(err, rows)
}

// lockName takes the lock of directory name Staging/<name>, which an
// operation is about to create or rename into, so the rescan can't adopt it
// before the catalog knows it (Decision 1, pre-flight D2). Take it after
// the folders' locks; like them it never waits.
func (s *Service) lockName(name string) (func(), error) {
	release, err := s.locks.TryLockSet(HolderOperation, LockSet{Names: []string{Dir + "/" + name}})
	return release, s.busyNamed(err, nil)
}

func (s *Service) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// lockFolders reads, locks and reads again folders ids (an operation that
// finished just before the lock may have renamed one). The caller releases.
// Once the service is closing every operation gets ErrClosed (503).
func (s *Service) lockFolders(ctx context.Context, ids []int64) ([]catalog.StagingFolder, func(), error) {
	if s.isClosed() {
		return nil, nil, ErrClosed
	}
	rows, err := s.rows(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	release, err := s.lockRows(HolderOperation, rows...)
	if err != nil {
		return nil, nil, err
	}
	rows, err = s.rows(ctx, ids)
	if err != nil {
		release()
		return nil, nil, err
	}
	return rows, release, nil
}

// summary is folder id as the UI shows it.
func (s *Service) summary(ctx context.Context, id int64) (Folder, error) {
	ff, err := s.Files(ctx, id, "")
	if err != nil {
		return Folder{}, err
	}
	return ff.Folder, nil
}

// nameFree returns catalog.ErrNameTaken when a folder row other than self,
// or a directory in Staging/ that isn't one of self's, already has name. On
// a case-insensitive disk a case-only rename finds the folder itself, which
// doesn't count.
func (s *Service) nameFree(ctx context.Context, name string, self ...catalog.StagingFolder) error {
	mine := map[int64]bool{}
	for _, r := range self {
		mine[r.ID] = true
	}
	all, err := s.db.StagingFolders(ctx)
	if err != nil {
		return err
	}
	for _, r := range all {
		if r.Name == name && !mine[r.ID] {
			return fmt.Errorf("%w: %q", catalog.ErrNameTaken, name)
		}
	}
	info, err := os.Lstat(s.folderDir(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, r := range self {
		if own, err := os.Lstat(s.folderDir(r.Name)); err == nil && os.SameFile(info, own) {
			return nil
		}
	}
	return fmt.Errorf("%w: %q (a folder by that name is already in %s/)", catalog.ErrNameTaken, name, Dir)
}

// renameFolder renames row's directory, when it has one, then its row and
// its assets' paths. The caller holds row's lock and checked nameFree.
func (s *Service) renameFolder(ctx context.Context, row catalog.StagingFolder, name string, touched map[string]bool) error {
	from, to := s.folderDir(row.Name), s.folderDir(name)
	switch _, err := os.Lstat(from); {
	case err == nil:
		if err := s.rename(from, to); err != nil {
			return fmt.Errorf("rename %s/%s: %w", Dir, row.Name, err)
		}
		touched[filepath.Join(s.photosRoot, Dir)] = true
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := s.db.RenameStagingFolder(ctx, row.ID, name, prefixOf(row.Name), prefixOf(name), s.now()); err != nil {
		return fmt.Errorf("renamed %s/%s to %q but could not record it (a rescan repairs this): %w", Dir, row.Name, name, err)
	}
	return nil
}

// Rename gives folder id the title, keeping its date prefix (§8):
// "2025-05-30" with "Trip" becomes "2025-05-30 - Trip"; an empty title
// leaves the date. A name without a date takes the folder's earliest date.
func (s *Service) Rename(ctx context.Context, id int64, title string) (Folder, error) {
	t, err := CleanTitle(title)
	if err != nil {
		return Folder{}, err
	}
	rows, release, err := s.lockFolders(ctx, []int64{id})
	if err != nil {
		return Folder{}, err
	}
	defer release()
	row := rows[0]
	name := FolderName(folderDate(row), t)
	if name == "" {
		return Folder{}, fmt.Errorf("%w: the folder has no date, so it needs a title", ErrBadTitle)
	}
	if err := checkName(name); err != nil {
		return Folder{}, fmt.Errorf("%w: %q %v", ErrBadTitle, name, err)
	}
	if name != row.Name {
		releaseName, err := s.lockName(name)
		if err != nil {
			return Folder{}, err
		}
		defer releaseName()
		if err := s.nameFree(ctx, name, row); err != nil {
			return Folder{}, err
		}
		defer s.changed()
		touched := map[string]bool{}
		if err := s.renameFolder(ctx, row, name, touched); err != nil {
			return Folder{}, err
		}
		if err := syncDirs(touched); err != nil {
			return Folder{}, err
		}
	}
	return s.summary(ctx, id)
}

// earliestDate is the earliest folderDate among rows, "" when none has one.
func earliestDate(rows []catalog.StagingFolder) string {
	best := ""
	for _, r := range rows {
		if d := folderDate(r); d != "" && (best == "" || d < best) {
			best = d
		}
	}
	return best
}

// unionDates is every date the rows cover, plus extra.
func unionDates(rows []catalog.StagingFolder, extra ...string) []string {
	out := slices.Clone(extra)
	for _, r := range rows {
		out = append(out, r.Dates...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// oldestCreated is the earliest created_at among rows.
func oldestCreated(rows []catalog.StagingFolder) time.Time {
	var t time.Time
	for _, r := range rows {
		if t.IsZero() || r.CreatedAt.Before(t) {
			t = r.CreatedAt
		}
	}
	return t
}

// mergeTarget is the folder a merge into name keeps: the one already named
// name, else the one with the earliest date (lowest id on a tie; folders
// with no date last).
func mergeTarget(rows []catalog.StagingFolder, name string) catalog.StagingFolder {
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	best := rows[0]
	for _, r := range rows[1:] {
		bd, rd := folderDate(best), folderDate(r)
		switch {
		case bd == "" && rd != "", rd != "" && rd < bd, rd == bd && r.ID < best.ID:
			best = r
		}
	}
	return best
}

// Merge moves every file of folders ids into one folder named for the
// earliest date among them and title, then removes the emptied sources
// (§8). Files keep their path inside the folder; clashes get -1, -2.
func (s *Service) Merge(ctx context.Context, ids []int64, title string) (Folder, error) {
	ids = uniqueIDs(ids)
	if len(ids) < 2 {
		return Folder{}, fmt.Errorf("%w: merging needs at least two folders", ErrBadRequest)
	}
	t, err := CleanTitle(title)
	if err != nil {
		return Folder{}, err
	}
	rows, release, err := s.lockFolders(ctx, ids)
	if err != nil {
		return Folder{}, err
	}
	defer release()
	name := FolderName(earliestDate(rows), t)
	if name == "" {
		return Folder{}, fmt.Errorf("%w: these folders have no date, so the merged folder needs a title", ErrBadTitle)
	}
	if err := checkName(name); err != nil {
		return Folder{}, fmt.Errorf("%w: %q %v", ErrBadTitle, name, err)
	}
	releaseName, err := s.lockName(name)
	if err != nil {
		return Folder{}, err
	}
	defer releaseName()
	if err := s.nameFree(ctx, name, rows...); err != nil {
		return Folder{}, err
	}
	target := mergeTarget(rows, name)
	defer s.changed()
	touched := map[string]bool{}
	for _, src := range rows {
		if src.ID == target.ID {
			continue
		}
		removed, err := s.mergeInto(ctx, src, target, touched)
		if err != nil {
			return Folder{}, fmt.Errorf("merge %q into %q: %w", src.Name, target.Name, err)
		}
		if !removed {
			// Kept only for files that can't be moved; it covers no files now,
			// so its dates move to the target (pre-flight D11).
			if err := s.db.SetStagingFolderInfo(ctx, src.ID, nil, time.Time{}); err != nil {
				return Folder{}, err
			}
		}
	}
	if err := s.db.SetStagingFolderInfo(ctx, target.ID, unionDates(rows), oldestCreated(rows)); err != nil {
		return Folder{}, err
	}
	if target.Name != name {
		if err := s.renameFolder(ctx, target, name, touched); err != nil {
			return Folder{}, err
		}
	}
	if err := syncDirs(touched); err != nil {
		return Folder{}, err
	}
	return s.summary(ctx, target.ID)
}

// moveTree moves every file below src, catalogued or not, to the same
// path below dst (moveFile: collision loop, EXDEV fallback) and calls
// record with the new rel_path of each catalogued one. tracked maps paths
// inside src to their assets; the entries moved are deleted from it, so
// what is left was not on disk. Merge and promote share it (pre-flight D10).
func (s *Service) moveTree(ctx context.Context, src, dst string, tracked map[string]catalog.Asset, touched map[string]bool,
	record func(a catalog.Asset, newRel string) error) error {
	files, err := folderFiles(src)
	if err != nil {
		return err
	}
	for _, rel := range files {
		a, isTracked := tracked[rel]
		from := filepath.Join(src, filepath.FromSlash(rel))
		to, err := s.moveFile(ctx, from, filepath.Join(dst, filepath.FromSlash(path.Dir(rel))), path.Base(rel), a.SHA256)
		// moveFile returns the new path along with an error when the copy was
		// verified but the original could not be removed: the file is at to now,
		// so record that before returning the error.
		moveErr := err
		if moveErr != nil && to == "" {
			return moveErr
		}
		touched[filepath.Dir(from)], touched[filepath.Dir(to)] = true, true
		if isTracked {
			delete(tracked, rel)
			newRel, err := s.relOf(to)
			if err == nil {
				err = record(a, newRel)
			}
			if err != nil {
				return errors.Join(moveErr, fmt.Errorf("moved %s but could not record it (a rescan repairs this): %w", rel, err))
			}
		}
		if moveErr != nil {
			return moveErr
		}
	}
	return nil
}

// trackedIn maps the paths inside folder name to its staged assets.
func (s *Service) trackedIn(ctx context.Context, name string) (map[string]catalog.Asset, error) {
	prefix := prefixOf(name)
	assets, err := s.db.AssetsUnder(ctx, prefix, catalog.StateStaged)
	if err != nil {
		return nil, err
	}
	out := map[string]catalog.Asset{}
	for _, a := range assets {
		out[strings.TrimPrefix(a.RelPath, prefix)] = a
	}
	return out, nil
}

// mergeInto moves every file of src into dst (moveTree), culls catalogued
// files that were already gone (as a rescan would), and removes src when
// nothing is left. It reports whether src was removed.
func (s *Service) mergeInto(ctx context.Context, src, dst catalog.StagingFolder, touched map[string]bool) (bool, error) {
	tracked, err := s.trackedIn(ctx, src.Name)
	if err != nil {
		return false, err
	}
	err = s.moveTree(ctx, s.folderDir(src.Name), s.folderDir(dst.Name), tracked, touched, func(a catalog.Asset, newRel string) error {
		return s.db.MoveAsset(ctx, a.SHA256, newRel, s.now())
	})
	if err != nil {
		return false, err
	}
	if err := s.cullGone(ctx, tracked); err != nil {
		return false, err
	}
	gone, kept, err := removeEmptyTree(s.folderDir(src.Name))
	if err != nil {
		return false, err
	}
	if !gone {
		// Only special files (symlinks, devices) are never moved.
		s.log.Warn("merged folder kept: it holds files that were not moved", "folder", src.Name, "kept", kept)
		return false, nil
	}
	touched[filepath.Join(s.photosRoot, Dir)] = true
	return true, s.db.DeleteStagingFolder(ctx, src.ID)
}

// cullGone culls catalogued files whose file is no longer on disk.
func (s *Service) cullGone(ctx context.Context, gone map[string]catalog.Asset) error {
	var shas []string
	for _, a := range gone {
		s.log.Warn("culling a staged file that is no longer on disk", "path", a.RelPath)
		if err := s.db.CullAsset(ctx, a.SHA256, s.now()); err != nil {
			s.removed(shas)
			return err
		}
		shas = append(shas, a.SHA256)
	}
	s.removed(shas)
	return nil
}

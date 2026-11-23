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
	"github.com/aspiziri/SD2NAS/internal/media"
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
// rows, or for a directory-name key the name without "Staging/"; for a
// promote's key it says whether the job is still queued. Other errors pass.
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
	if busy.Holder == HolderPromote {
		busy.Queued = s.promoteQueued(busy.ID, busy.Name)
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

// MaxFiles is how many files one move or cull may name.
const MaxFiles = 20000

// ErrChanged: a file moved to a folder the request did not lock between
// the lookup and the lock.
var ErrChanged = errors.New("staging: files moved while the request was being made; reload and try again")

// MoveRequest names the files to move and exactly one destination: an
// existing folder, or the title of a new one.
type MoveRequest struct {
	SHAs     []string
	ToID     int64  // an existing folder…
	NewTitle string // …or a new folder "<earliest source date> - NewTitle" (exactly one of the two)
}

// MoveResult is what Move did.
type MoveResult struct {
	Folder  Folder  `json:"folder"` // the destination
	Moved   int     `json:"moved"`
	Removed []int64 `json:"removed"` // source folders left empty and removed
}

// CullResult is what Cull did.
type CullResult struct {
	Culled  int     `json:"culled"`
	Removed []int64 `json:"removed"`
}

// DiscardResult is what Discard did.
type DiscardResult struct {
	Culled  int      `json:"culled"`
	Removed bool     `json:"removed"` // false: untracked files kept the folder (listed in Kept)
	Kept    []string `json:"kept"`
}

// checkSHAs de-duplicates shas and checks each is 64 lowercase hex digits.
func checkSHAs(shas []string) ([]string, error) {
	if len(shas) == 0 {
		return nil, fmt.Errorf("%w: no files given", ErrBadRequest)
	}
	if len(shas) > MaxFiles {
		return nil, fmt.Errorf("%w: at most %d files at once", ErrBadRequest, MaxFiles)
	}
	out := slices.Clone(shas)
	slices.Sort(out)
	out = slices.Compact(out)
	for _, h := range out {
		if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
			return nil, fmt.Errorf("%w: %q is not a file hash", ErrBadRequest, h)
		}
	}
	return out, nil
}

// located is a staged file and the folder holding it.
type located struct {
	asset  catalog.Asset
	folder catalog.StagingFolder
	rel    string // path inside the folder
}

// locate finds staged files shas and their folders. A hash that isn't a
// staged file inside a staging folder is ErrBadRequest.
func (s *Service) locate(ctx context.Context, shas []string) ([]located, error) {
	byHash, err := s.db.AssetsByHash(ctx, shas)
	if err != nil {
		return nil, err
	}
	all, err := s.db.StagingFolders(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]catalog.StagingFolder{}
	for _, r := range all {
		byName[r.Name] = r
	}
	out := make([]located, 0, len(shas))
	for _, h := range shas {
		a, ok := byHash[h]
		if !ok {
			return nil, fmt.Errorf("%w: no file %s", ErrBadRequest, h)
		}
		if a.State != catalog.StateStaged {
			return nil, fmt.Errorf("%w: %s is not in staging (it is %s)", ErrBadRequest, h, a.State)
		}
		name, rel, ok := strings.Cut(strings.TrimPrefix(a.RelPath, Dir+"/"), "/")
		row, known := byName[name]
		if !strings.HasPrefix(a.RelPath, Dir+"/") || !ok || !known {
			return nil, fmt.Errorf("%w: %s is not inside a staging folder", ErrBadRequest, a.RelPath)
		}
		out = append(out, located{asset: a, folder: row, rel: rel})
	}
	slices.SortFunc(out, func(a, b located) int { return strings.Compare(a.asset.RelPath, b.asset.RelPath) })
	return out, nil
}

// lockLocated locates shas, locks their folders plus extra, and locates
// again under the locks: a file that moved meanwhile is ErrChanged.
func (s *Service) lockLocated(ctx context.Context, shas []string, extra ...int64) ([]located, map[int64]catalog.StagingFolder, func(), error) {
	files, err := s.locate(ctx, shas)
	if err != nil {
		return nil, nil, nil, err
	}
	ids := slices.Clone(extra)
	for _, f := range files {
		ids = append(ids, f.folder.ID)
	}
	rows, release, err := s.lockFolders(ctx, uniqueIDs(ids))
	if err != nil {
		return nil, nil, nil, err
	}
	locked := map[int64]catalog.StagingFolder{}
	for _, r := range rows {
		locked[r.ID] = r
	}
	files, err = s.locate(ctx, shas)
	if err == nil {
		for _, f := range files {
			if _, ok := locked[f.folder.ID]; !ok {
				err = ErrChanged
				break
			}
		}
	}
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	return files, locked, release, nil
}

// dropIfEmpty removes row's directory and row when no staged file is left
// in it and only junk remains on disk. It reports whether it removed them.
func (s *Service) dropIfEmpty(ctx context.Context, row catalog.StagingFolder, touched map[string]bool) (bool, error) {
	left, err := s.db.AssetsUnder(ctx, prefixOf(row.Name), catalog.StateStaged)
	if err != nil || len(left) > 0 {
		return false, err
	}
	gone, _, err := removeEmptyTree(s.folderDir(row.Name))
	if err != nil || !gone {
		return false, err
	}
	touched[filepath.Join(s.photosRoot, Dir)] = true
	return true, s.db.DeleteStagingFolder(ctx, row.ID)
}

// moveDate is the folder date a moved file adds to its destination: its
// capture date with today's day cutoff, else its source folder's date.
func moveDate(a catalog.Asset, src catalog.StagingFolder, cutoff time.Duration) string {
	if !a.CaptureTime.IsZero() {
		return media.FolderDate(a.CaptureTime, cutoff)
	}
	return folderDate(src)
}

// Move moves staged files into an existing folder or a new one (§8 "Move
// files"), keeping each file's path inside its folder. Source folders left
// with no files are removed.
func (s *Service) Move(ctx context.Context, req MoveRequest) (MoveResult, error) {
	shas, err := checkSHAs(req.SHAs)
	if err != nil {
		return MoveResult{}, err
	}
	title := strings.TrimSpace(req.NewTitle)
	switch {
	case req.ToID > 0 && req.NewTitle != "", req.ToID <= 0 && req.NewTitle == "":
		return MoveResult{}, fmt.Errorf("%w: give either a destination folder or a new folder's title", ErrBadRequest)
	case req.ToID <= 0:
		if title, err = CleanTitle(title); err != nil {
			return MoveResult{}, err
		}
		if title == "" {
			return MoveResult{}, fmt.Errorf("%w: a new folder needs a title", ErrBadTitle)
		}
	}
	var extra []int64
	if req.ToID > 0 {
		extra = append(extra, req.ToID)
	}
	files, locked, release, err := s.lockLocated(ctx, shas, extra...)
	if err != nil {
		return MoveResult{}, err
	}
	defer release()
	settings, err := s.db.Settings(ctx)
	if err != nil {
		return MoveResult{}, err
	}
	var sources []catalog.StagingFolder
	for id, r := range locked {
		if id != req.ToID {
			sources = append(sources, r)
		}
	}
	slices.SortFunc(sources, func(a, b catalog.StagingFolder) int { return strings.Compare(a.Name, b.Name) })

	dest, ok := locked[req.ToID]
	if !ok { // a new folder
		name := FolderName(earliestDate(sources), title)
		if err := checkName(name); err != nil {
			return MoveResult{}, fmt.Errorf("%w: %q %v", ErrBadTitle, name, err)
		}
		releaseName, err := s.lockName(name)
		if err != nil {
			return MoveResult{}, err
		}
		defer releaseName()
		if err := s.nameFree(ctx, name); err != nil {
			return MoveResult{}, err
		}
		defer s.changed()
		// The directory first, then the row (filesystem first, pre-flight G7).
		if err := os.MkdirAll(s.folderDir(name), 0o777); err != nil {
			return MoveResult{}, err
		}
		if dest, err = s.db.CreateStagingFolder(ctx, name, nil, oldestCreated(sources)); err != nil {
			if rmErr := os.Remove(s.folderDir(name)); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				err = errors.Join(err, rmErr)
			}
			return MoveResult{}, err
		}
		releaseNew, err := s.lockRows(HolderOperation, dest) // nobody else knows it yet
		if err != nil {
			return MoveResult{}, err
		}
		defer releaseNew()
	} else {
		defer s.changed()
	}
	res := MoveResult{Removed: []int64{}}
	touched := map[string]bool{filepath.Join(s.photosRoot, Dir): true}
	dates := slices.Clone(dest.Dates)
	var moveErr error
	for _, f := range files {
		if f.folder.ID == dest.ID {
			continue
		}
		from := s.abs(f.asset.RelPath)
		dir := filepath.Join(s.folderDir(dest.Name), filepath.FromSlash(path.Dir(f.rel)))
		to, err := s.moveFile(ctx, from, dir, path.Base(f.rel), f.asset.SHA256)
		// moveFile returns the new path along with an error when the copy was
		// verified but the original could not be removed: the file is at to now,
		// so record that before returning the error.
		if err != nil && to == "" {
			moveErr = fmt.Errorf("moved %d of %d files: %w", res.Moved, len(files), err)
			break
		}
		touched[filepath.Dir(from)], touched[filepath.Dir(to)] = true, true
		newRel, relErr := s.relOf(to)
		if relErr == nil {
			relErr = s.db.MoveAsset(ctx, f.asset.SHA256, newRel, s.now())
		}
		if relErr != nil {
			moveErr = errors.Join(err, fmt.Errorf("moved %s but could not record it (a rescan repairs this): %w", f.asset.RelPath, relErr))
			break
		}
		res.Moved++
		if d := moveDate(f.asset, f.folder, settings.DayStartsAt); d != "" {
			dates = append(dates, d)
		}
		if err != nil {
			moveErr = fmt.Errorf("moved %d of %d files: %w", res.Moved, len(files), err)
			break
		}
	}
	// Even after a failure, record the dates of the files that did move.
	if err := s.db.SetStagingFolderInfo(ctx, dest.ID, dates, time.Time{}); err != nil {
		return res, errors.Join(moveErr, err)
	}
	if moveErr != nil {
		return res, errors.Join(moveErr, syncDirs(touched))
	}
	for _, src := range sources {
		removed, err := s.dropIfEmpty(ctx, src, touched)
		if err != nil {
			return res, err
		}
		if removed {
			res.Removed = append(res.Removed, src.ID)
		}
	}
	if err := syncDirs(touched); err != nil {
		return res, err
	}
	res.Folder, err = s.summary(ctx, dest.ID)
	return res, err
}

// Cull deletes staged files and remembers them as culled (§6), so the card
// never brings them back. Folders left with no files are removed.
func (s *Service) Cull(ctx context.Context, shas []string) (CullResult, error) {
	shas, err := checkSHAs(shas)
	if err != nil {
		return CullResult{}, err
	}
	files, locked, release, err := s.lockLocated(ctx, shas)
	if err != nil {
		return CullResult{}, err
	}
	defer release()
	defer s.changed()
	res := CullResult{Removed: []int64{}}
	touched := map[string]bool{}
	assets := make([]catalog.Asset, len(files))
	for i, f := range files {
		assets[i] = f.asset
	}
	culled, err := s.cullFiles(ctx, assets, touched)
	s.removed(culled)
	res.Culled = len(culled)
	if err != nil {
		return res, err
	}
	for _, row := range sortedRows(locked) {
		removed, err := s.dropIfEmpty(ctx, row, touched)
		if err != nil {
			return res, err
		}
		if removed {
			res.Removed = append(res.Removed, row.ID)
		}
	}
	return res, syncDirs(touched)
}

// cullFiles deletes the files of staged assets and records each as culled,
// one by one, and returns the hashes it culled (also on error). Cull and
// Discard share it (pre-flight D10).
func (s *Service) cullFiles(ctx context.Context, assets []catalog.Asset, touched map[string]bool) ([]string, error) {
	var culled []string
	for _, a := range assets {
		p := s.abs(a.RelPath)
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return culled, fmt.Errorf("culled %d of %d files: %w", len(culled), len(assets), err)
		}
		touched[filepath.Dir(p)] = true
		if err := s.db.CullAsset(ctx, a.SHA256, s.now()); err != nil {
			return culled, fmt.Errorf("deleted %s but could not record it (a rescan repairs this): %w", a.RelPath, err)
		}
		culled = append(culled, a.SHA256)
	}
	return culled, nil
}

func sortedRows(m map[int64]catalog.StagingFolder) []catalog.StagingFolder {
	out := make([]catalog.StagingFolder, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b catalog.StagingFolder) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Discard culls every staged file of folder id and removes the folder. Files
// SD2NAS doesn't track are never deleted: they keep the folder, and Kept
// lists them (Decision 4).
func (s *Service) Discard(ctx context.Context, id int64) (DiscardResult, error) {
	rows, release, err := s.lockFolders(ctx, []int64{id})
	if err != nil {
		return DiscardResult{}, err
	}
	defer release()
	row := rows[0]
	assets, err := s.db.AssetsUnder(ctx, prefixOf(row.Name), catalog.StateStaged)
	if err != nil {
		return DiscardResult{}, err
	}
	defer s.changed()
	res := DiscardResult{Kept: []string{}}
	touched := map[string]bool{filepath.Join(s.photosRoot, Dir): true}
	culled, err := s.cullFiles(ctx, assets, touched)
	s.removed(culled)
	res.Culled = len(culled)
	if err != nil {
		return res, err
	}
	gone, kept, err := removeEmptyTree(s.folderDir(row.Name))
	if err != nil {
		return res, err
	}
	if gone {
		if err := s.db.DeleteStagingFolder(ctx, row.ID); err != nil {
			return res, err
		}
		res.Removed = true
	} else {
		res.Kept = kept
	}
	return res, syncDirs(touched)
}

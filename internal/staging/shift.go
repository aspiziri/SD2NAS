package staging

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

// MaxShift is the largest shift, in seconds (100 years of 365.25 days).
const MaxShift = 3155760000

const stampLayout = "2006-01-02 15.04.05"

// shiftWall adds d to t's wall clock, ignoring t's location's DST rules:
// capture times are naive wall clocks (catalog.CaptureLayout). The result
// carries that wall clock in UTC, so a time that doesn't exist in t's zone
// (a DST gap) is kept as it is; callers use only its wall fields.
func shiftWall(t time.Time, d time.Duration) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC).Add(d)
}

// shiftedName replaces base's leading stamp when it is old's; anything
// after it (video details, a "-1" burst suffix, the extension) is kept.
func shiftedName(base string, old, new time.Time) (string, bool) {
	s := old.Format(stampLayout)
	if !strings.HasPrefix(base, s) {
		return base, false
	}
	return new.Format(stampLayout) + base[len(s):], true
}

// wholeFolderName is the name a folder takes when all its files are
// shifted and now cover dates: a name starting with a date takes the
// earliest of them, title kept; any other name stays (M7 Decision 19).
func wholeFolderName(name string, dates []string) string {
	date, title := SplitName(name)
	if date == "" || len(dates) == 0 {
		return name
	}
	return FolderName(slices.Min(dates), title)
}

// ShiftRequest names the staged files to shift, all from one folder.
type ShiftRequest struct {
	SHAs    []string
	Seconds *int64 // required; ±MaxShift
}

// ShiftResult is what Shift did.
type ShiftResult struct {
	Shifted    int    `json:"shifted"`               // capture times changed
	Renamed    int    `json:"renamed"`               // of those, files whose name changed
	Moved      int    `json:"moved"`                 // of those, files re-filed into another folder
	Skipped    int    `json:"skipped"`               // files with no capture time, left alone
	Folder     Folder `json:"folder"`                // the source folder afterwards; zero when removed
	Removed    bool   `json:"removed"`               // the source folder was left empty and removed
	WantedName string `json:"wanted_name,omitempty"` // a whole-folder shift's new name, when it was taken
}

// shiftFile is a file to shift: where it is and its new capture time.
type shiftFile struct {
	located
	newCapture time.Time
	date       string // the folder date of newCapture
}

// shiftPlan is what a shift will do, read from the catalog.
type shiftPlan struct {
	src     catalog.StagingFolder
	files   []shiftFile      // the files with a capture time, in the order to process them
	skipped int              // files with no capture time
	whole   bool             // the request covers every staged file of src
	targets map[string]int64 // partial only: a new date src doesn't cover → the existing folder it goes to
	fresh   []string         // partial only: new dates no folder has, sorted
}

func (p shiftPlan) same(q shiftPlan) bool {
	return p.src.ID == q.src.ID && p.whole == q.whole && maps.Equal(p.targets, q.targets) && slices.Equal(p.fresh, q.fresh)
}

// planShift checks files (located, all staged) can be shifted by d and
// works out where each goes. cutoff is the day_starts_at setting.
func (s *Service) planShift(ctx context.Context, files []located, d time.Duration, cutoff time.Duration) (shiftPlan, error) {
	p := shiftPlan{src: files[0].folder, targets: map[string]int64{}}
	for _, f := range files {
		if f.folder.ID != p.src.ID {
			return shiftPlan{}, fmt.Errorf("%w: Shift files from one folder at a time", ErrBadRequest)
		}
	}
	under, err := s.db.AssetsUnder(ctx, prefixOf(p.src.Name), catalog.StateStaged)
	if err != nil {
		return shiftPlan{}, err
	}
	p.whole = len(under) == len(files) // the hashes are distinct and all under src
	for _, f := range files {
		if f.asset.CaptureTime.IsZero() {
			p.skipped++
			continue
		}
		nc := shiftWall(f.asset.CaptureTime, d)
		if y := nc.Year(); y < 1970 || y > 2099 {
			return shiftPlan{}, fmt.Errorf("%w: Shifting by that much would put %s outside 1970–2099", ErrBadRequest, path.Base(f.rel))
		}
		p.files = append(p.files, shiftFile{located: f, newCapture: nc, date: media.FolderDate(nc, cutoff)})
	}
	// Latest first for a positive shift, earliest first for a negative one,
	// so a file never lands on a name another file of the batch still has.
	slices.SortFunc(p.files, func(a, b shiftFile) int {
		c := a.asset.CaptureTime.Compare(b.asset.CaptureTime)
		if c == 0 {
			c = strings.Compare(a.asset.RelPath, b.asset.RelPath)
		}
		if d > 0 {
			return -c
		}
		return c
	})
	if p.whole {
		return p, nil
	}
	for _, f := range p.files {
		if slices.Contains(p.src.Dates, f.date) || p.targets[f.date] != 0 || slices.Contains(p.fresh, f.date) {
			continue
		}
		row, ok, err := s.db.StagingFolderWithDate(ctx, f.date)
		switch {
		case err != nil:
			return shiftPlan{}, err
		case ok:
			p.targets[f.date] = row.ID
		default:
			p.fresh = append(p.fresh, f.date)
		}
	}
	slices.Sort(p.fresh)
	return p, nil
}

// newDateFolder creates the folder named date for files re-filed into it,
// directory first, as Move creates a new folder, and locks it. The release
// gives back its name and its row.
func (s *Service) newDateFolder(ctx context.Context, date string, createdAt time.Time) (catalog.StagingFolder, func(), error) {
	releaseName, err := s.lockName(date)
	if err != nil {
		return catalog.StagingFolder{}, nil, err
	}
	fail := func(err error) (catalog.StagingFolder, func(), error) {
		releaseName()
		return catalog.StagingFolder{}, nil, err
	}
	if err := s.nameFree(ctx, date); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(s.folderDir(date), 0o777); err != nil {
		return fail(err)
	}
	row, err := s.db.CreateStagingFolder(ctx, date, []string{date}, createdAt)
	if err != nil {
		if rmErr := os.Remove(s.folderDir(date)); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
		return fail(err)
	}
	releaseRow, err := s.lockRows(HolderOperation, row) // nobody else knows it yet
	if err != nil {
		return fail(err)
	}
	return row, func() { releaseRow(); releaseName() }, nil
}

// Shift shifts the capture times of staged files of one folder by
// req.Seconds (§8 "Shift times", M7 Decisions 17–22): each file's leading
// stamp is renamed, and the files stay or are re-filed by their new folder
// date. Bytes never change. It stops between files when the service closes.
func (s *Service) Shift(ctx context.Context, req ShiftRequest) (ShiftResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ShiftResult{}, ErrClosed
	}
	s.wg.Add(1) // under s.mu with closed false, so Close waits for it
	s.mu.Unlock()
	defer s.wg.Done()

	switch {
	case req.Seconds == nil:
		return ShiftResult{}, fmt.Errorf("%w: Say how far to shift: seconds is required", ErrBadRequest)
	case *req.Seconds == 0:
		return ShiftResult{}, fmt.Errorf("%w: Shift by at least one second", ErrBadRequest)
	case *req.Seconds > MaxShift || *req.Seconds < -MaxShift:
		return ShiftResult{}, fmt.Errorf("%w: Shift by at most 100 years", ErrBadRequest)
	}
	d := time.Duration(*req.Seconds) * time.Second
	shas, err := checkSHAs(req.SHAs)
	if err != nil {
		return ShiftResult{}, err
	}
	settings, err := s.db.Settings(ctx)
	if err != nil {
		return ShiftResult{}, err
	}
	files, err := s.locate(ctx, shas)
	if err != nil {
		return ShiftResult{}, err
	}
	first, err := s.planShift(ctx, files, d, settings.DayStartsAt)
	if err != nil {
		return ShiftResult{}, err
	}
	files, locked, release, err := s.lockLocated(ctx, shas, slices.Collect(maps.Values(first.targets))...)
	if err != nil {
		return ShiftResult{}, err
	}
	defer release()
	p, err := s.planShift(ctx, files, d, settings.DayStartsAt)
	if err != nil {
		return ShiftResult{}, err
	}
	if !p.same(first) {
		return ShiftResult{}, ErrChanged
	}
	src := locked[p.src.ID]
	defer s.changed()

	touched := map[string]bool{filepath.Join(s.photosRoot, Dir): true}
	rows := map[int64]catalog.StagingFolder{src.ID: src} // every folder files go to
	dest := map[string]catalog.StagingFolder{}           // re-filed dates → their folder
	for date, id := range p.targets {
		dest[date], rows[id] = locked[id], locked[id]
	}
	var fresh []catalog.StagingFolder
	// dropFresh removes the new folders no file ended up in.
	dropFresh := func() error {
		var errs []error
		for _, row := range fresh {
			if _, err := s.dropIfEmpty(ctx, row, touched); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	for _, date := range p.fresh {
		row, releaseNew, err := s.newDateFolder(ctx, date, src.CreatedAt)
		if err != nil {
			return ShiftResult{}, errors.Join(err, dropFresh(), syncDirs(touched))
		}
		defer releaseNew()
		fresh = append(fresh, row)
		dest[date], rows[row.ID] = row, row
	}

	res := ShiftResult{Skipped: p.skipped}
	added := map[int64][]string{} // folder id → the dates its shifted files cover
	var runErr error
	for _, f := range p.files {
		if s.bgCtx.Err() != nil {
			runErr = errors.New("SD2NAS is shutting down")
			break
		}
		target := src
		if row, ok := dest[f.date]; ok {
			target = row
		}
		done, renamed, err := s.shiftOne(ctx, f, target, touched)
		if done {
			res.Shifted++
			if renamed {
				res.Renamed++
			}
			if target.ID != src.ID {
				res.Moved++
			}
			added[target.ID] = append(added[target.ID], f.date)
		}
		if err != nil {
			runErr = err
			break
		}
	}

	// Record the dates of the files that were shifted, also after a failure.
	var datesErr error
	for id, dates := range added {
		row := rows[id]
		next := unionDates([]catalog.StagingFolder{row}, dates...)
		if p.whole && runErr == nil {
			next = unionDates(nil, dates...)
		}
		if !slices.Equal(next, row.Dates) {
			if err := s.db.SetStagingFolderInfo(ctx, id, next, time.Time{}); err != nil {
				datesErr = errors.Join(datesErr, err)
			}
			row.Dates = next
			rows[id] = row
		}
	}
	if runErr != nil {
		return res, errors.Join(fmt.Errorf("shifted %d of %d files: %w", res.Shifted, len(p.files), runErr),
			datesErr, dropFresh(), syncDirs(touched))
	}
	if datesErr != nil {
		return res, errors.Join(datesErr, dropFresh(), syncDirs(touched))
	}

	if p.whole && res.Shifted > 0 {
		src = rows[src.ID]
		if want := wholeFolderName(src.Name, src.Dates); want != src.Name {
			wanted, err := s.renameWhole(ctx, src, want, touched)
			if err != nil {
				return res, errors.Join(err, syncDirs(touched))
			}
			if wanted {
				res.WantedName = want
			}
		}
	} else if !p.whole {
		if res.Removed, err = s.dropIfEmpty(ctx, src, touched); err != nil {
			return res, errors.Join(err, dropFresh(), syncDirs(touched))
		}
	}
	if err := errors.Join(dropFresh(), syncDirs(touched)); err != nil {
		return res, err
	}
	if !res.Removed {
		res.Folder, err = s.summary(ctx, src.ID)
	}
	return res, err
}

// renameWhole renames src to want after a whole-folder shift. When the
// name is taken or busy, src keeps its name and wanted is true.
func (s *Service) renameWhole(ctx context.Context, src catalog.StagingFolder, want string, touched map[string]bool) (wanted bool, _ error) {
	releaseName, err := s.lockName(want)
	var busy *BusyError
	if errors.As(err, &busy) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer releaseName()
	switch err := s.nameFree(ctx, want, src); {
	case errors.Is(err, catalog.ErrNameTaken):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, s.renameFolder(ctx, src, want, touched)
}

// shiftOne moves file f to its new name in target and records its new
// capture time and path. When the catalog refuses the record, the file is
// moved back, so a file is either fully shifted or untouched. done says
// whether the shift was recorded (err may still report a problem after
// it, as moveFile can).
func (s *Service) shiftOne(ctx context.Context, f shiftFile, target catalog.StagingFolder, touched map[string]bool) (done, renamed bool, err error) {
	sha := f.asset.SHA256
	oldBase := path.Base(f.rel)
	base, _ := shiftedName(oldBase, f.asset.CaptureTime, f.newCapture)
	from := s.abs(f.asset.RelPath)
	to, newRel := from, f.asset.RelPath
	var moveErr error
	if target.ID != f.folder.ID || base != oldBase {
		dir := filepath.Join(s.folderDir(target.Name), filepath.FromSlash(path.Dir(f.rel)))
		to, moveErr = s.moveFile(ctx, from, dir, base, sha)
		// moveFile returns the new path along with an error when the copy was
		// verified but the original could not be removed: the file is at to.
		if moveErr != nil && to == "" {
			return false, false, moveErr
		}
		touched[filepath.Dir(from)], touched[filepath.Dir(to)] = true, true
	}
	recErr := error(nil)
	if to != from {
		newRel, recErr = s.relOf(to)
	}
	if recErr == nil {
		recErr = s.shiftAsset(ctx, sha, newRel, f.newCapture, s.now())
	}
	if recErr == nil {
		return true, path.Base(newRel) != oldBase, moveErr
	}
	recErr = fmt.Errorf("could not record the shift of %s: %w", f.asset.RelPath, recErr)
	if to != from {
		back, backErr := s.moveFile(ctx, to, filepath.Dir(from), oldBase, sha)
		if backErr == nil && back != from {
			backErr = fmt.Errorf("it is at %s", back)
		}
		if backErr != nil {
			s.log.Error("shift: could not move a file back after its record failed; a rescan follows it", "path", to, "err", backErr)
			recErr = errors.Join(recErr, fmt.Errorf("moving it back from %s failed (a rescan follows it): %w", to, backErr))
		}
	}
	return false, false, errors.Join(moveErr, recErr)
}

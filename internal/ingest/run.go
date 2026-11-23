package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

// Request is what the card script posts.
type Request struct {
	Path  string // card root inside the container, e.g. /cards/SONY_A7IV
	Label string
	UUID  string
}

// Config wires a Runner to its catalog and photos share.
type Config struct {
	Catalog    *catalog.DB
	PhotosRoot string                            // the photos share; Staging is PhotosRoot/Staging
	Now        func() time.Time                  // nil: time.Now
	FreeSpace  func(path string) (uint64, error) // nil: FreeSpace
	Log        *slog.Logger                      // nil: slog.Default()
}

// Runner imports cards into Staging.
type Runner struct {
	cfg Config
	// Seams, set by NewRunner to the media functions; package tests replace
	// them to inject failures or block.
	fingerprint func(path string) (string, error)                          // media.QuickFingerprint
	hashFile    func(path string) (string, error)                          // media.HashFile
	copyFile    func(ctx context.Context, src, dst string) (string, error) // media.CopyHashed
}

// NewRunner returns a Runner for cfg.
func NewRunner(cfg Config) *Runner {
	return &Runner{cfg: cfg, fingerprint: media.QuickFingerprint, hashFile: media.HashFile, copyFile: media.CopyHashed}
}

func (r *Runner) now() time.Time {
	if r.cfg.Now != nil {
		return r.cfg.Now()
	}
	return time.Now()
}

func (r *Runner) log() *slog.Logger {
	if r.cfg.Log != nil {
		return r.cfg.Log
	}
	return slog.Default()
}

func (r *Runner) freeSpace(path string) (uint64, error) {
	if r.cfg.FreeSpace != nil {
		return r.cfg.FreeSpace(path)
	}
	return FreeSpace(path)
}

// ingestRun is the state of one Run.
type ingestRun struct {
	r           *Runner
	ctx         context.Context // the job's context: stops the pipeline
	dbctx       context.Context // context.WithoutCancel(ctx), for every catalog call (Decision 24)
	id          int64
	card        string      // the card root, in.CardPath
	now         time.Time   // r.now(), taken once
	rules       media.Rules // media.DefaultRules()
	settings    catalog.Settings
	stagingRoot string       // filepath.Join(PhotosRoot, StagingDir)
	log         *slog.Logger // r.log().With("ingest", id, "card", CardName(in))
	p           *Progress
	counts      catalog.Counts
	warnings    []catalog.ClockWarning
	cardSize    int64 // sum of the Media sizes
	// For the "ingest finished" log line (Decision 22):
	scanDur     time.Duration
	classifyDur time.Duration
	captureDur  time.Duration
	copyDur     time.Duration
	copyBytes   int64
}

func (j *ingestRun) cat() *catalog.DB { return j.r.cfg.Catalog }

// Run imports the card of in (a queued row from CreateIngest) and returns
// the finished row. The outcome, including failure, is in the row; the
// error is non-nil only when the catalog couldn't record the start or the
// finish. When the start can't be recorded, Run first tries to mark the
// row failed with that error, so it is never left queued.
func (r *Runner) Run(ctx context.Context, in catalog.Ingest, p *Progress) (catalog.Ingest, error) {
	now := r.now()
	j := &ingestRun{
		r:           r,
		ctx:         ctx,
		dbctx:       context.WithoutCancel(ctx),
		id:          in.ID,
		card:        in.CardPath,
		now:         now,
		rules:       media.DefaultRules(),
		stagingRoot: filepath.Join(r.cfg.PhotosRoot, StagingDir),
		log:         r.log().With("ingest", in.ID, "card", CardName(in)),
		p:           p,
	}
	if err := j.cat().StartIngest(j.dbctx, in.ID, now); err != nil {
		text := "start import: " + err.Error()
		finishErr := j.cat().FinishIngest(j.dbctx, catalog.Ingest{
			ID: in.ID, Status: catalog.StatusFailed, FinishedAt: now, Error: text,
		})
		in.Status, in.FinishedAt, in.Error = catalog.StatusFailed, now, text
		return in, fmt.Errorf("start ingest %d: %w", in.ID, errors.Join(err, finishErr))
	}
	status, errText := j.pipeline()
	return j.finish(status, errText)
}

// classified is a card file with its catalog outcome.
type classified struct {
	CardFile
	fp      string
	outcome catalog.Outcome
	sha     string // the matched asset's, for a known outcome
	err     error  // the reason, for a failed outcome
	skipped bool   // not classified: the job was cancelled first
}

func (j *ingestRun) pipeline() (catalog.IngestStatus, string) {
	r := j.r
	j.p.update(func(s *ProgressSnapshot) { s.Phase = PhaseScanning })
	start := time.Now()
	settings, err := j.cat().Settings(j.dbctx)
	if err != nil {
		return catalog.StatusFailed, "settings: " + err.Error()
	}
	j.settings = settings
	scan, err := Scan(j.card, j.rules)
	j.scanDur = time.Since(start)
	if err != nil {
		return catalog.StatusFailed, "scan card: " + err.Error()
	}
	var rows []catalog.IngestFile
	for _, se := range scan.Errors {
		rows = append(rows, catalog.IngestFile{CardPath: se.Rel, Outcome: catalog.OutcomeFailed, Error: se.Err.Error()})
	}
	for _, u := range scan.Unsupported {
		rows = append(rows, catalog.IngestFile{CardPath: u.Rel, Size: u.Size, Outcome: catalog.OutcomeUnsupported})
	}
	for _, f := range scan.Media {
		j.cardSize += f.Size
	}

	j.p.update(func(s *ProgressSnapshot) {
		s.Phase, s.FilesTotal, s.FilesDone = PhaseClassifying, len(scan.Media), 0
	})
	start = time.Now()
	results := j.classifyAll(scan.Media)
	j.classifyDur = time.Since(start)

	var fresh []newFile
	for _, c := range results {
		if c.skipped {
			continue
		}
		switch c.outcome {
		case catalog.OutcomeNew:
			fresh = append(fresh, newFile{CardFile: c.CardFile, fp: c.fp})
		case catalog.OutcomeFailed:
			rows = append(rows, catalog.IngestFile{CardPath: c.Rel, Size: c.Size, QuickFP: c.fp,
				Outcome: catalog.OutcomeFailed, Error: c.err.Error()})
		default:
			rows = append(rows, catalog.IngestFile{CardPath: c.Rel, Size: c.Size, QuickFP: c.fp, SHA256: c.sha, Outcome: c.outcome})
		}
	}
	if err := j.cat().AddIngestFiles(j.dbctx, j.id, rows); err != nil {
		return catalog.StatusFailed, "record files: " + err.Error()
	}
	for _, row := range rows {
		j.addOutcome(row.Outcome)
	}
	if j.ctx.Err() != nil {
		return catalog.StatusCancelled, ""
	}
	if j.counts.Failed > 0 {
		if err := j.cardErr(); err != nil {
			return catalog.StatusFailed, "card removed during import: " + err.Error()
		}
	}
	if len(fresh) == 0 {
		return catalog.StatusDone, ""
	}

	partials, err := CleanPartials(j.stagingRoot)
	for _, p := range partials {
		j.log.Warn("removed partial file from an interrupted import", "path", p)
	}
	if err != nil {
		return catalog.StatusFailed, "clean partial files: " + err.Error()
	}
	free, err := r.freeSpace(r.cfg.PhotosRoot)
	if err != nil {
		return catalog.StatusFailed, "free space: " + err.Error()
	}
	var need uint64
	var needBytes int64
	for _, f := range fresh {
		need += uint64(f.Size)
		needBytes += f.Size
	}
	if free < need+j.settings.ReserveBytes {
		return catalog.StatusRefused, fmt.Sprintf("staging would drop below the %s free-space reserve (%d new %s need %s, %s free)",
			formatGB(j.settings.ReserveBytes), len(fresh), plural(len(fresh), "file", "files"), formatGB(need), formatGB(free))
	}

	j.p.update(func(s *ProgressSnapshot) {
		s.Phase, s.FilesTotal, s.FilesDone, s.BytesTotal, s.BytesDone = PhaseCopying, len(fresh), 0, needBytes, 0
	})
	for _, f := range fresh {
		if j.ctx.Err() != nil {
			return catalog.StatusCancelled, ""
		}
		res := j.importOne(f)
		if res.canceled {
			return catalog.StatusCancelled, ""
		}
		if !res.recorded {
			if err := j.cat().AddIngestFiles(j.dbctx, j.id, []catalog.IngestFile{res.row}); err != nil {
				return catalog.StatusFailed, "record files: " + err.Error()
			}
		}
		j.addOutcome(res.row.Outcome)
		j.warnings = append(j.warnings, res.warnings...)
		j.p.update(func(s *ProgressSnapshot) { s.FilesDone++; s.BytesDone += f.Size })
		if res.row.Outcome == catalog.OutcomeFailed {
			if err := j.cardErr(); err != nil {
				return catalog.StatusFailed, "card removed during import: " + err.Error()
			}
		}
	}
	return catalog.StatusDone, ""
}

// cardErr is non-nil when the card root can no longer be read (pulled card).
func (j *ingestRun) cardErr() error {
	_, err := os.Stat(j.card)
	return err
}

// addOutcome counts one file in the run's totals and the live progress.
func (j *ingestRun) addOutcome(o catalog.Outcome) {
	j.counts.Add(o)
	j.p.update(func(s *ProgressSnapshot) { s.Counts.Add(o) })
}

// classifyAll classifies files with 3 workers; results keep the files' order.
func (j *ingestRun) classifyAll(files []CardFile) []classified {
	results := make([]classified, len(files))
	idx := make(chan int)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				if j.ctx.Err() != nil {
					results[i] = classified{CardFile: files[i], skipped: true}
					continue
				}
				results[i] = j.classifyOne(files[i])
				j.p.update(func(s *ProgressSnapshot) { s.FilesDone++ })
			}
		}()
	}
	for i := range files {
		idx <- i
	}
	close(idx)
	wg.Wait()
	return results
}

// stateRank orders asset states for a fingerprint matching several assets
// (Decision 8): archived, staged, culled, missing. An unknown state has no rank.
func stateRank(s catalog.AssetState) (int, error) {
	switch s {
	case catalog.StateArchived:
		return 0, nil
	case catalog.StateStaged:
		return 1, nil
	case catalog.StateCulled:
		return 2, nil
	case catalog.StateMissing:
		return 3, nil
	}
	return 0, fmt.Errorf("catalog asset has unknown state %q", s)
}

// knownOutcome maps an asset state to the outcome of a card file matching it
// (Decision 8). An unknown state is an error, never a safe outcome.
func knownOutcome(s catalog.AssetState) (catalog.Outcome, error) {
	switch s {
	case catalog.StateArchived:
		return catalog.OutcomeInArchive, nil
	case catalog.StateStaged:
		return catalog.OutcomeInStaging, nil
	case catalog.StateCulled, catalog.StateMissing:
		return catalog.OutcomeSeenBefore, nil
	}
	return "", fmt.Errorf("catalog asset has unknown state %q", s)
}

func (j *ingestRun) classifyOne(f CardFile) classified {
	r := j.r
	c := classified{CardFile: f}
	fail := func(err error) classified {
		c.outcome, c.err = catalog.OutcomeFailed, err
		return c
	}
	known := func(a catalog.Asset) classified {
		o, err := knownOutcome(a.State)
		if err != nil {
			return fail(err)
		}
		c.outcome, c.sha = o, a.SHA256
		return c
	}
	if f.Size == 0 {
		return fail(errors.New("empty file"))
	}
	fp, err := r.fingerprint(f.Path)
	if err != nil {
		return fail(fmt.Errorf("fingerprint: %w", err))
	}
	c.fp = fp
	matches, err := j.cat().AssetsByFingerprint(j.dbctx, fp)
	if err != nil {
		return fail(fmt.Errorf("catalog lookup: %w", err))
	}
	if len(matches) == 0 {
		c.outcome = catalog.OutcomeNew
		return c
	}
	if j.settings.VerifyKnown != catalog.VerifyFull {
		best, bestRank := matches[0], 0
		for i, a := range matches {
			rank, err := stateRank(a.State)
			if err != nil {
				return fail(err)
			}
			if i == 0 || rank < bestRank {
				best, bestRank = a, rank
			}
		}
		return known(best)
	}
	h, err := r.hashFile(f.Path)
	if err != nil {
		return fail(fmt.Errorf("hash: %w", err))
	}
	for _, a := range matches {
		if a.SHA256 == h {
			return known(a)
		}
	}
	a, err := j.cat().AssetBySHA(j.dbctx, h)
	switch {
	case err == nil:
		if err := j.cat().AddFingerprint(j.dbctx, fp, a.SHA256); err != nil {
			return fail(fmt.Errorf("catalog update: %w", err))
		}
		return known(a)
	case errors.Is(err, catalog.ErrNotFound):
		c.outcome = catalog.OutcomeNew
		return c
	}
	return fail(fmt.Errorf("catalog lookup: %w", err))
}

// newFile is a card file classified as new, with its quick fingerprint.
type newFile struct {
	CardFile
	fp string
}

// fileResult is what importing one new file produced.
type fileResult struct {
	row      catalog.IngestFile
	recorded bool // RecordNew already wrote row (and the asset)
	warnings []catalog.ClockWarning
	canceled bool // the job was cancelled mid-copy; row is not to be recorded
}

// captureOf resolves f's capture time and, for a video, returns the
// metadata it read, so the card's moov box is read once (Decision 25).
// Non-videos go through media.CaptureTime. For a video it mirrors
// CaptureTime's video branch: a metadata error or a missing mvhd time is
// the Fallback, and the modified time names the file.
func captureOf(path string, kind media.Kind) (media.Capture, *media.VideoMeta, error) {
	if kind != media.KindVideo {
		c, err := media.CaptureTime(path, kind)
		return c, nil, err
	}
	var c media.Capture
	meta, err := media.ReadVideoMeta(path)
	switch {
	case err != nil:
		meta = nil
		c.Fallback = fmt.Errorf("video metadata: %w", err)
	case meta.CreationTime.IsZero():
		c.Camera = strings.TrimSpace(meta.Make + " " + meta.Model)
		c.Fallback = media.ErrNoMvhdTime
	default:
		c.Camera = strings.TrimSpace(meta.Make + " " + meta.Model)
		c.Time, c.Source = meta.CreationTime.In(time.Local), media.SourceMvhd
		return c, meta, nil
	}
	mtime, err := media.FileModDate(path)
	if err != nil {
		return media.Capture{}, nil, errors.Join(c.Fallback, err)
	}
	c.Time, c.Source = mtime, media.SourceMtime
	return c, meta, nil
}

func (j *ingestRun) importOne(f newFile) fileResult {
	fail := func(err error) fileResult { return fileResult{row: failedRow(f, err)} }
	start := time.Now()
	c, meta, err := captureOf(f.Path, f.Kind) // meta is nil unless f is a video whose metadata was read
	j.captureDur += time.Since(start)
	if err != nil {
		return fail(fmt.Errorf("capture time: %w", err))
	}
	if c.Fallback != nil {
		j.log.Warn("capture time fell back", "file", f.Rel, "source", c.Source, "reason", c.Fallback)
	}
	folder, err := j.cat().StagingFolderForDate(j.dbctx, media.FolderDate(c.Time, j.settings.DayStartsAt), j.now)
	if err != nil {
		return fail(fmt.Errorf("staging folder: %w", err))
	}
	if folder.Name == "" || folder.Name == "." || folder.Name == ".." || strings.ContainsAny(folder.Name, `/\`) {
		return fail(fmt.Errorf("staging folder name %q is not a single folder name", folder.Name))
	}
	dir := filepath.Join(j.stagingRoot, folder.Name, media.TypeSubdir(f.Kind, j.rules))
	if err := mkdirDurable(dir); err != nil {
		return fail(err)
	}
	name := media.DestFileName(c.Time, f.Kind, meta, filepath.Ext(f.Path))
	start = time.Now()
	dst, sum, dup, err := j.copyNew(f, dir, name)
	j.copyDur += time.Since(start)
	if err != nil {
		if media.ClassifyCopyError(err, f.Path, dst) == media.CopyFailCanceled {
			return fileResult{canceled: true}
		}
		return fail(err)
	}
	rel, err := j.rel(dst)
	if err != nil {
		return fail(fmt.Errorf("copied but cannot record it: %w", err))
	}
	if dup {
		return j.recordExisting(f, dst, rel)
	}
	j.copyBytes += f.Size
	if err := media.SyncDir(filepath.Dir(dst)); err != nil {
		return fail(fmt.Errorf("copied to %s but could not sync its folder: %w", rel, err))
	}
	existing, err := j.cat().AssetBySHA(j.dbctx, sum)
	switch {
	case err == nil:
		return j.dropCopy(f, dst, rel, existing) // fingerprint collision or alias (§6)
	case !errors.Is(err, catalog.ErrNotFound):
		return fail(fmt.Errorf("copied to %s but the catalog lookup failed: %w", rel, err))
	}
	info, err := os.Stat(dst)
	if err != nil {
		return fail(fmt.Errorf("copied to %s but could not stat it: %w", rel, err))
	}
	a := catalog.Asset{SHA256: sum, Size: f.Size, Kind: f.Kind, CaptureTime: c.Time, TZOffset: c.Offset,
		State: catalog.StateStaged, RelPath: rel, Mtime: info.ModTime().UnixNano(), UpdatedAt: j.now}
	row := catalog.IngestFile{CardPath: f.Rel, Size: f.Size, QuickFP: f.fp, SHA256: sum, Outcome: catalog.OutcomeNew, RelPath: a.RelPath}
	if f.Kind == media.KindVideo && meta == nil {
		row.Error = "named without video metadata: " + c.Fallback.Error() // imported, but not silently (Decision 25)
	}
	if err := j.cat().RecordNew(j.dbctx, j.id, a, f.fp, row); err != nil {
		return fail(fmt.Errorf("copied to %s but not recorded: %w", a.RelPath, err))
	}
	ws := CheckClock(c, j.now)
	for i := range ws {
		ws[i].CardPath, ws[i].SHA256 = f.Rel, sum
	}
	return fileResult{row: row, recorded: true, warnings: ws}
}

func failedRow(f newFile, err error) catalog.IngestFile {
	return catalog.IngestFile{CardPath: f.Rel, Size: f.Size, QuickFP: f.fp, Outcome: catalog.OutcomeFailed, Error: err.Error()}
}

// rel is dst as a slash path relative to the photos root; it is an error
// when dst is not below that root.
func (j *ingestRun) rel(dst string) (string, error) {
	rel, err := filepath.Rel(j.r.cfg.PhotosRoot, dst)
	if err != nil {
		return "", fmt.Errorf("%s is not under the photos root: %w", dst, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is not under the photos root %s", dst, j.r.cfg.PhotosRoot)
	}
	return filepath.ToSlash(rel), nil
}

// copyNew picks a free name in dir and copies f there, retrying the
// failures a retry can fix (Decision 13). dup reports identical content
// already at dst, so nothing was copied.
func (j *ingestRun) copyNew(f newFile, dir, name string) (dst, sum string, dup bool, err error) {
	const attempts = 3
	for range attempts {
		dst, dup, err = media.FreeName(f.Path, dir, name)
		if err != nil || dup {
			return dst, "", dup, err
		}
		sum, err = j.r.copyFile(j.ctx, f.Path, dst)
		if err == nil {
			return dst, sum, false, nil
		}
		switch media.ClassifyCopyError(err, f.Path, dst) {
		case media.CopyFailDestTaken:
			j.log.Warn("destination appeared during the copy; choosing another name", "dst", filepath.ToSlash(dst))
		case media.CopyFailStalePartial:
			// Ingest is the only writer of partials in Staging and runs alone,
			// so this one was left by an earlier, interrupted copy.
			if rmErr := os.Remove(dst + media.PartialSuffix); rmErr != nil {
				return dst, "", false, errors.Join(err, rmErr)
			}
			j.log.Warn("removed a stale partial", "path", filepath.ToSlash(dst+media.PartialSuffix))
		case media.CopyFailDestDirMissing:
			if mkErr := mkdirDurable(dir); mkErr != nil {
				return dst, "", false, errors.Join(err, mkErr)
			}
		default: // cancelled, source missing, or anything else: a retry can't help
			return dst, "", false, err
		}
	}
	return dst, "", false, fmt.Errorf("gave up after %d attempts: %w", attempts, err)
}

func knownRow(f newFile, a catalog.Asset) (catalog.IngestFile, error) {
	o, err := knownOutcome(a.State)
	if err != nil {
		return catalog.IngestFile{}, err
	}
	return catalog.IngestFile{CardPath: f.Rel, Size: f.Size, QuickFP: f.fp, SHA256: a.SHA256, Outcome: o}, nil
}

// recordExisting handles a file whose identical content is already at dst.
func (j *ingestRun) recordExisting(f newFile, dst, rel string) fileResult {
	fail := func(err error) fileResult { return fileResult{row: failedRow(f, err)} }
	sum, err := j.r.hashFile(f.Path)
	if err != nil {
		return fail(fmt.Errorf("hash %s: %w", f.Rel, err))
	}
	a, err := j.cat().AssetBySHA(j.dbctx, sum)
	switch {
	case err == nil:
		if err := j.cat().AddFingerprint(j.dbctx, f.fp, a.SHA256); err != nil {
			return fail(fmt.Errorf("record fingerprint: %w", err))
		}
		return j.knownResult(f, a)
	case errors.Is(err, catalog.ErrNotFound):
		return j.adopt(f, dst, rel, sum)
	}
	return fail(fmt.Errorf("catalog lookup: %w", err))
}

// adopt records the copy at dst, whose content (sum) the catalog lacks: an
// earlier import copied it and stopped before writing the catalog row
// (Decision 14).
func (j *ingestRun) adopt(f newFile, dst, rel, sum string) fileResult {
	fail := func(err error) fileResult { return fileResult{row: failedRow(f, err)} }
	if err := media.SyncDir(filepath.Dir(dst)); err != nil {
		return fail(fmt.Errorf("adopting %s: could not sync its folder: %w", rel, err))
	}
	info, err := os.Stat(dst)
	if err != nil {
		return fail(fmt.Errorf("adopting %s: %w", rel, err))
	}
	a := catalog.Asset{SHA256: sum, Size: f.Size, Kind: f.Kind, State: catalog.StateStaged,
		RelPath: rel, Mtime: info.ModTime().UnixNano(), UpdatedAt: j.now}
	c, err := media.CaptureTime(f.Path, f.Kind)
	if err != nil {
		j.log.Warn("adopted copy has no capture time", "file", f.Rel, "reason", err)
	} else {
		a.CaptureTime, a.TZOffset = c.Time, c.Offset
	}
	row := catalog.IngestFile{CardPath: f.Rel, Size: f.Size, QuickFP: f.fp, SHA256: sum,
		Outcome: catalog.OutcomeInStaging, RelPath: rel}
	if err := j.cat().RecordNew(j.dbctx, j.id, a, f.fp, row); err != nil {
		return fail(fmt.Errorf("adopting %s: not recorded: %w", rel, err))
	}
	j.log.Warn("adopted a copy left in staging without a catalog row", "path", rel)
	return fileResult{row: row, recorded: true}
}

// dropCopy deletes the fresh copy at dst, whose content the catalog already has as a.
func (j *ingestRun) dropCopy(f newFile, dst, rel string, a catalog.Asset) fileResult {
	fail := func(err error) fileResult { return fileResult{row: failedRow(f, err)} }
	if err := os.Remove(dst); err != nil {
		return fail(fmt.Errorf("duplicate copy %s could not be removed: %w", rel, err))
	}
	if err := media.SyncDir(filepath.Dir(dst)); err != nil {
		return fail(fmt.Errorf("duplicate copy %s removed but its folder could not be synced: %w", rel, err))
	}
	if err := j.cat().AddFingerprint(j.dbctx, f.fp, a.SHA256); err != nil {
		return fail(fmt.Errorf("record fingerprint: %w", err))
	}
	return j.knownResult(f, a)
}

func (j *ingestRun) knownResult(f newFile, a catalog.Asset) fileResult {
	row, err := knownRow(f, a)
	if err != nil {
		return fileResult{row: failedRow(f, err)}
	}
	return fileResult{row: row}
}

func (j *ingestRun) finish(status catalog.IngestStatus, errText string) (catalog.Ingest, error) {
	j.p.update(func(s *ProgressSnapshot) { s.Phase = PhaseFinishing })
	if len(j.warnings) > 0 {
		if err := j.cat().AddClockWarnings(j.dbctx, j.id, j.warnings); err != nil {
			msg := "record clock warnings: " + err.Error()
			if errText != "" {
				msg = errText + "; " + msg
			}
			status, errText = catalog.StatusFailed, msg
		}
	}
	c := j.counts
	safe := status == catalog.StatusDone && c.Failed == 0 && c.Unsupported == 0 && c.Seen > 0 // Decision 9
	err := j.cat().FinishIngest(j.dbctx, catalog.Ingest{
		ID: j.id, Status: status, FinishedAt: j.r.now(), Counts: c, ClockWarnings: len(j.warnings),
		CardSize: j.cardSize, SafeToFormat: safe, Error: errText,
	})
	if err != nil {
		return catalog.Ingest{}, fmt.Errorf("finish ingest %d: %w", j.id, err)
	}
	j.log.Info("ingest finished", "status", status, "seen", c.Seen, "new", c.New, "failed", c.Failed,
		"scan", j.scanDur, "classify", j.classifyDur, "capture", j.captureDur, "copy", j.copyDur, "copy_bytes", j.copyBytes)
	j.p.update(func(s *ProgressSnapshot) { s.Phase = PhaseDone })
	return j.cat().Ingest(j.dbctx, j.id)
}

package staging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

// ArchiveDir is the archive under the photos root (§3).
const ArchiveDir = "Archive"

// promoteReportEvery throttles promote.progress (Decision 14).
const promoteReportEvery = 250 * time.Millisecond

var (
	ErrNoSuchJob  = errors.New("staging: no such promote job")
	ErrJobStarted = errors.New("staging: that promote has already started or finished")
)

const (
	PromoteQueued    = "queued"    // job
	PromoteRunning   = "running"   // job
	PromoteCancelled = "cancelled" // job
	PromoteWaiting   = "waiting"   // folder
	PromoteMoving    = "moving"    // folder
	PromoteDone      = "done"      // folder or job
	PromoteFailed    = "failed"    // folder, or a job where some folder failed
)

type PromoteFolder struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Target string `json:"target"` // "Archive/…", expanded at enqueue
	Files  int    `json:"files"`  // catalogued files when the job was queued
	Done   int    `json:"done"`   // of those, moved so far
	State  string `json:"state"`
	Merged bool   `json:"merged,omitempty"` // the target existed: merged file by file
	Copied bool   `json:"copied,omitempty"` // across filesystems: copied, verified, deleted
	Error  string `json:"error,omitempty"`
}
type PromoteJob struct {
	ID         int64           `json:"id"`
	State      string          `json:"state"`
	Pattern    string          `json:"pattern"`
	QueuedAt   time.Time       `json:"queued_at"`
	StartedAt  time.Time       `json:"started_at,omitzero"`
	FinishedAt time.Time       `json:"finished_at,omitzero"`
	FilesTotal int             `json:"files_total"`
	FilesDone  int             `json:"files_done"`
	BytesTotal int64           `json:"bytes_total"`
	BytesDone  int64           `json:"bytes_done"`
	Folders    []PromoteFolder `json:"folders"`
}

// PromoteQueue is the GET /api/staging/promote body and the promote.progress data.
type PromoteQueue struct {
	Running *PromoteJob  `json:"running"`
	Queued  []PromoteJob `json:"queued"` // FIFO order; never null
	Last    *PromoteJob  `json:"last"`   // the last finished (done or failed) job
}

// queuedJob is a job with its folders and their locks (one release per
// folder: its id and its target's name).
type queuedJob struct {
	job      *PromoteJob
	rows     []catalog.StagingFolder
	releases []func()
}

func (j *PromoteJob) clone() PromoteJob {
	c := *j
	c.Folders = slices.Clone(j.Folders)
	return c
}

// OnPromote registers fn to receive the queue after every change. Call it
// before serving. fn runs with the queue's lock held and must not block.
func (s *Service) OnPromote(fn func(PromoteQueue)) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.promoteFn = fn
}

// Promotes returns the running job, the queue and the last finished job.
func (s *Service) Promotes() PromoteQueue {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	return s.queueLocked()
}

func (s *Service) queueLocked() PromoteQueue {
	q := PromoteQueue{Queued: []PromoteJob{}}
	if s.running != nil {
		j := s.running.job.clone()
		q.Running = &j
	}
	for _, qj := range s.queue {
		q.Queued = append(q.Queued, qj.job.clone())
	}
	if s.last != nil {
		j := s.last.clone()
		q.Last = &j
	}
	return q
}

// promoteQueued reports whether folder id (or, for id 0, target name) is
// held by a job still waiting in the queue, for the 409 message.
func (s *Service) promoteQueued(id int64, name string) bool {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	for _, qj := range s.queue {
		for _, f := range qj.job.Folders {
			if (id != 0 && f.ID == id) || (id == 0 && strings.EqualFold(f.Target, name)) {
				return true
			}
		}
	}
	return false
}

// promoteTargets expands pattern for each row, as rel_paths under Archive/.
func promoteTargets(pattern string, rows []catalog.StagingFolder) ([]string, error) {
	out := make([]string, len(rows))
	for i, r := range rows {
		t, err := ExpandPattern(pattern, r)
		if err != nil {
			return nil, err
		}
		out[i] = ArchiveDir + "/" + t
	}
	return out, nil
}

// Promote queues a job moving folders ids into Archive/<pattern> (§8,
// Decision 9). The folders and their target names are locked now (holder
// promote), so busy folders or a bad pattern fail the request before
// anything is queued; jobs run one after another.
func (s *Service) Promote(ctx context.Context, ids []int64, pattern string) (PromoteJob, error) {
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return PromoteJob{}, fmt.Errorf("%w: no folders given", ErrBadRequest)
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		pattern = DefaultPattern
	}
	if s.isClosed() {
		return PromoteJob{}, ErrClosed
	}
	rows, err := s.rows(ctx, ids)
	if err != nil {
		return PromoteJob{}, err
	}
	targets, err := promoteTargets(pattern, rows)
	if err != nil {
		return PromoteJob{}, err
	}
	groups := make([]LockSet, len(rows))
	for i, row := range rows {
		groups[i] = LockSet{IDs: []int64{row.ID}, Names: []string{targets[i]}}
	}
	releases, err := s.locks.TryLockGroups(HolderPromote, groups)
	if err != nil {
		return PromoteJob{}, s.busyNamed(err, rows)
	}
	queued := false
	defer func() {
		if !queued {
			for _, r := range releases {
				r()
			}
		}
	}()
	// Again under the locks: an operation that finished just before them may
	// have renamed a folder, which would change its target.
	again, err := s.rows(ctx, ids)
	if err != nil {
		return PromoteJob{}, err
	}
	if t2, err := promoteTargets(pattern, again); err != nil || !slices.Equal(t2, targets) {
		return PromoteJob{}, errors.Join(ErrChanged, err)
	}
	job := &PromoteJob{State: PromoteQueued, Pattern: pattern, QueuedAt: s.now(), Folders: make([]PromoteFolder, len(again))}
	for i, row := range again {
		assets, err := s.db.AssetsUnder(ctx, prefixOf(row.Name), catalog.StateStaged)
		if err != nil {
			return PromoteJob{}, err
		}
		var bytes int64
		for _, a := range assets {
			bytes += a.Size
		}
		job.Folders[i] = PromoteFolder{ID: row.ID, Name: row.Name, Target: targets[i], Files: len(assets), State: PromoteWaiting}
		job.FilesTotal += len(assets)
		job.BytesTotal += bytes
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return PromoteJob{}, ErrClosed
	}
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.jobSeq++
	job.ID = s.jobSeq
	s.queue = append(s.queue, &queuedJob{job: job, rows: again, releases: releases})
	queued = true
	if !s.working {
		s.working = true
		s.wg.Add(1) // under s.mu with closed false, so Close waits for it
		go s.promoteWorker()
	}
	s.reportLocked(true)
	return job.clone(), nil
}

// CancelPromote drops queued job id and releases its folders and names. A
// running or finished job is ErrJobStarted; an unknown id ErrNoSuchJob.
func (s *Service) CancelPromote(id int64) (PromoteJob, error) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	for i, qj := range s.queue {
		if qj.job.ID != id {
			continue
		}
		s.queue = slices.Delete(s.queue, i, i+1)
		for _, r := range qj.releases {
			r()
		}
		qj.job.State, qj.job.FinishedAt = PromoteCancelled, s.now()
		s.reportLocked(true)
		return qj.job.clone(), nil
	}
	if id > 0 && id <= s.jobSeq {
		return PromoteJob{}, ErrJobStarted
	}
	return PromoteJob{}, ErrNoSuchJob
}

// dropQueue (from Close, after the context is cancelled) drops every queued
// job: their folders stay in Staging untouched and their locks go.
func (s *Service) dropQueue() {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	if len(s.queue) == 0 {
		return
	}
	var names []string
	for _, qj := range s.queue {
		for _, r := range qj.releases {
			r()
		}
		for _, f := range qj.job.Folders {
			names = append(names, f.Name)
		}
	}
	s.log.Warn("shutting down: dropped queued promotes; their folders stay in Staging", "jobs", len(s.queue), "folders", names)
	s.queue = nil
	s.reportLocked(true)
}

// promoteWorker runs queued jobs one after another until the queue is
// empty or the service is closing.
func (s *Service) promoteWorker() {
	defer s.wg.Done()
	for {
		s.pmu.Lock()
		if len(s.queue) == 0 || s.bgCtx.Err() != nil {
			s.working = false
			s.pmu.Unlock()
			return
		}
		qj := s.queue[0]
		s.queue = s.queue[1:]
		s.running = qj
		qj.job.State, qj.job.StartedAt = PromoteRunning, s.now()
		s.reportLocked(true)
		s.pmu.Unlock()
		s.runPromote(qj)
	}
}

// updateJob changes the running job under the queue's lock and reports
// (throttled unless force).
func (s *Service) updateJob(force bool, fn func(*PromoteJob)) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	fn(s.running.job)
	s.reportLocked(force)
}

func (s *Service) reportLocked(force bool) {
	now := time.Now() // the real clock: tests fix s.now
	if !force && now.Sub(s.lastReport) < promoteReportEvery {
		return
	}
	s.lastReport = now
	if s.promoteFn != nil {
		s.promoteFn(s.queueLocked())
	}
}

func (s *Service) runPromote(qj *queuedJob) {
	ctx := s.bgCtx
	failed := false
	for i, row := range qj.rows {
		err := ctx.Err()
		if err == nil {
			s.updateJob(true, func(j *PromoteJob) { j.Folders[i].State = PromoteMoving })
			err = s.promoteFolder(ctx, i, row, s.targetOf(i))
		}
		qj.releases[i]()
		s.changed()
		if err != nil {
			failed = true
			if ctx.Err() != nil {
				err = fmt.Errorf("not finished: SD2NAS is shutting down (%w)", err)
			}
			s.log.Error("promote failed", "folder", row.Name, "err", err)
		}
		s.updateJob(true, func(j *PromoteJob) {
			if err != nil {
				j.Folders[i].State, j.Folders[i].Error = PromoteFailed, err.Error()
			} else {
				j.Folders[i].State = PromoteDone
			}
		})
	}
	s.pmu.Lock()
	defer s.pmu.Unlock()
	j := qj.job
	j.State = PromoteDone
	if failed {
		j.State = PromoteFailed
	}
	j.FinishedAt = s.now()
	s.last, s.running = j, nil
	s.reportLocked(true)
}

func (s *Service) targetOf(i int) string {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	return s.running.job.Folders[i].Target
}

// promoteFolder moves folder row to target (a rel_path under Archive/): one
// rename when target doesn't exist, else file by file (Decision 3). Files
// move on ctx, so shutdown stops between files; the catalog is written on
// a context that isn't cancelled, so a moved file is always recorded.
func (s *Service) promoteFolder(ctx context.Context, i int, row catalog.StagingFolder, target string) error {
	dbctx := context.WithoutCancel(ctx)
	src := s.folderDir(row.Name)
	if _, err := os.Lstat(src); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s/%s is not on disk", Dir, row.Name)
		}
		return err
	}
	prefix := prefixOf(row.Name)
	assets, err := s.db.AssetsUnder(dbctx, prefix, catalog.StateStaged)
	if err != nil {
		return err
	}
	present, gone := map[string]catalog.Asset{}, map[string]catalog.Asset{}
	for _, a := range assets {
		switch _, err := os.Lstat(s.abs(a.RelPath)); {
		case err == nil:
			present[strings.TrimPrefix(a.RelPath, prefix)] = a
		case errors.Is(err, fs.ErrNotExist):
			gone[a.RelPath] = a
		default:
			return err
		}
	}
	// The archive index must only claim files it has (Decision 9).
	if err := s.cullGone(dbctx, gone); err != nil {
		return err
	}
	dst := s.abs(target)
	touched := map[string]bool{filepath.Join(s.photosRoot, Dir): true, filepath.Dir(dst): true}
	switch _, err := os.Lstat(dst); {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
			return err
		}
		err := s.rename(src, dst)
		if err == nil {
			n, err := s.db.ArchiveFolder(dbctx, row.ID, prefix, target+"/", s.now())
			if err != nil {
				return fmt.Errorf("moved to %s but could not record it: %w", target, err)
			}
			var bytes int64
			for _, a := range present {
				bytes += a.Size
			}
			s.updateJob(false, func(j *PromoteJob) { j.Folders[i].Done += n; j.FilesDone += n; j.BytesDone += bytes })
			return syncDirs(touched)
		}
		if !crossDevice(err) {
			return err
		}
		// moveFile creates the target folders as files arrive and checks the
		// context first, so a stop before the first file leaves nothing in
		// Archive/.
		s.updateJob(false, func(j *PromoteJob) { j.Folders[i].Copied = true })
	case err != nil:
		return err
	default:
		s.updateJob(false, func(j *PromoteJob) { j.Folders[i].Merged = true })
	}
	return s.promoteFiles(ctx, i, row, present, dst, touched)
}

// promoteFiles moves every file of row to the same path inside dst
// (moveTree: collision loop; EXDEV → copy, verify, delete), then removes
// the emptied staging folder and its row.
func (s *Service) promoteFiles(ctx context.Context, i int, row catalog.StagingFolder, present map[string]catalog.Asset, dst string, touched map[string]bool) error {
	dbctx := context.WithoutCancel(ctx)
	src := s.folderDir(row.Name)
	err := s.moveTree(ctx, src, dst, present, touched, func(a catalog.Asset, newRel string) error {
		if err := s.db.ArchiveAsset(dbctx, a.SHA256, newRel, s.now()); err != nil {
			return err
		}
		s.updateJob(false, func(j *PromoteJob) { j.Folders[i].Done++; j.FilesDone++; j.BytesDone += a.Size })
		return nil
	})
	if err != nil {
		return err
	}
	gone, kept, err := removeEmptyTree(src)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("%s/%s still holds %s", Dir, row.Name, strings.Join(kept, ", "))
	}
	if err := s.db.DeleteStagingFolder(dbctx, row.ID); err != nil {
		return err
	}
	return syncDirs(touched)
}

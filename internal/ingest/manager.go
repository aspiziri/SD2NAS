package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

var (
	ErrBadCardPath = errors.New("ingest: not a card folder")
	ErrQueueFull   = errors.New("ingest: too many imports waiting")
	ErrClosed      = errors.New("ingest: shutting down")
)

const MaxQueued = 8 // jobs waiting behind the running one

const stoppedBeforeStart = "SD2NAS stopped before this import started"

// ValidateCardPath resolves symlinks in cardsRoot and path, and returns the
// resolved path if it is an absolute path to a directory strictly inside
// cardsRoot.
func ValidateCardPath(cardsRoot, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s is not an absolute path", path)
	}
	root, err := filepath.EvalSymlinks(cardsRoot)
	if err != nil {
		return "", fmt.Errorf("resolve cards folder: %w", err)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s is not inside the cards folder %s", path, cardsRoot)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a folder", path)
	}
	return real, nil
}

// Manager runs ingests one at a time, in submission order.
type Manager struct {
	r         *Runner
	cardsRoot string
	ctx       context.Context // the running job's context; cancelled by Close
	cancel    context.CancelFunc
	wake      chan struct{} // capacity 1; Submit does a non-blocking send
	exited    chan struct{} // closed when the worker returns
	mu        sync.Mutex
	running   *Job
	queue     []*Job
	closed    bool
	started   bool
}

// Job is one submitted ingest.
type Job struct {
	path     string // resolved card path, for de-duplication
	done     chan struct{}
	progress *Progress
	mu       sync.Mutex
	row      catalog.Ingest
}

// NewManager returns a Manager that runs r's ingests for cards under
// cardsRoot. Call Recover, then Start.
func NewManager(r *Runner, cardsRoot string) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		r:         r,
		cardsRoot: cardsRoot,
		ctx:       ctx,
		cancel:    cancel,
		wake:      make(chan struct{}, 1),
		exited:    make(chan struct{}),
	}
}

func (m *Manager) stamp() time.Time { return m.r.now().Truncate(time.Millisecond) }

// Recover fails the ingests a crash left queued or running and removes
// leftover partial files from Staging. Call it before Start.
func (m *Manager) Recover(ctx context.Context) error {
	var errs []error
	n, err := m.r.cfg.Catalog.FailInterrupted(ctx, m.r.now())
	if err != nil {
		errs = append(errs, fmt.Errorf("fail interrupted ingests: %w", err))
	}
	if n != 0 {
		m.r.log().Warn("marked interrupted ingests failed", "count", n)
	}
	removed, err := CleanPartials(filepath.Join(m.r.cfg.PhotosRoot, StagingDir))
	for _, p := range removed {
		m.r.log().Info("removed partial file", "path", p)
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("clean partials: %w", err))
	}
	return errors.Join(errs...)
}

// Start starts the worker goroutine. It does nothing if already started
// or closed.
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.closed {
		return
	}
	m.started = true
	go m.worker()
}

// Submit queues an ingest of req.Path. Submitting a card that is already
// running or queued returns that job.
func (m *Manager) Submit(ctx context.Context, req Request) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	path, err := ValidateCardPath(m.cardsRoot, req.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCardPath, err)
	}
	if m.running != nil && m.running.path == path {
		return m.running, nil
	}
	for _, j := range m.queue {
		if j.path == path {
			return j, nil
		}
	}
	if len(m.queue) >= MaxQueued {
		return nil, ErrQueueFull
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = filepath.Base(path)
	}
	row, err := m.r.cfg.Catalog.CreateIngest(ctx, path, label, req.UUID, m.stamp())
	if err != nil {
		return nil, fmt.Errorf("record ingest: %w", err)
	}
	j := &Job{path: path, done: make(chan struct{}), progress: NewProgress(), row: row}
	m.queue = append(m.queue, j)
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return j, nil
}

// Current returns the running job (nil if none) and the queued ones.
func (m *Manager) Current() (running *Job, queued []*Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running, append([]*Job(nil), m.queue...)
}

// Close cancels the running job and the queued ones, and waits for the
// worker or ctx.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	queued := m.queue
	m.queue = nil
	started := m.started
	m.mu.Unlock()

	m.cancel()
	var errs []error
	for _, j := range queued {
		row := j.Result()
		row.Status = catalog.StatusCancelled
		row.Error = stoppedBeforeStart
		row.FinishedAt = m.stamp()
		// Not ctx-cancellable work for the row itself: the job must end up recorded.
		if err := m.r.cfg.Catalog.FinishIngest(context.WithoutCancel(ctx), row); err != nil {
			errs = append(errs, fmt.Errorf("cancel queued ingest %d: %w", row.ID, err))
		}
		j.finish(row)
	}
	if started {
		select {
		case <-m.exited:
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) worker() {
	defer close(m.exited)
	for {
		j := m.next()
		if j == nil {
			return
		}
		m.run(j)
	}
}

// next pops the head of the queue, or returns nil once closed.
func (m *Manager) next() *Job {
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil
		}
		if len(m.queue) > 0 {
			j := m.queue[0]
			m.queue = m.queue[1:]
			m.running = j
			m.mu.Unlock()
			return j
		}
		m.mu.Unlock()
		select {
		case <-m.wake:
		case <-m.ctx.Done():
			return nil
		}
	}
}

func (m *Manager) run(j *Job) {
	row := j.Result()
	row.Status = catalog.StatusRunning
	row.StartedAt = m.stamp()
	j.set(row)
	out, err := m.r.Run(m.ctx, row, j.progress)
	if err != nil {
		m.r.log().Error("ingest failed", "ingest", row.ID, "error", err)
		out = row
		out.Status = catalog.StatusFailed
		out.Error = err.Error()
		out.FinishedAt = m.stamp()
	}
	m.mu.Lock()
	m.running = nil
	j.finish(out)
	m.mu.Unlock()
}

func (j *Job) set(row catalog.Ingest) {
	j.mu.Lock()
	j.row = row
	j.mu.Unlock()
}

// finish stores the final row and closes done.
func (j *Job) finish(row catalog.Ingest) {
	j.set(row)
	close(j.done)
}

// ID returns the ingest's catalog id.
func (j *Job) ID() int64 { return j.Result().ID }

// Done is closed when the job has finished.
func (j *Job) Done() <-chan struct{} { return j.done }

// Result returns the final row once Done is closed; before that, the row
// as last known.
func (j *Job) Result() catalog.Ingest {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.row
}

// Progress returns a snapshot of the job's progress.
func (j *Job) Progress() ProgressSnapshot { return j.progress.Snapshot() }

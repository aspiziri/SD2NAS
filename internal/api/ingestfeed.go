package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/ingest"
)

// DefaultFeedInterval is how often the ingest feed looks at the manager;
// progress reaches /api/events clients at most this often.
const DefaultFeedInterval = 500 * time.Millisecond

// currentBody is the body of GET /api/ingest/current and the data of the
// ingest.current event.
type currentBody struct {
	Running *ingestView  `json:"running"`
	Queued  []ingestView `json:"queued"`
}

func currentOf(running *ingest.Job, queued []*ingest.Job) currentBody {
	out := currentBody{Queued: []ingestView{}}
	if running != nil {
		v := viewOf(running.Result(), running)
		out.Running = &v
	}
	for _, j := range queued {
		out.Queued = append(out.Queued, viewOf(j.Result(), j))
	}
	return out
}

// ingestFeed turns the manager's state into events. It polls instead of
// hooking into internal/ingest (Decision 5).
type ingestFeed struct {
	m       *ingest.Manager
	hub     *Hub
	log     *slog.Logger
	hooks   func() []func(catalog.Ingest)
	mu      sync.Mutex // serialises tick and watch
	watched map[int64]*ingest.Job
	last    []byte // the last ingest.current body published
}

// watch makes sure j gets an ingest.finished event, even if it finishes
// before a tick sees it running.
func (f *ingestFeed) watch(j *ingest.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watched[j.ID()] = j
}

func (f *ingestFeed) snapshot() (string, any, error) {
	running, queued := f.m.Current()
	return EventIngestCurrent, currentOf(running, queued), nil
}

// tick publishes ingest.current if it changed, then ingest.finished (and
// runs the finished hooks) for every watched job that has finished, oldest
// first. The state is read and published under f.mu, so the last
// ingest.current a client sees is never older than the previous tick's.
func (f *ingestFeed) tick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	running, queued := f.m.Current()
	for _, j := range queued {
		f.watched[j.ID()] = j
	}
	if running != nil {
		f.watched[running.ID()] = running
	}
	var finished []*ingest.Job
	for id, j := range f.watched {
		select {
		case <-j.Done():
			finished = append(finished, j)
			delete(f.watched, id)
		default:
		}
	}
	slices.SortFunc(finished, func(a, b *ingest.Job) int { return cmp.Compare(a.ID(), b.ID()) })

	b, err := json.Marshal(currentOf(running, queued))
	if err != nil {
		f.log.Error("encode ingest.current", "err", err)
	} else if !bytes.Equal(b, f.last) {
		f.last = b
		if err := f.hub.Publish(EventIngestCurrent, json.RawMessage(b)); err != nil {
			f.log.Error("publish ingest.current", "err", err)
		}
	}
	for _, j := range finished {
		row := j.Result()
		if err := f.hub.Publish(EventIngestFinished, viewOf(row, nil)); err != nil {
			f.log.Error("publish ingest.finished", "ingest", row.ID, "err", err)
		}
		for _, fn := range f.hooks() {
			fn(row)
		}
	}
}

func (f *ingestFeed) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.tick()
		}
	}
}

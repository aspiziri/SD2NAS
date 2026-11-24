package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/ingest"
)

// DefaultMaxWait is the ?wait=1 ceiling. The card script's curl --max-time 7260
// waits 60 s longer, so this reply reaches it first.
const DefaultMaxWait = 2 * time.Hour

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) // the client hung up; nothing useful to do with the error
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeText writes the two lines the card script turns into a notification.
func writeText(w http.ResponseWriter, code int, level, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, "%s\n%s\n", level, msg) // the client hung up
}

// wantsText reports whether the client asked for the card script's two-line
// replies (?format=text).
func wantsText(r *http.Request) bool { return r.URL.Query().Get("format") == "text" }

// writeFail answers a request that failed before an ingest could report on
// it: the two lines "alert" and msg (control characters replaced by spaces)
// when r asked for ?format=text, else a JSON error. Either way with code, so
// curl -f fails and the card stays mounted.
func writeFail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if wantsText(r) {
		line := strings.TrimSpace(strings.Map(func(c rune) rune {
			if unicode.IsControl(c) {
				return ' '
			}
			return c
		}, msg))
		writeText(w, code, ingest.LevelAlert, line)
		return
	}
	writeError(w, code, msg)
}

type ingestHandlers struct {
	m       *ingest.Manager
	db      *catalog.DB
	maxWait time.Duration
	feed    *ingestFeed
	submit  func(context.Context, ingest.Request) (*ingest.Job, error)
}

// MountIngest registers the ingest endpoints. Call it before serving.
func (s *Server) MountIngest(m *ingest.Manager, db *catalog.DB, maxWait time.Duration) {
	f := &ingestFeed{m: m, hub: s.hub, log: s.log, hooks: s.finishedHooks, watched: map[int64]*ingest.Job{}}
	s.feed = f
	s.hub.AddSnapshot(f.snapshot)
	h := &ingestHandlers{m: m, db: db, maxWait: maxWait, feed: f, submit: s.SubmitIngest}
	s.mux.HandleFunc("POST /api/ingest", h.post)
	s.mux.HandleFunc("GET /api/ingest/current", h.current)
	s.mux.HandleFunc("GET /api/ingests", h.list)
	s.mux.HandleFunc("GET /api/ingests/{id}", h.detail)
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		f.run(s.bgCtx, s.feedInterval)
	}()
}

type ingestView struct {
	catalog.Ingest
	Level        string                   `json:"level"`
	Message      string                   `json:"message"`
	Progress     *ingest.ProgressSnapshot `json:"progress,omitempty"`      // while queued or running
	Files        []catalog.IngestFile     `json:"files,omitempty"`         // detail only: every outcome except new
	ClockDetails []catalog.ClockWarning   `json:"clock_details,omitempty"` // detail only
}

func viewOf(row catalog.Ingest, job *ingest.Job) ingestView {
	v := ingestView{Ingest: row}
	v.Level, v.Message = ingest.Describe(row)
	if job != nil && !row.Status.Terminal() {
		p := job.Progress()
		v.Progress = &p
	}
	return v
}

func (h *ingestHandlers) respond(w http.ResponseWriter, code int, job *ingest.Job, text bool) {
	v := viewOf(job.Result(), job)
	if text {
		writeText(w, code, v.Level, v.Message)
		return
	}
	writeJSON(w, code, v)
}

func (h *ingestHandlers) post(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	text := wantsText(r)
	wait := false
	if q.Has("wait") {
		switch v := q.Get("wait"); v {
		case "1", "true":
			wait = true
		case "0", "false":
		default:
			writeFail(w, r, http.StatusBadRequest, fmt.Sprintf("wait must be 1, true, 0 or false, not %q", v))
			return
		}
	}
	var body struct {
		Path  string `json:"path"`
		Label string `json:"label"`
		UUID  string `json:"uuid"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeFail(w, r, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	if body.Path == "" {
		writeFail(w, r, http.StatusBadRequest, "path is required")
		return
	}
	job, err := h.submit(r.Context(), ingest.Request{Path: body.Path, Label: body.Label, UUID: body.UUID})
	switch {
	case errors.Is(err, ingest.ErrBadCardPath):
		writeFail(w, r, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, ingest.ErrQueueFull), errors.Is(err, ingest.ErrClosed):
		writeFail(w, r, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		writeFail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	if !wait {
		h.respond(w, http.StatusAccepted, job, text)
		return
	}
	timer := time.NewTimer(h.maxWait)
	defer timer.Stop()
	select {
	case <-job.Done():
		h.respond(w, finishedCode(job.Result().Status), job, text)
	case <-timer.C:
		msg := fmt.Sprintf("%s: still importing after %s. Card left mounted.", ingest.CardName(job.Result()), h.maxWait)
		if text {
			writeText(w, http.StatusGatewayTimeout, ingest.LevelAlert, msg)
		} else {
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": msg, "id": job.ID()})
		}
	case <-r.Context().Done():
		// The client hung up; the job carries on.
	}
}

// finishedCode: anything that must leave the card mounted is non-2xx (curl -f fails).
func finishedCode(s catalog.IngestStatus) int {
	switch s {
	case catalog.StatusDone:
		return http.StatusOK
	case catalog.StatusRefused:
		return http.StatusInsufficientStorage
	case catalog.StatusCancelled:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func (h *ingestHandlers) current(w http.ResponseWriter, r *http.Request) {
	running, queued := h.m.Current()
	writeJSON(w, http.StatusOK, currentOf(running, queued))
}

// positiveInt parses a query or path value that must be a positive integer.
func positiveInt(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a positive integer", s)
	}
	return n, nil
}

// liveJobs maps the ids of the running and queued jobs to the jobs.
func (h *ingestHandlers) liveJobs() map[int64]*ingest.Job {
	running, queued := h.m.Current()
	live := map[int64]*ingest.Job{}
	for _, j := range append(queued, running) {
		if j != nil {
			live[j.ID()] = j
		}
	}
	return live
}

func (h *ingestHandlers) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var limit, before int64
	for _, p := range []struct {
		name string
		dst  *int64
	}{{"limit", &limit}, {"before", &before}} {
		if !q.Has(p.name) {
			continue
		}
		n, err := positiveInt(q.Get(p.name))
		if err != nil {
			writeError(w, http.StatusBadRequest, p.name+": "+err.Error())
			return
		}
		*p.dst = n
	}
	rows, err := h.db.Ingests(r.Context(), int(min(limit, 1000)), before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	live := h.liveJobs()
	views := make([]ingestView, 0, len(rows))
	for _, row := range rows {
		views = append(views, viewOf(row, live[row.ID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ingests": views})
}

func (h *ingestHandlers) detail(w http.ResponseWriter, r *http.Request) {
	id, err := positiveInt(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "id: "+err.Error())
		return
	}
	row, err := h.db.Ingest(r.Context(), id)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no ingest %d", id))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	files, err := h.db.IngestFiles(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	warnings, err := h.db.ClockWarnings(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	v := viewOf(row, h.liveJobs()[id])
	for _, f := range files {
		if f.Outcome != catalog.OutcomeNew {
			v.Files = append(v.Files, f)
		}
	}
	v.ClockDetails = warnings
	writeJSON(w, http.StatusOK, v)
}

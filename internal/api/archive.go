package api

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/aspiziri/SD2NAS/internal/indexer"
)

type archiveHandlers struct {
	idx *indexer.Service
	log *slog.Logger
}

// MountArchive registers /api/archive/* and the indexer.progress event, and
// makes Close close idx. Call it before serving.
func (s *Server) MountArchive(idx *indexer.Service) {
	h := &archiveHandlers{idx: idx, log: s.log}
	s.mux.HandleFunc("GET /api/archive/stats", h.stats)
	s.mux.HandleFunc("POST /api/archive/rescan", h.rescan)
	s.mux.HandleFunc("POST /api/archive/pause", h.pause)
	s.mux.HandleFunc("POST /api/archive/resume", h.resume)
	s.mux.HandleFunc("GET /api/archive/changed", h.changed)
	s.mux.HandleFunc("POST /api/archive/changed/accept", h.accept)
	// The snapshot runs outside the hub's lock, and the OnChange callback
	// (which runs under the indexer's lock) only publishes.
	s.hub.AddSnapshot(func() (string, any, error) { return EventIndexerProgress, idx.Status(), nil })
	idx.OnChange(func(st indexer.Status) {
		if err := s.hub.Publish(EventIndexerProgress, st); err != nil {
			s.log.Error("publish indexer.progress", "err", err)
		}
	})
	s.closers = append(s.closers, idx.Close)
}

// fail answers an indexer error: 503 after shutdown, 409 when no scan runs,
// else a logged 500.
func (h *archiveHandlers) fail(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, indexer.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, "SD2NAS is shutting down")
	case errors.Is(err, indexer.ErrNotRunning):
		writeJSON(w, http.StatusConflict, opError{Error: "No scan is running.", Code: "not_running"})
	default:
		h.log.Error(what, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func (h *archiveHandlers) stats(w http.ResponseWriter, r *http.Request) {
	st, err := h.idx.Stats(r.Context())
	if err != nil {
		h.fail(w, "archive stats", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *archiveHandlers) rescan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Full bool `json:"full"`
	}
	empty := r.ContentLength == 0
	if !empty {
		br := bufio.NewReader(http.MaxBytesReader(w, r.Body, smallBody))
		if _, err := br.Peek(1); errors.Is(err, io.EOF) {
			empty = true
		} else {
			r.Body = io.NopCloser(br)
		}
	}
	if !empty {
		if err := decodeBody(w, r, &body, smallBody); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": h.idx.StartRescan(body.Full)})
}

func (h *archiveHandlers) pause(w http.ResponseWriter, r *http.Request) {
	st, err := h.idx.Pause()
	if err != nil {
		h.fail(w, "pause archive scan", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *archiveHandlers) resume(w http.ResponseWriter, r *http.Request) {
	st, err := h.idx.Resume()
	if err != nil {
		h.fail(w, "resume archive scan", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *archiveHandlers) changed(w http.ResponseWriter, r *http.Request) {
	cl, err := h.idx.Changes(r.Context())
	if err != nil {
		h.fail(w, "list archive changes", err)
		return
	}
	writeJSON(w, http.StatusOK, cl)
}

func (h *archiveHandlers) accept(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UpTo *int64 `json:"upTo"`
	}
	if err := decodeBody(w, r, &body, smallBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.UpTo == nil || *body.UpTo < 1 {
		writeError(w, http.StatusBadRequest, "upTo must be a positive whole number.")
		return
	}
	n, err := h.idx.Accept(opContext(r), *body.UpTo)
	if err != nil {
		h.fail(w, "accept archive changes", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"accepted": n})
}

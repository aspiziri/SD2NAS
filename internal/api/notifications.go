package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/aspiziri/SD2NAS/internal/notices"
)

type notificationHandlers struct {
	n *notices.Service
	s *Server
}

// MountNotifications registers GET /api/notifications and
// POST /api/notifications/take, tells n about every saved settings change,
// and makes Close close n. Call it before serving.
func (s *Server) MountNotifications(n *notices.Service) {
	h := &notificationHandlers{n: n, s: s}
	s.mux.HandleFunc("GET /api/notifications", h.pending)
	s.mux.HandleFunc("POST /api/notifications/take", h.take)
	s.onSettingsChanged(n.SettingsChanged) // never blocks
	// Notices close first, before the services registered earlier, so a late
	// tick never reads a closed service.
	s.closers = append([]func(){n.Close}, s.closers...)
}

// fail answers a failed Pending or Take: 503 once closed, else 500 and a log line.
func (h *notificationHandlers) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	if errors.Is(err, notices.ErrClosed) {
		writeFail(w, r, http.StatusServiceUnavailable, "SD2NAS is shutting down")
		return
	}
	h.s.log.Error(what, "err", err)
	writeFail(w, r, http.StatusInternalServerError, err.Error())
}

func (h *notificationHandlers) pending(w http.ResponseWriter, r *http.Request) {
	list, err := h.n.Pending(r.Context())
	if err != nil {
		h.fail(w, r, "list notices", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": list})
}

func (h *notificationHandlers) take(w http.ResponseWriter, r *http.Request) {
	list, err := h.n.Take(r.Context())
	if err != nil {
		h.fail(w, r, "take notices", err)
		return
	}
	if !wantsText(r) {
		writeJSON(w, http.StatusOK, map[string]any{"notifications": list})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	for _, n := range list {
		_, _ = fmt.Fprintf(w, "%s\n%s\n", n.Level, n.Message) // the client hung up
	}
}

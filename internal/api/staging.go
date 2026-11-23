package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/staging"
)

type stagingHandlers struct {
	svc *staging.Service
	log *slog.Logger
}

// MountStaging registers the read-only staging endpoints. Call it before serving.
func (s *Server) MountStaging(svc *staging.Service) {
	h := &stagingHandlers{svc: svc, log: s.log}
	s.mux.HandleFunc("GET /api/staging", h.list)
	s.mux.HandleFunc("GET /api/staging/{id}/files", h.files)
}

func (h *stagingHandlers) list(w http.ResponseWriter, r *http.Request) {
	l, err := h.svc.List(r.Context())
	if err != nil {
		h.log.Error("list staging", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (h *stagingHandlers) files(w http.ResponseWriter, r *http.Request) {
	id, err := positiveInt(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "id: "+err.Error())
		return
	}
	ff, err := h.svc.Files(r.Context(), id, r.URL.Query().Get("kind"))
	switch {
	case errors.Is(err, staging.ErrBadKind):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, fmt.Sprintf("no staging folder %d", id))
	case err != nil:
		h.log.Error("list staging files", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, ff)
	}
}

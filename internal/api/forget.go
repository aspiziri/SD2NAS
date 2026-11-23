package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/ingest"
	"github.com/aspiziri/SD2NAS/internal/staging"
)

type forgetHandlers struct {
	m    *ingest.Manager
	db   *catalog.DB
	svc  *staging.Service
	feed *ingestFeed // MountIngest's; nil when it wasn't mounted
	log  *slog.Logger
}

// MountForget registers "Forget this import" and "Re-import anyway" (§8,
// §10, Decisions 10 and 11). Call it after MountIngest.
func (s *Server) MountForget(m *ingest.Manager, db *catalog.DB, svc *staging.Service) {
	h := &forgetHandlers{m: m, db: db, svc: svc, feed: s.feed, log: s.log}
	s.mux.HandleFunc("GET /api/ingest/{id}/forget", h.summary)
	s.mux.HandleFunc("POST /api/ingest/{id}/forget", h.forget)
	s.mux.HandleFunc("POST /api/ingest/{id}/reimport-culled", h.reimport)
}

func (h *forgetHandlers) id(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := positiveInt(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "The import id must be a positive whole number.")
		return 0, false
	}
	return id, true
}

func (h *forgetHandlers) summary(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	sum, err := h.svc.ForgetSummary(r.Context(), id)
	if err != nil {
		writeOpError(w, h.log, "import", "forget summary", err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (h *forgetHandlers) forget(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	var body struct {
		DeleteFiles *bool `json:"deleteFiles"`
	}
	if err := decodeBody(w, r, &body, smallBody); err != nil {
		writeError(w, http.StatusBadRequest, "The request body is not valid JSON.")
		return
	}
	if body.DeleteFiles == nil {
		writeError(w, http.StatusBadRequest, "Say whether to delete the files: deleteFiles is required.")
		return
	}
	release, err := h.m.Exclusive()
	if err != nil {
		writeOpError(w, h.log, "import", "forget import", err)
		return
	}
	defer release()
	ctx := opContext(r)
	res, err := h.svc.Forget(ctx, id, *body.DeleteFiles)
	if err != nil {
		writeOpError(w, h.log, "import", "forget import", err)
		return
	}
	row, err := h.db.Ingest(ctx, id)
	if err != nil {
		writeOpError(w, h.log, "import", "forget import", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res, "ingest": viewOf(row, nil)})
}

func (h *forgetHandlers) reimport(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	release, err := h.m.Exclusive()
	if err != nil {
		writeOpError(w, h.log, "import", "re-import culled files", err)
		return
	}
	ctx := opContext(r)
	n, err := h.svc.ForgetCulled(ctx, id)
	release() // before Submit, so the new import can start
	if err != nil {
		writeOpError(w, h.log, "import", "re-import culled files", err)
		return
	}
	row, err := h.db.Ingest(ctx, id)
	if err != nil {
		writeOpError(w, h.log, "import", "re-import culled files", err)
		return
	}
	name := ingest.CardName(row)
	resp := map[string]any{"forgotten": n, "started": nil}
	if n == 0 {
		resp["message"] = "Nothing to re-import: none of this import's skipped files is still marked as culled."
		writeJSON(w, http.StatusOK, resp)
		return
	}
	job, err := h.m.Submit(ctx, ingest.Request{Path: row.CardPath, Label: row.CardLabel, UUID: row.CardUUID})
	switch {
	case err == nil:
		if h.feed != nil {
			h.feed.watch(job)
		}
		v := viewOf(job.Result(), job)
		resp["started"] = v
		resp["message"] = fmt.Sprintf("Importing %d %s again from %s.", n, plural(n, "file", "files"), name)
	case errors.Is(err, ingest.ErrBadCardPath):
		resp["message"] = fmt.Sprintf("%d %s will be imported again the next time you insert %s.", n, plural(n, "file", "files"), name)
	default:
		h.log.Error("start re-import", "ingest", id, "err", err)
		resp["message"] = fmt.Sprintf("%d %s will be imported again the next time you insert %s (starting an import now failed: %v).",
			n, plural(n, "file", "files"), name, err)
	}
	writeJSON(w, http.StatusOK, resp)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

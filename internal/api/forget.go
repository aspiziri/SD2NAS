package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/ingest"
	"github.com/aspiziri/SD2NAS/internal/staging"
)

type forgetHandlers struct {
	m   *ingest.Manager
	db  *catalog.DB
	svc *staging.Service
	// submit queues an import and makes the ingest feed watch it
	// (Server.SubmitIngest, the one submit + watch path).
	submit func(context.Context, ingest.Request) (*ingest.Job, error)
	log    *slog.Logger
}

// MountForget registers "Forget this import", "Re-import anyway" and
// "Re-import missing files" (§8, §10, Decisions 10 and 11, M7). Call it
// after MountIngest.
func (s *Server) MountForget(m *ingest.Manager, db *catalog.DB, svc *staging.Service) {
	h := &forgetHandlers{m: m, db: db, svc: svc, submit: s.SubmitIngest, log: s.log}
	s.mux.HandleFunc("GET /api/ingest/{id}/forget", h.summary)
	s.mux.HandleFunc("POST /api/ingest/{id}/forget", h.forget)
	s.mux.HandleFunc("POST /api/ingest/{id}/reimport-culled", h.reimport)
	s.mux.HandleFunc("POST /api/ingest/{id}/reimport-missing", h.reimportMissing)
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
		writeError(w, http.StatusBadRequest, err.Error())
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
	resp := map[string]any{"forgotten": n, "started": nil}
	if n == 0 {
		resp["message"] = "Nothing to re-import: none of this import's skipped files is still marked as culled."
		writeJSON(w, http.StatusOK, resp)
		return
	}
	h.startAgain(ctx, w, row, n, resp)
}

// reimportMissing asks the next import of this import's card to copy again
// the files whose archive copy is still missing (a restore request, kept
// in the catalog until honoured), and starts that import now when the card
// is mounted.
func (h *forgetHandlers) reimportMissing(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	ctx := opContext(r)
	row, err := h.db.Ingest(ctx, id)
	if err != nil {
		writeOpError(w, h.log, "import", "re-import missing files", err)
		return
	}
	n, err := h.db.RequestRestore(ctx, id, time.Now())
	if err != nil {
		writeOpError(w, h.log, "import", "re-import missing files", err)
		return
	}
	resp := map[string]any{"requested": n, "started": nil}
	if n == 0 {
		resp["message"] = "Nothing to re-import: none of this import's files is still missing from the archive."
		writeJSON(w, http.StatusOK, resp)
		return
	}
	h.startAgain(ctx, w, row, n, resp)
}

// startAgain submits an import of row's card, which will copy n of its
// files again, and answers with resp plus "started" and "message".
func (h *forgetHandlers) startAgain(ctx context.Context, w http.ResponseWriter, row catalog.Ingest, n int, resp map[string]any) {
	name := ingest.CardName(row)
	job, err := h.submit(ctx, ingest.Request{Path: row.CardPath, Label: row.CardLabel, UUID: row.CardUUID})
	switch {
	case err == nil:
		resp["started"] = viewOf(job.Result(), job)
		resp["message"] = fmt.Sprintf("Importing %d %s again from %s.", n, plural(n, "file", "files"), name)
	case errors.Is(err, ingest.ErrBadCardPath):
		resp["message"] = fmt.Sprintf("%d %s will be imported again the next time you insert %s.", n, plural(n, "file", "files"), name)
	default:
		h.log.Error("start re-import", "ingest", row.ID, "err", err)
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

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/ingest"
	"github.com/aspiziri/SD2NAS/internal/staging"
)

type stagingHandlers struct {
	svc *staging.Service
	log *slog.Logger
}

const (
	smallBody = 64 << 10 // rename, merge, promote, forget
	largeBody = 4 << 20  // move, cull, shift: up to staging.MaxFiles hashes
)

// MountStaging registers the staging endpoints and the staging.changed
// event, and makes Close close svc. Call it before serving.
func (s *Server) MountStaging(svc *staging.Service) {
	h := &stagingHandlers{svc: svc, log: s.log}
	s.mux.HandleFunc("GET /api/staging", h.list)
	s.mux.HandleFunc("GET /api/staging/{id}/files", h.files)
	s.mux.HandleFunc("PATCH /api/staging/{id}", h.rename)
	s.mux.HandleFunc("DELETE /api/staging/{id}", h.discard)
	s.mux.HandleFunc("POST /api/staging/merge", h.merge)
	s.mux.HandleFunc("POST /api/staging/move", h.move)
	s.mux.HandleFunc("POST /api/staging/cull", h.cull)
	s.mux.HandleFunc("POST /api/staging/shift", h.shift)
	s.mux.HandleFunc("POST /api/staging/promote", h.promote)
	s.mux.HandleFunc("GET /api/staging/promote", h.promoteStatus)
	s.mux.HandleFunc("DELETE /api/staging/promote/{jobId}", h.cancelPromote) // no clash: DELETE /api/staging/{id} has one segment fewer
	s.mux.HandleFunc("POST /api/staging/rescan", h.rescan)
	// The snapshot runs outside the hub's lock, and the OnChange callback
	// only publishes, so neither inverts the service's lock order.
	s.hub.AddSnapshot(func() (string, any, error) { return EventStagingChanged, svc.Status(), nil })
	svc.OnChange(func(st staging.Status) {
		if err := s.hub.Publish(EventStagingChanged, st); err != nil {
			s.log.Error("publish staging.changed", "err", err)
		}
	})
	s.hub.AddSnapshot(func() (string, any, error) { return EventPromoteProgress, svc.Promotes(), nil })
	svc.OnPromote(func(q staging.PromoteQueue) {
		if err := s.hub.Publish(EventPromoteProgress, q); err != nil {
			s.log.Error("publish promote.progress", "err", err)
		}
	})
	s.addFinishedHook(func(catalog.Ingest) { svc.Touch() }) // new files may have landed
	s.closers = append(s.closers, svc.Close)
}

// opError is the JSON body of a failed operation.
type opError struct {
	Error    string `json:"error"`
	Code     string `json:"code,omitempty"`
	FolderID int64  `json:"folder_id,omitempty"`
	Holder   string `json:"holder,omitempty"`
}

func busyMessage(b *staging.BusyError) string {
	name := b.Name
	if name == "" {
		name = fmt.Sprintf("Folder %d", b.ID)
	}
	switch b.Holder {
	case staging.HolderIngest:
		return name + " is being imported into. Try again when the import finishes."
	case staging.HolderRescan:
		return name + " is being rescanned. Try again in a moment."
	case staging.HolderPromote:
		if b.Queued {
			return name + " is waiting to be promoted."
		}
		return name + " is being promoted."
	}
	return name + " is busy with another change. Try again in a moment."
}

// plainReason turns a 400 error into a sentence for the UI: the text after
// the last staging sentinel ("staging: bad folder title: "), capitalised,
// with a full stop (pre-flight D8).
func plainReason(err error) string {
	msg := err.Error()
	for _, sentinel := range []error{staging.ErrBadTitle, staging.ErrBadPattern, staging.ErrBadRequest} {
		if i := strings.LastIndex(msg, sentinel.Error()+": "); i >= 0 {
			msg = msg[i+len(sentinel.Error())+2:]
		}
	}
	r, size := utf8.DecodeRuneInString(msg)
	msg = string(unicode.ToUpper(r)) + msg[size:]
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg
}

// writeOpError answers a failed staging operation on subject (folder,
// import) with a plain sentence; only unexpected failures are logged.
func writeOpError(w http.ResponseWriter, log *slog.Logger, subject, what string, err error) {
	var busy *staging.BusyError
	conflict := func(code, msg string) { writeJSON(w, http.StatusConflict, opError{Error: msg, Code: code}) }
	switch {
	case errors.As(err, &busy):
		writeJSON(w, http.StatusConflict, opError{Error: busyMessage(busy), Code: "folder_busy", FolderID: busy.ID, Holder: string(busy.Holder)})
	case errors.Is(err, catalog.ErrNameTaken):
		conflict("name_taken", "A folder with that name already exists in Staging. Pick another title.")
	case errors.Is(err, staging.ErrChanged):
		conflict("changed", "Some files moved while you were choosing. Reload and try again.")
	case errors.Is(err, staging.ErrJobStarted):
		conflict("promote_started", "That promote has already started or finished, so it can't be cancelled.")
	case errors.Is(err, staging.ErrIngestRunning), errors.Is(err, ingest.ErrBusy):
		conflict("ingest_running", "An import is running or waiting. Try again when it finishes.")
	case errors.Is(err, staging.ErrAlreadyForgotten):
		conflict("already_forgotten", "This import was already forgotten.")
	case errors.Is(err, staging.ErrNoSuchJob):
		writeError(w, http.StatusNotFound, "That promote no longer exists.")
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, "That "+subject+" no longer exists.")
	case errors.Is(err, staging.ErrBadTitle), errors.Is(err, staging.ErrBadPattern), errors.Is(err, staging.ErrBadRequest):
		writeError(w, http.StatusBadRequest, plainReason(err))
	case errors.Is(err, staging.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, "SD2NAS is shutting down")
	default:
		log.Error(what, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// decodeBody reads a JSON body of at most limit bytes into v, refusing
// unknown fields and trailing data. Its error is a plain sentence for the
// 400 answer (no Go error text).
func decodeBody(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			return errors.New("The request is too large.")
		case strings.HasPrefix(err.Error(), "json: unknown field"):
			return errors.New("The request has a field this server does not know.")
		}
		return errors.New("The request body is not valid JSON.")
	}
	if dec.More() {
		return errors.New("The request body must hold exactly one JSON value.")
	}
	return nil
}

// opContext keeps an operation running when the client goes away (a phone
// sleeping mid-merge must not leave it half done).
func opContext(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }

func (h *stagingHandlers) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := positiveInt(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "The folder id must be a positive whole number.")
		return 0, false
	}
	return id, true
}

func (h *stagingHandlers) rename(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Title *string `json:"title"`
		Name  *string `json:"name"` // the whole name; wins over title
	}
	if err := decodeBody(w, r, &body, smallBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var f staging.Folder
	var err error
	switch {
	case body.Name != nil:
		f, err = h.svc.RenameAs(opContext(r), id, *body.Name)
	case body.Title != nil:
		f, err = h.svc.Rename(opContext(r), id, *body.Title)
	default:
		writeError(w, http.StatusBadRequest, "Give a name, or a title (an empty one removes it).")
		return
	}
	if err != nil {
		writeOpError(w, h.log, "folder", "rename staging folder", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"folder": f})
}

func (h *stagingHandlers) discard(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	res, err := h.svc.Discard(opContext(r), id)
	if err != nil {
		writeOpError(w, h.log, "folder", "discard staging folder", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *stagingHandlers) merge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs   []int64 `json:"ids"`
		Title string  `json:"title"`
		Name  *string `json:"name"` // the whole name; wins over title
	}
	if err := decodeBody(w, r, &body, smallBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var f staging.Folder
	var err error
	if body.Name != nil {
		f, err = h.svc.MergeAs(opContext(r), body.IDs, *body.Name)
	} else {
		f, err = h.svc.Merge(opContext(r), body.IDs, body.Title)
	}
	if err != nil {
		writeOpError(w, h.log, "folder", "merge staging folders", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"folder": f})
}

func (h *stagingHandlers) move(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FileShas []string `json:"fileShas"`
		ToID     *int64   `json:"toId"`
		NewTitle *string  `json:"newTitle"`
		NewName  *string  `json:"newName"` // the new folder's whole name
	}
	if err := decodeBody(w, r, &body, largeBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	given := 0
	for _, p := range []bool{body.ToID != nil, body.NewTitle != nil, body.NewName != nil} {
		if p {
			given++
		}
	}
	if given != 1 {
		writeError(w, http.StatusBadRequest, "Give exactly one of toId, newTitle and newName.")
		return
	}
	req := staging.MoveRequest{SHAs: body.FileShas}
	if body.ToID != nil {
		if *body.ToID <= 0 {
			writeError(w, http.StatusBadRequest, "The destination folder id must be a positive whole number.")
			return
		}
		req.ToID = *body.ToID
	} else if body.NewName != nil {
		req.NewName = *body.NewName
		if strings.TrimSpace(req.NewName) == "" {
			writeError(w, http.StatusBadRequest, "Give the new folder a name.")
			return
		}
	} else {
		req.NewTitle = *body.NewTitle
		if strings.TrimSpace(req.NewTitle) == "" {
			writeError(w, http.StatusBadRequest, "Give the new folder a title.")
			return
		}
	}
	res, err := h.svc.Move(opContext(r), req)
	if err != nil {
		writeOpError(w, h.log, "folder", "move staged files", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *stagingHandlers) cull(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FileShas []string `json:"fileShas"`
	}
	if err := decodeBody(w, r, &body, largeBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := h.svc.Cull(opContext(r), body.FileShas)
	if err != nil {
		writeOpError(w, h.log, "folder", "cull staged files", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *stagingHandlers) shift(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FileShas []string `json:"fileShas"`
		Seconds  *int64   `json:"seconds"` // nil: Shift answers the "Say how far" sentence
	}
	if err := decodeBody(w, r, &body, largeBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := h.svc.Shift(opContext(r), staging.ShiftRequest{SHAs: body.FileShas, Seconds: body.Seconds})
	if err != nil {
		writeOpError(w, h.log, "folder", "shift staged files", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *stagingHandlers) promote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs     []int64 `json:"ids"`
		Pattern string  `json:"pattern"`
	}
	if err := decodeBody(w, r, &body, smallBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := h.svc.Promote(opContext(r), body.IDs, body.Pattern)
	if err != nil {
		writeOpError(w, h.log, "folder", "promote staging folders", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (h *stagingHandlers) promoteStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.svc.Promotes())
}

func (h *stagingHandlers) cancelPromote(w http.ResponseWriter, r *http.Request) {
	id, err := positiveInt(r.PathValue("jobId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "The promote id must be a positive whole number.")
		return
	}
	job, err := h.svc.CancelPromote(id)
	if err != nil {
		writeOpError(w, h.log, "promote", "cancel promote", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

func (h *stagingHandlers) rescan(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": h.svc.StartRescan()})
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
		writeError(w, http.StatusBadRequest, "The folder id must be a positive whole number.")
		return
	}
	ff, err := h.svc.Files(r.Context(), id, r.URL.Query().Get("kind"))
	switch {
	case errors.Is(err, staging.ErrBadKind):
		writeError(w, http.StatusBadRequest, "The kind must be jpg, raw or video.")
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, "That folder no longer exists.")
	case err != nil:
		h.log.Error("list staging files", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, ff)
	}
}

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/indexer"
)

type settingsHandlers struct {
	db         *catalog.DB
	idx        *indexer.Service
	photosRoot string
	log        *slog.Logger
	srv        *Server
}

// MountSettings registers GET/PUT /api/settings.
func (s *Server) MountSettings(db *catalog.DB, idx *indexer.Service, photosRoot string) {
	h := &settingsHandlers{db: db, idx: idx, photosRoot: photosRoot, log: s.log, srv: s}
	s.mux.HandleFunc("GET /api/settings", h.get)
	s.mux.HandleFunc("PUT /api/settings", h.put)
}

func (h *settingsHandlers) get(w http.ResponseWriter, r *http.Request) {
	h.write(w, r.Context())
}

// write answers with every setting's effective value.
func (h *settingsHandlers) write(w http.ResponseWriter, ctx context.Context) {
	set, err := h.db.Settings(ctx)
	if err != nil {
		h.log.Error("read settings", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := set.Strings()
	out[catalog.KeyBackupDir] = h.idx.BackupDir(set)
	defaults := catalog.DefaultSettings()
	def := defaults.Strings()
	def[catalog.KeyBackupDir] = h.idx.BackupDir(defaults)
	writeJSON(w, http.StatusOK, map[string]any{"settings": out, "defaults": def})
}

func (h *settingsHandlers) put(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(w, r, &body, smallBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "Name at least one setting.")
		return
	}
	known := catalog.SettingKeys()
	kv := make(map[string]string, len(body))
	for _, key := range slices.Sorted(maps.Keys(body)) {
		if !slices.Contains(known, key) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("Unknown setting %s.", key))
			return
		}
		v, ok := body[key].(string)
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("Setting %s must be a string.", key))
			return
		}
		if key == catalog.KeyBackupDir && v != "" && errors.Is(indexer.CheckBackupDir(h.photosRoot, v), indexer.ErrBackupInside) {
			writeError(w, http.StatusBadRequest, "Setting backup_dir: pick a folder outside Archive/ and Staging/.")
			return
		}
		kv[key] = v
	}
	if err := h.db.SetSettings(opContext(r), kv); err != nil {
		var se *catalog.SettingError
		if errors.As(err, &se) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("Setting %s: %s.", se.Key, se.Reason))
			return
		}
		h.log.Error("save settings", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.idx.SettingsChanged()
	for _, fn := range h.srv.settingsHooks() {
		fn()
	}
	h.write(w, context.WithoutCancel(r.Context()))
}

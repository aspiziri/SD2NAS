package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/thumbs"
)

// warmQueue is how many finished ingests may wait for their thumbnails.
const warmQueue = 16

var errNoFile = errors.New("this hash has no file in the photos share (culled or never stored)")

type thumbHandlers struct {
	db     *catalog.DB
	cache  *thumbs.Cache
	photos string
	log    *slog.Logger
	warm   chan int64 // ingest ids
}

// MountThumbs registers GET /api/thumbs/{sha} and GET /api/preview/{sha} and
// warms the thumbnails of each finished ingest's new files. The server owns
// cache from then on: Close closes it.
func (s *Server) MountThumbs(db *catalog.DB, cache *thumbs.Cache, photosRoot string) {
	h := &thumbHandlers{db: db, cache: cache, photos: photosRoot, log: s.log, warm: make(chan int64, warmQueue)}
	s.mux.HandleFunc("GET /api/thumbs/{sha}", h.serve(thumbs.ThumbSize))
	s.mux.HandleFunc("GET /api/preview/{sha}", h.serve(thumbs.PreviewSize))
	s.addFinishedHook(h.enqueue)
	s.closers = append(s.closers, cache.Close)
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		h.warmLoop(s.bgCtx)
	}()
}

// source resolves sha (following an alias) to the file to render. A
// rel_path that isn't local to the photos root is refused.
func (h *thumbHandlers) source(ctx context.Context, sha string) (thumbs.Source, error) {
	a, err := h.db.AssetBySHA(ctx, sha)
	if err != nil {
		return thumbs.Source{}, err
	}
	if a.RelPath == "" {
		return thumbs.Source{}, errNoFile
	}
	rel := filepath.FromSlash(a.RelPath)
	if !filepath.IsLocal(rel) {
		return thumbs.Source{}, fmt.Errorf("asset %s has a rel_path outside the photos share: %q", a.SHA256, a.RelPath)
	}
	return thumbs.Source{SHA256: a.SHA256, Path: filepath.Join(h.photos, rel), Kind: a.Kind}, nil
}

// thumbFailure maps a Cache.Get error to a response, and says whether it
// deserves an error log (an expected property of the file does not).
func thumbFailure(err error) (code int, msg string, logIt bool) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound, "the file is missing from the photos share: " + err.Error(), false
	case errors.Is(err, thumbs.ErrNoImage):
		return http.StatusNotFound, err.Error(), false
	case errors.Is(err, exec.ErrNotFound):
		// exiftool or ffmpeg isn't installed: /healthz already warns, so
		// don't log an error for every RAW or video tile.
		return http.StatusNotFound, "no thumbnail: " + err.Error(), false
	case errors.Is(err, thumbs.ErrTooLarge):
		// A property of the file, not a fault of the server.
		return http.StatusUnprocessableEntity, err.Error(), false
	case errors.Is(err, thumbs.ErrClosed), errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable, "SD2NAS is shutting down", false
	}
	return http.StatusInternalServerError, err.Error(), true
}

func (h *thumbHandlers) serve(size int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sha := r.PathValue("sha")
		if !thumbs.ValidSHA(sha) {
			writeError(w, http.StatusBadRequest, "sha must be 64 lowercase hex digits")
			return
		}
		src, err := h.source(r.Context(), sha)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, "no file with hash "+sha)
			return
		case errors.Is(err, errNoFile):
			writeError(w, http.StatusNotFound, err.Error())
			return
		case err != nil:
			h.log.Error("thumbnail source", "sha", sha, "err", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		path, err := h.cache.Get(r.Context(), src, size)
		if err != nil {
			if r.Context().Err() != nil {
				return // the client went away; there is nobody to answer
			}
			code, msg, logIt := thumbFailure(err)
			if logIt {
				h.log.Error("thumbnail failed", "sha", sha, "size", size, "path", src.Path, "err", err)
			} else if errors.Is(err, thumbs.ErrTooLarge) {
				h.log.Warn("thumbnail refused", "sha", sha, "path", src.Path, "err", err)
			}
			writeError(w, code, msg)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			h.log.Error("open thumbnail", "path", path, "err", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer func() {
			if err := f.Close(); err != nil {
				h.log.Warn("close thumbnail", "path", path, "err", err)
			}
		}()
		fi, err := f.Stat()
		if err != nil {
			h.log.Error("stat thumbnail", "path", path, "err", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // the URL is content-addressed
		w.Header().Set("ETag", fmt.Sprintf(`"%s-%d"`, sha, size))
		http.ServeContent(w, r, "", fi.ModTime(), f)
	}
}

// enqueue runs on the ingest feed goroutine and must not block.
func (h *thumbHandlers) enqueue(in catalog.Ingest) {
	if in.Counts.New == 0 {
		return
	}
	select {
	case h.warm <- in.ID:
	default:
		h.log.Warn("thumbnail warm-up queue is full; thumbnails will be made when first viewed", "ingest", in.ID)
	}
}

func (h *thumbHandlers) warmLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-h.warm:
			h.warmIngest(ctx, id)
		}
	}
}

// warmIngest makes the small thumbnail of each new file of ingest id, one
// at a time, so it never takes more than one render slot. A duplicate job
// is cheap: Cache.Get returns an existing file without rendering.
func (h *thumbHandlers) warmIngest(ctx context.Context, id int64) {
	files, err := h.db.IngestFiles(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			h.log.Error("thumbnail warm-up: read ingest files", "ingest", id, "err", err)
		}
		return
	}
	made := 0
	for _, f := range files {
		if f.Outcome != catalog.OutcomeNew || f.SHA256 == "" {
			continue
		}
		src, err := h.source(ctx, f.SHA256)
		if err == nil {
			_, err = h.cache.Get(ctx, src, thumbs.ThumbSize)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			h.log.Warn("thumbnail warm-up failed", "ingest", id, "sha", f.SHA256, "err", err)
			continue
		}
		made++
	}
	h.log.Info("thumbnails ready", "ingest", id, "count", made)
}

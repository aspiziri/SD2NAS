package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

const (
	cacheImmutable  = "public, max-age=31536000, immutable" // Vite's content-hashed assets/ files
	cacheRevalidate = "no-cache"                            // everything else: revalidate with the ETag
)

// contentTypes pins the types of the files Vite writes. Go would otherwise
// take them from the OS (the Windows registry, /etc/mime.types), which can
// map .js to application/javascript, and its table lacks .webmanifest.
var contentTypes = map[string]string{
	".js":          "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".webmanifest": "application/manifest+json",
}

type uiHandler struct {
	version  string
	files    fs.FS
	etags    map[string]string // file path → strong ETag, computed once at mount
	index    []byte            // index.html; nil when the UI isn't built
	problems func() []string   // the server's permanent problems (catalog, …)
}

// MountUI serves the single-page app in dist (web.Dist()). Files are served
// by path, and every other extension-less GET outside /api/ gets index.html,
// so client-side routes such as /staging/3 survive a reload. Without
// index.html, "/" shows a page saying the UI isn't built. It also answers
// every /api/ request no other route matches, whatever the method, with a
// JSON error. Call it before serving.
func (s *Server) MountUI(dist fs.FS) error {
	h := &uiHandler{version: s.version, files: dist, etags: map[string]string{},
		problems: func() []string { return s.problems }}
	err := fs.WalkDir(dist, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(dist, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		h.etags[p] = `"` + hex.EncodeToString(sum[:12]) + `"`
		if p == "index.html" {
			h.index = b
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("read embedded web UI: %w", err)
	}
	// Any method, not "GET /": the mux would answer a POST to an unknown
	// /api/ path with a plain-text 405, and "GET /" plus "/api/" conflict
	// (the mux panics). Every specific route still wins over "/".
	s.mux.Handle("/", h)
	return nil
}

func (h *uiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/") // "/" → ""
	if p == "api" || strings.HasPrefix(p, "api/") {
		if probs := h.problems(); len(probs) > 0 {
			writeError(w, http.StatusServiceUnavailable, "not available while SD2NAS is degraded: "+strings.Join(probs, "; "))
			return
		}
		writeError(w, http.StatusNotFound, "no such API endpoint: "+r.Method+" "+r.URL.Path)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if etag, ok := h.etags[p]; ok && !hidden(p) {
		h.serveFile(w, r, p, etag)
		return
	}
	if path.Ext(p) != "" { // a missing file, not a client-side route
		http.NotFound(w, r)
		return
	}
	if h.index == nil {
		h.notBuilt(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", cacheRevalidate)
	w.Header().Set("ETag", h.etags["index.html"])
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(h.index))
}

// hidden reports whether any element of p starts with a dot (.placeholder).
func hidden(p string) bool {
	for _, el := range strings.Split(p, "/") {
		if strings.HasPrefix(el, ".") {
			return true
		}
	}
	return false
}

func (h *uiHandler) serveFile(w http.ResponseWriter, r *http.Request, p, etag string) {
	f, err := h.files.Open(p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "open "+p+": "+err.Error())
		return
	}
	defer f.Close() // an embedded, read-only, in-memory file: Close cannot fail
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		writeError(w, http.StatusInternalServerError, p+" is not seekable")
		return
	}
	cache := cacheRevalidate
	if strings.HasPrefix(p, "assets/") {
		cache = cacheImmutable
	}
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("ETag", etag)
	if ct, ok := contentTypes[path.Ext(p)]; ok {
		w.Header().Set("Content-Type", ct) // ServeContent keeps a type that is already set
	}
	http.ServeContent(w, r, path.Base(p), time.Time{}, rs)
}

func (h *uiHandler) notBuilt(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The write fails only when the client has gone.
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>SD2NAS</title>
<style>body{font-family:system-ui,sans-serif;background:#141c27;color:#e6edf6;margin:0;padding:24px 16px}
a{color:#4f9cf9}code{background:#223047;padding:2px 6px;border-radius:4px}</style>
</head><body>
<h1>SD2NAS</h1>
<p>Version <code>%s</code>. The web UI is not built into this binary: run <code>npm run build</code> in <code>web/</code>, then rebuild sd2nas.</p>
<p><a href="/healthz">/healthz</a> shows the time zone, user and mount checks.</p>
</body></html>
`, h.version)
}

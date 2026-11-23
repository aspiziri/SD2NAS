// Package api serves the HTTP API and (from M3) the embedded web UI.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// PathInfo describes one of the mounted directories.
type PathInfo struct {
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Writable *bool  `json:"writable,omitempty"` // probed once at startup; nil = not checked
	Error    string `json:"error,omitempty"`
}

// Mount is a directory the server depends on.
type Mount struct {
	Name      string // "cards", "photos", "config"
	Path      string
	NeedWrite bool // probe writability at startup and fail health if not writable
}

// Server holds the HTTP handlers and the state /healthz reports on.
type Server struct {
	version  string
	started  time.Time
	mounts   []Mount
	writable map[string]PathInfo // startup write probes, keyed by mount name
	mux      *http.ServeMux
}

// New builds the server and probes writable mounts once. Probing on every
// health check would write to the array every 30 seconds.
func New(version string, mounts []Mount) *Server {
	s := &Server{
		version:  version,
		started:  time.Now(),
		mounts:   mounts,
		writable: map[string]PathInfo{},
		mux:      http.NewServeMux(),
	}
	for _, m := range mounts {
		if m.NeedWrite {
			s.writable[m.Name] = probeWrite(m.Path)
		}
	}
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// WriteProblems lists mounts that failed the startup write probe, for logging.
func (s *Server) WriteProblems() []string {
	var out []string
	for _, m := range s.mounts {
		if p, ok := s.writable[m.Name]; ok && (p.Writable == nil || !*p.Writable) {
			out = append(out, fmt.Sprintf("%s (%s): %s", m.Name, m.Path, p.Error))
		}
	}
	return out
}

type health struct {
	Status   string              `json:"status"` // ok | degraded
	Problems []string            `json:"problems,omitempty"`
	Warnings []string            `json:"warnings,omitempty"`
	Version  string              `json:"version"`
	Uptime   string              `json:"uptime"`
	TZ       string              `json:"tz"`
	Time     string              `json:"time"`
	UID      int                 `json:"uid"`
	GID      int                 `json:"gid"`
	Paths    map[string]PathInfo `json:"paths"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	h := health{
		Status:  "ok",
		Version: s.version,
		Uptime:  time.Since(s.started).Round(time.Second).String(),
		TZ:      os.Getenv("TZ"),
		Time:    time.Now().Format(time.RFC3339),
		UID:     os.Getuid(),
		GID:     os.Getgid(),
		Paths:   map[string]PathInfo{},
	}
	if h.TZ == "" {
		h.Warnings = append(h.Warnings, "TZ is not set: video and file times will be read as UTC")
	}
	for _, m := range s.mounts {
		info := PathInfo{Path: m.Path}
		fi, err := os.Stat(m.Path)
		switch {
		case err != nil:
			info.Error = err.Error()
		case !fi.IsDir():
			info.Error = "not a directory"
		default:
			info.Exists = true
		}
		if p, ok := s.writable[m.Name]; ok {
			info.Writable = p.Writable
			if info.Error == "" {
				info.Error = p.Error
			}
		}
		if !info.Exists {
			h.Problems = append(h.Problems, fmt.Sprintf("%s: %s is missing (%s)", m.Name, m.Path, info.Error))
		} else if info.Writable != nil && !*info.Writable {
			h.Problems = append(h.Problems, fmt.Sprintf("%s: %s is not writable as uid %d (%s)", m.Name, m.Path, h.UID, info.Error))
		}
		h.Paths[m.Name] = info
	}

	code := http.StatusOK
	if len(h.Problems) > 0 {
		h.Status = "degraded"
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(h) // the client hung up; nothing useful to do with the error
}

// handleIndex is a placeholder until the Svelte UI is embedded in M3.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>SD2NAS</title>
<style>body{font-family:system-ui,sans-serif;background:#16181d;color:#e6e6e6;margin:0;padding:24px 16px}
a{color:#7fb4ff}code{background:#24272e;padding:2px 6px;border-radius:4px}</style>
</head><body>
<h1>SD2NAS</h1>
<p>Version <code>%s</code>. The web UI arrives in a later milestone.</p>
<p><a href="/healthz">/healthz</a> shows the time zone, user and mount checks.</p>
</body></html>
`, s.version)
}

// probeWrite creates and removes a temporary file to confirm the directory is
// writable by the current user.
func probeWrite(dir string) PathInfo {
	info := PathInfo{Path: dir}
	ok := false
	info.Writable = &ok
	f, err := os.CreateTemp(dir, ".sd2nas-write-test-*")
	if err != nil {
		info.Error = err.Error()
		return info
	}
	name := f.Name()
	closeErr := f.Close()
	removeErr := os.Remove(name)
	if closeErr != nil {
		info.Error = closeErr.Error()
		return info
	}
	if removeErr != nil {
		info.Error = "created a test file but could not remove it: " + removeErr.Error()
		return info
	}
	ok = true
	return info
}

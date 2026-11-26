// Package api serves the HTTP API and (from M3) the embedded web UI.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
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
	handler  http.Handler // mux behind the token check, behind the request guard
	auth     auth         // SetToken; the zero value means no token
	problems []string     // permanent /healthz problems added after startup
	warnings []string     // permanent /healthz warnings added after startup

	log       *slog.Logger // SetLogger; slog.Default() until then
	hub       *Hub
	keepalive time.Duration   // DefaultKeepalive; tests shorten it
	bgCtx     context.Context // cancelled by Close; background goroutines stop on it
	bgCancel  context.CancelFunc
	bg        sync.WaitGroup // background goroutines (ingest feed, thumbnail warm-up)
	closers   []func()       // run by Close once bg has stopped (e.g. the thumbnail cache)
	closeOnce sync.Once

	feedInterval time.Duration // DefaultFeedInterval; set before MountIngest
	feed         *ingestFeed   // set by MountIngest
	hookMu       sync.Mutex
	finished     []func(catalog.Ingest)

	settingsMu  sync.Mutex
	settingsFns []func() // run after every saved settings change
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
	s.log = slog.Default()
	s.hub = NewHub()
	s.keepalive = DefaultKeepalive
	s.feedInterval = DefaultFeedInterval
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /api/events", s.handleEvents)
	s.mux.HandleFunc("GET /api/auth", s.handleAuth)
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/logout", s.handleLogout)
	// The guard goes first: a cross-site request is refused before it can
	// learn whether its credentials were right.
	s.handler = guard(s.requireToken(s.mux))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// SetLogger sets the logger for errors the server can only log (event
// snapshots, thumbnail failures). Call it before any Mount method.
func (s *Server) SetLogger(l *slog.Logger) { s.log = l }

// Close ends every /api/events stream, stops the server's background work
// and runs the registered closers (the thumbnail cache). Call it before
// http.Server.Shutdown, which would otherwise wait for the open streams.
// Calling it again does nothing.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.bgCancel()
		s.bg.Wait()
		s.hub.Close()
		for _, c := range s.closers {
			c()
		}
	})
}

// addFinishedHook registers fn to run, on the feed goroutine, for every
// ingest that finishes. fn must not block.
func (s *Server) addFinishedHook(fn func(catalog.Ingest)) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	s.finished = append(s.finished, fn)
}

func (s *Server) finishedHooks() []func(catalog.Ingest) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	return append([](func(catalog.Ingest))(nil), s.finished...)
}

// onSettingsChanged registers fn to run after every saved settings change.
// fn must not block.
func (s *Server) onSettingsChanged(fn func()) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	s.settingsFns = append(s.settingsFns, fn)
}

func (s *Server) settingsHooks() []func() {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	return append([]func(){}, s.settingsFns...)
}

// AddProblem adds a permanent /healthz problem (for example, the catalog
// failed to open). Call it before serving.
func (s *Server) AddProblem(p string) { s.problems = append(s.problems, p) }

// AddWarning adds a permanent /healthz warning (healthz stays 200). Call it
// before serving.
func (s *Server) AddWarning(w string) { s.warnings = append(s.warnings, w) }

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
	h.Problems = append(h.Problems, s.problems...)
	if h.TZ == "" {
		h.Warnings = append(h.Warnings, "TZ is not set: video and file times will be read as UTC")
	}
	h.Warnings = append(h.Warnings, s.warnings...)
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
	// A browser opening /healthz asks for HTML. Some (Edge on iOS, Samsung
	// Internet) offer to download application/json, so they get the same
	// JSON as plain text. The UI, the card script and curl get JSON.
	ct := "application/json"
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		ct = "text/plain; charset=utf-8"
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(h) // the client hung up; nothing useful to do with the error
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

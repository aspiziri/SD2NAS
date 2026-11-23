package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) health {
	t.Helper()
	var h health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return h
}

func TestHealthzOK(t *testing.T) {
	t.Setenv("TZ", "America/New_York")
	dir := t.TempDir()
	s := New("test", []Mount{
		{Name: "cards", Path: dir},
		{Name: "photos", Path: dir, NeedWrite: true},
		{Name: "config", Path: dir, NeedWrite: true},
	})

	rec := get(t, s, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	h := decode(t, rec)
	if h.Status != "ok" || len(h.Problems) != 0 || len(h.Warnings) != 0 {
		t.Errorf("unexpected health: %+v", h)
	}
	if h.TZ != "America/New_York" || h.Version != "test" {
		t.Errorf("tz/version: %+v", h)
	}
	if w := h.Paths["config"].Writable; w == nil || !*w {
		t.Errorf("config should be writable: %+v", h.Paths["config"])
	}
	if h.Paths["cards"].Writable != nil {
		t.Errorf("cards is never write-probed: %+v", h.Paths["cards"])
	}
	if p := s.WriteProblems(); len(p) != 0 {
		t.Errorf("WriteProblems: %v", p)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".sd2nas-write-test-*"))
	if len(matches) != 0 {
		t.Errorf("write probe left files behind: %v", matches)
	}
}

func TestHealthzMissingMount(t *testing.T) {
	t.Setenv("TZ", "UTC")
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope")
	s := New("test", []Mount{
		{Name: "cards", Path: dir},
		{Name: "photos", Path: missing, NeedWrite: true},
	})

	rec := get(t, s, "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	h := decode(t, rec)
	if h.Status != "degraded" || len(h.Problems) != 1 || !strings.Contains(h.Problems[0], "photos") {
		t.Errorf("unexpected health: %+v", h)
	}
	if len(s.WriteProblems()) != 1 {
		t.Errorf("WriteProblems should report photos: %v", s.WriteProblems())
	}
}

func TestHealthzWarnsWithoutTZ(t *testing.T) {
	t.Setenv("TZ", "")
	s := New("test", []Mount{{Name: "config", Path: t.TempDir(), NeedWrite: true}})
	rec := get(t, s, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("missing TZ is a warning, not a failure: status %d", rec.Code)
	}
	if h := decode(t, rec); len(h.Warnings) != 1 {
		t.Errorf("expected a TZ warning: %+v", h)
	}
}

func TestIndexAndUnknownPaths(t *testing.T) {
	s := uiServer(t, fstest.MapFS{".placeholder": {Data: []byte("x")}})
	if rec := get(t, s, "/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "v9") {
		t.Errorf("index: %d %s", rec.Code, rec.Body)
	}
	if rec := get(t, s, "/nope.txt"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	req.Header.Set("X-SD2NAS", "1") // pass the request guard to reach the router
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz: %d", rec.Code)
	}
}

func TestHealthzReportsAddedProblem(t *testing.T) {
	s := New("test", nil)
	s.AddProblem("catalog: boom")
	rec := get(t, s, "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	h := decode(t, rec)
	if h.Status != "degraded" || len(h.Problems) != 1 || h.Problems[0] != "catalog: boom" {
		t.Errorf("health: %+v", h)
	}
}

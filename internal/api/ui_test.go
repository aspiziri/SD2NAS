package api

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// builtUI stands in for a Vite build.
func builtUI() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                {Data: []byte(`<!doctype html><div id="app"></div>`)},
		"assets/index-AbC12345.js":  {Data: []byte("console.log(1)")},
		"assets/index-DeF67890.css": {Data: []byte("body{}")},
		"manifest.webmanifest":      {Data: []byte(`{"name":"SD2NAS"}`)},
		"icon-192.png":              {Data: []byte("png")},
		".placeholder":              {Data: []byte("x")},
	}
}

func uiServer(t *testing.T, files fs.FS) *Server {
	t.Helper()
	s := New("v9", nil)
	if err := s.MountUI(files); err != nil {
		t.Fatal(err)
	}
	return s
}

func wantHeader(t *testing.T, rec *httptest.ResponseRecorder, key, want string) {
	t.Helper()
	if got := rec.Header().Get(key); got != want {
		t.Errorf("%s = %q, want %q", key, got, want)
	}
}

func TestUIServesIndex(t *testing.T) {
	s := uiServer(t, builtUI())
	rec := get(t, s, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if rec.Body.String() != string(builtUI()["index.html"].Data) {
		t.Errorf("body %q", rec.Body)
	}
	wantHeader(t, rec, "Content-Type", "text/html; charset=utf-8")
	wantHeader(t, rec, "Cache-Control", "no-cache")
	if etag := rec.Header().Get("ETag"); len(etag) < 3 || etag[0] != '"' || etag[len(etag)-1] != '"' {
		t.Errorf("ETag %q is not a quoted string", etag)
	}
}

func TestUISPAFallback(t *testing.T) {
	s := uiServer(t, builtUI())
	for _, p := range []string{"/ingest", "/staging/3", "/ingest/12"} {
		rec := get(t, s, p)
		if rec.Code != http.StatusOK || rec.Body.String() != string(builtUI()["index.html"].Data) {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Body)
		}
		wantHeader(t, rec, "Cache-Control", "no-cache")
	}
}

func TestUIHashedAssetsImmutable(t *testing.T) {
	s := uiServer(t, builtUI())
	rec := get(t, s, "/assets/index-AbC12345.js")
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log(1)" {
		t.Fatalf("js: %d %q", rec.Code, rec.Body)
	}
	wantHeader(t, rec, "Cache-Control", "public, max-age=31536000, immutable")
	wantHeader(t, rec, "Content-Type", "text/javascript; charset=utf-8")
	rec = get(t, s, "/assets/index-DeF67890.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("css: %d", rec.Code)
	}
	wantHeader(t, rec, "Cache-Control", "public, max-age=31536000, immutable")
	wantHeader(t, rec, "Content-Type", "text/css; charset=utf-8")
}

func TestUIManifestAndIcons(t *testing.T) {
	s := uiServer(t, builtUI())
	rec := get(t, s, "/manifest.webmanifest")
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest: %d", rec.Code)
	}
	wantHeader(t, rec, "Content-Type", "application/manifest+json")
	wantHeader(t, rec, "Cache-Control", "no-cache")
	rec = get(t, s, "/icon-192.png")
	if rec.Code != http.StatusOK {
		t.Fatalf("icon: %d", rec.Code)
	}
	wantHeader(t, rec, "Cache-Control", "no-cache")
}

func TestUIConditional(t *testing.T) {
	s := uiServer(t, builtUI())
	for _, p := range []string{"/", "/assets/index-AbC12345.js"} {
		etag := get(t, s, p).Header().Get("ETag")
		if etag == "" {
			t.Fatalf("%s: no ETag", p)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("If-None-Match", etag)
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Errorf("%s: status %d, want 304", p, rec.Code)
		}
	}
}

func TestUIMissingFile(t *testing.T) {
	s := uiServer(t, builtUI())
	for _, p := range []string{"/assets/nope.js", "/favicon.ico", "/.placeholder"} {
		rec := get(t, s, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `id="app"`) {
			t.Errorf("%s: served the SPA shell", p)
		}
	}
}

func TestUIAPIUnknown(t *testing.T) {
	s := uiServer(t, builtUI())
	rec := get(t, s, "/api/nope")
	wantJSONError(t, rec, http.StatusNotFound)
	wantHeader(t, rec, "Cache-Control", "no-store")

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/nope", nil)
	req.Header.Set("X-SD2NAS", "1")
	s.ServeHTTP(rec, req)
	wantJSONError(t, rec, http.StatusNotFound)

	s = uiServer(t, builtUI())
	s.AddProblem("catalog: boom")
	msg := wantJSONError(t, get(t, s, "/api/ingests"), http.StatusServiceUnavailable)
	if !strings.Contains(msg, "catalog: boom") {
		t.Errorf("message %q does not name the problem", msg)
	}
}

func TestUINotBuilt(t *testing.T) {
	s := uiServer(t, fstest.MapFS{".placeholder": {Data: []byte("x")}})
	for _, p := range []string{"/", "/staging"} {
		rec := get(t, s, p)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", p, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "not built") || !strings.Contains(body, "v9") {
			t.Errorf("%s: body %q", p, body)
		}
		wantHeader(t, rec, "Cache-Control", "no-store")
	}
	if rec := get(t, s, "/app.js"); rec.Code != http.StatusNotFound {
		t.Errorf("/app.js: status %d, want 404", rec.Code)
	}
}

func TestUIHealthzUnaffected(t *testing.T) {
	s := uiServer(t, builtUI())
	rec := get(t, s, "/healthz")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version": "v9"`) {
		t.Errorf("healthz: %d %s", rec.Code, rec.Body)
	}
}

func TestUIPostIsNotServed(t *testing.T) {
	s := uiServer(t, builtUI())
	for _, p := range []string{"/", "/staging"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, p, nil)
		req.Header.Set("X-SD2NAS", "1")
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: status %d, want 405", p, rec.Code)
		}
		wantHeader(t, rec, "Allow", "GET, HEAD")
	}
}

package api

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
	"github.com/aspiziri/SD2NAS/internal/thumbs"
)

var shaCounter atomic.Int64

func nextSHA() string { return fmt.Sprintf("%064x", shaCounter.Add(1)) }

// insertAsset adds an asset with a fresh sha and returns the sha.
func insertAsset(t *testing.T, f fullServer, state catalog.AssetState, rel string) string {
	t.Helper()
	sha := nextSHA()
	a := catalog.Asset{SHA256: sha, Size: 10, Kind: media.KindImage, State: state, RelPath: rel, UpdatedAt: time.Now()}
	if err := f.db.InsertAsset(t.Context(), a, ""); err != nil {
		t.Fatal(err)
	}
	return sha
}

func writeJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(out, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// stageJPEG writes a real w x h JPEG at rel under the photos root and
// records it as a staged asset.
func stageJPEG(t *testing.T, f fullServer, rel string, w, h int) string {
	t.Helper()
	writeJPEG(t, filepath.Join(f.photos, filepath.FromSlash(rel)), w, h)
	return insertAsset(t, f, catalog.StateStaged, rel)
}

func thumbPath(f fullServer, sha string, size int) string {
	return filepath.Join(f.config, "thumbs", sha[:2], fmt.Sprintf("%s-%d.jpg", sha, size))
}

func getWith(t *testing.T, s *Server, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func wantThumbError(t *testing.T, rec *httptest.ResponseRecorder, code int, contains string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status %d, want %d: %s", rec.Code, code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var e struct {
		Error string `json:"error"`
	}
	decodeInto(t, rec, &e)
	if e.Error == "" || !strings.Contains(e.Error, contains) {
		t.Errorf("error %q should mention %q", e.Error, contains)
	}
}

func decodedSize(t *testing.T, rec *httptest.ResponseRecorder) (int, int) {
	t.Helper()
	img, err := jpeg.Decode(rec.Body)
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	b := img.Bounds()
	return b.Dx(), b.Dy()
}

func TestThumbServesJPEG(t *testing.T) {
	f := newFullServer(t)
	sha := stageJPEG(t, f, "Staging/2025-05-30/a.JPG", 2000, 1000)

	rec := get(t, f.s, "/api/thumbs/"+sha)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if et := rec.Header().Get("ETag"); et != `"`+sha+`-400"` {
		t.Errorf("ETag = %q", et)
	}
	if w, h := decodedSize(t, rec); w != 400 || h != 200 {
		t.Errorf("thumb is %dx%d, want 400x200", w, h)
	}
	if rec := getWith(t, f.s, "/api/thumbs/"+sha, map[string]string{"If-None-Match": `"` + sha + `-400"`}); rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: status %d, want 304", rec.Code)
	}

	rec = get(t, f.s, "/api/preview/"+sha)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status %d: %s", rec.Code, rec.Body)
	}
	if et := rec.Header().Get("ETag"); et != `"`+sha+`-1600"` {
		t.Errorf("preview ETag = %q", et)
	}
	if w, h := decodedSize(t, rec); w != 1600 || h != 800 {
		t.Errorf("preview is %dx%d, want 1600x800", w, h)
	}
	if _, err := os.Stat(thumbPath(f, sha, 400)); err != nil {
		t.Errorf("cache file: %v", err)
	}
}

func TestThumbBadSHA(t *testing.T) {
	f := newFullServer(t)
	for _, p := range []string{
		"/api/thumbs/abc",
		"/api/thumbs/" + strings.Repeat("A", 64),
		"/api/preview/" + strings.Repeat("z", 64),
	} {
		wantThumbError(t, get(t, f.s, p), http.StatusBadRequest, "sha")
	}
}

func TestThumbUnknownSHA(t *testing.T) {
	f := newFullServer(t)
	wantThumbError(t, get(t, f.s, "/api/thumbs/"+nextSHA()), http.StatusNotFound, "no file")
}

func TestThumbCulledAsset(t *testing.T) {
	f := newFullServer(t)
	sha := insertAsset(t, f, catalog.StateCulled, "")
	wantThumbError(t, get(t, f.s, "/api/thumbs/"+sha), http.StatusNotFound, "no file")
}

func TestThumbMissingFile(t *testing.T) {
	f := newFullServer(t)
	sha := stageJPEG(t, f, "Staging/2025-05-30/gone.JPG", 100, 100)
	if err := os.Remove(filepath.Join(f.photos, "Staging", "2025-05-30", "gone.JPG")); err != nil {
		t.Fatal(err)
	}
	wantThumbError(t, get(t, f.s, "/api/thumbs/"+sha), http.StatusNotFound, "missing")
}

func TestThumbSourceOutsideRoot(t *testing.T) {
	f := newFullServer(t)
	writeJPEG(t, filepath.Join(f.photos, "..", "outside.JPG"), 100, 100)
	sha := insertAsset(t, f, catalog.StateStaged, "../outside.JPG")
	rec := get(t, f.s, "/api/thumbs/"+sha)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(thumbPath(f, sha, 400)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file outside the photos root was rendered: %v", err)
	}
}

func TestThumbFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
		logs bool
	}{
		{"missing file", fmt.Errorf("x: %w", fs.ErrNotExist), 404, false},
		{"no image", thumbs.ErrNoImage, 404, false},
		{"missing tool", fmt.Errorf("x.ARW: %w", &exec.Error{Name: "exiftool", Err: exec.ErrNotFound}), 404, false},
		{"too large", fmt.Errorf("%w: 20000x10000", thumbs.ErrTooLarge), 422, false},
		{"closed", thumbs.ErrClosed, 503, false},
		{"canceled", context.Canceled, 503, false},
		{"deadline", context.DeadlineExceeded, 500, true},
		{"other", errors.New("x"), 500, true},
	}
	for _, c := range cases {
		code, msg, logIt := thumbFailure(c.err)
		if code != c.code || logIt != c.logs || msg == "" {
			t.Errorf("%s: got %d %q log=%v, want %d log=%v", c.name, code, msg, logIt, c.code, c.logs)
		}
	}
}

func TestThumbWarmAfterIngest(t *testing.T) {
	f := newFullServer(t)
	rec := post(t, f.s, "?wait=1", cardBody(f.card))
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest: %d %s", rec.Code, rec.Body)
	}
	var v viewForTest
	decodeInto(t, rec, &v)
	files, err := f.db.IngestFiles(t.Context(), v.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sha string
	for _, ff := range files {
		if ff.Outcome == catalog.OutcomeNew && strings.HasSuffix(ff.CardPath, ".JPG") {
			sha = ff.SHA256
		}
	}
	if sha == "" {
		t.Fatalf("no new JPEG in the ingest: %+v", files)
	}
	pollUntil(t, "the warmed thumbnail", func() bool {
		_, err := os.Stat(thumbPath(f, sha, 400))
		return err == nil
	})
}

func TestThumbsAfterClose(t *testing.T) {
	f := newFullServer(t)
	sha := stageJPEG(t, f, "Staging/2025-05-30/a.JPG", 100, 100)
	f.s.Close()
	wantThumbError(t, get(t, f.s, "/api/thumbs/"+sha), http.StatusServiceUnavailable, "shutting down")
}

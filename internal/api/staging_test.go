package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/aspiziri/SD2NAS/internal/staging"
)

func getFull(t *testing.T, f fullServer, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

func TestStagingAPI(t *testing.T) {
	f := newFullServer(t)
	if rec := post(t, f.s, "?wait=1", cardBody(f.card)); rec.Code != http.StatusOK {
		t.Fatalf("ingest: status %d, body %q", rec.Code, rec.Body)
	}

	rec := getFull(t, f, "/api/staging")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, cache-control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var l struct {
		Folders   []staging.Folder `json:"folders"`
		Untracked []string         `json:"untracked"`
	}
	decodeInto(t, rec, &l)
	if l.Untracked == nil || len(l.Untracked) != 0 || len(l.Folders) != 1 {
		t.Fatalf("listing = %s", rec.Body)
	}
	fo := l.Folders[0]
	if fo.Name != "2025-05-30" || fo.Counts != (staging.KindCounts{JPG: 1, Raw: 1}) || fo.Files != 2 ||
		len(fo.Samples) != 1 || fo.Samples[0].Kind != "jpg" {
		t.Errorf("folder = %+v", fo)
	}

	base := "/api/staging/" + strconv.FormatInt(fo.ID, 10) + "/files"
	var ff staging.FolderFiles
	rec = getFull(t, f, base)
	if rec.Code != http.StatusOK {
		t.Fatalf("files: status %d, body %q", rec.Code, rec.Body)
	}
	decodeInto(t, rec, &ff)
	if len(ff.Files) != 2 || ff.Folder.ID != fo.ID {
		t.Errorf("files = %+v", ff)
	}
	rec = getFull(t, f, base+"?kind=raw")
	if rec.Code != http.StatusOK {
		t.Fatalf("raw: status %d, body %q", rec.Code, rec.Body)
	}
	ff = staging.FolderFiles{}
	decodeInto(t, rec, &ff)
	if len(ff.Files) != 1 || !strings.HasPrefix(ff.Files[0].Path, "Capture/") {
		t.Errorf("raw files = %+v", ff.Files)
	}
}

func TestStagingAPIErrors(t *testing.T) {
	f := newFullServer(t)
	if rec := post(t, f.s, "?wait=1", cardBody(f.card)); rec.Code != http.StatusOK {
		t.Fatalf("ingest: status %d, body %q", rec.Code, rec.Body)
	}
	folders, err := f.db.StagingFolders(t.Context())
	if err != nil || len(folders) != 1 {
		t.Fatalf("folders = %v, err %v", folders, err)
	}
	for _, c := range []struct {
		url  string
		code int
		in   string
	}{
		{"/api/staging/999/files", http.StatusNotFound, "That folder no longer exists."},
		{"/api/staging/abc/files", http.StatusBadRequest, "The folder id must be a positive whole number."},
		{"/api/staging/0/files", http.StatusBadRequest, "The folder id must be a positive whole number."},
		{"/api/staging/" + strconv.FormatInt(folders[0].ID, 10) + "/files?kind=bogus", http.StatusBadRequest, "The kind must be jpg, raw or video."},
	} {
		rec := getFull(t, f, c.url)
		var e struct {
			Error string `json:"error"`
		}
		decodeInto(t, rec, &e)
		if rec.Code != c.code || !strings.Contains(e.Error, c.in) {
			t.Errorf("%s: status %d, error %q", c.url, rec.Code, e.Error)
		}
	}
}

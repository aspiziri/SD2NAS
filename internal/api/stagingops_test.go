package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/staging"
)

// send makes a guarded request (X-SD2NAS: 1) with an optional JSON body.
// (For the one request without the header, build it with
// httptest.NewRequest directly: pre-flight S17.)
func send(t *testing.T, s *Server, method, url, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, url, nil)
	} else {
		req = httptest.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-SD2NAS", "1")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// collectFrames subscribes to s's hub and drains it in the background, so
// a burst of events never fills the 16-frame buffer and drops the test
// (pre-flight E8); frames returns what has arrived so far. It unsubscribes
// at cleanup.
func collectFrames(t *testing.T, s *Server) (frames func() []string) {
	t.Helper()
	ch, unsubscribe := s.hub.subscribe()
	var (
		mu  sync.Mutex
		got []string
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for f := range ch {
			mu.Lock()
			got = append(got, string(f))
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		unsubscribe()
		<-done
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// ingested runs the SONY card import and returns its one folder.
func ingested(t *testing.T, f fullServer) staging.Folder {
	t.Helper()
	if rec := post(t, f.s, "?wait=1", cardBody(f.card)); rec.Code != http.StatusOK {
		t.Fatalf("ingest: status %d, body %s", rec.Code, rec.Body)
	}
	l := listing(t, f)
	if len(l.Folders) != 1 {
		t.Fatalf("want one staging folder, got %+v", l.Folders)
	}
	return l.Folders[0]
}

func listing(t *testing.T, f fullServer) staging.Listing {
	t.Helper()
	rec := send(t, f.s, http.MethodGet, "/api/staging", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d, body %s", rec.Code, rec.Body)
	}
	var l staging.Listing
	decodeInto(t, rec, &l)
	return l
}

func folderFiles(t *testing.T, f fullServer, id int64) staging.FolderFiles {
	t.Helper()
	rec := send(t, f.s, http.MethodGet, fmt.Sprintf("/api/staging/%d/files", id), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("files: status %d, body %s", rec.Code, rec.Body)
	}
	var ff staging.FolderFiles
	decodeInto(t, rec, &ff)
	return ff
}

type errBody struct {
	Error    string `json:"error"`
	Code     string `json:"code"`
	FolderID int64  `json:"folder_id"`
	Holder   string `json:"holder"`
}

// wantCode checks the status and, for an error, the code field.
func wantCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) errBody {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status %d, want %d (body %s)", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", cc)
	}
	var e errBody
	decodeInto(t, rec, &e)
	if e.Code != code {
		t.Errorf("code %q, want %q (body %s)", e.Code, code, rec.Body)
	}
	return e
}

func folderIn(t *testing.T, rec *httptest.ResponseRecorder) staging.Folder {
	t.Helper()
	var v struct {
		Folder staging.Folder `json:"folder"`
	}
	decodeInto(t, rec, &v)
	return v.Folder
}

func TestStagingRenameAPI(t *testing.T) {
	f := newFullServer(t)
	a := ingested(t, f)
	url := fmt.Sprintf("/api/staging/%d", a.ID)

	rec := send(t, f.s, http.MethodPatch, url, `{"title":"Trip"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if got := folderIn(t, rec); got.Name != "2025-05-30 - Trip" || got.ID != a.ID {
		t.Errorf("folder %+v", got)
	}
	if l := listing(t, f); len(l.Folders) != 1 || l.Folders[0].Name != "2025-05-30 - Trip" {
		t.Errorf("listing %+v", l.Folders)
	}
	entries, err := os.ReadDir(filepath.Join(f.photos, "Staging", "2025-05-30 - Trip"))
	if err != nil || len(entries) == 0 {
		t.Errorf("files not under the new name: %v %v", entries, err)
	}

	for _, body := range []string{`{}`, `{"title":"a/b"}`, `{"title":"x","extra":1}`} {
		e := wantCode(t, send(t, f.s, http.MethodPatch, url, body), http.StatusBadRequest, "")
		if strings.Contains(e.Error, "staging:") {
			t.Errorf("%s: error has a Go prefix: %q", body, e.Error)
		}
	}
	rec = send(t, f.s, http.MethodPatch, url, `{"title":"ignored","name":"2025-06-02 - Hike"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("name: status %d, body %s", rec.Code, rec.Body)
	}
	if got := folderIn(t, rec); got.Name != "2025-06-02 - Hike" {
		t.Errorf("name: folder %+v", got)
	}
	if e := wantCode(t, send(t, f.s, http.MethodPatch, url, `{"name":"a/b"}`), http.StatusBadRequest, ""); !strings.HasPrefix(e.Error, "The folder name contains") {
		t.Errorf("bad-name error %q", e.Error)
	}
	e := wantCode(t, send(t, f.s, http.MethodPatch, url, `{"title":"a/b"}`), http.StatusBadRequest, "")
	if !strings.HasPrefix(e.Error, "The title contains") {
		t.Errorf("bad-title error %q", e.Error)
	}
	e = wantCode(t, send(t, f.s, http.MethodPatch, "/api/staging/999", `{"title":"x"}`), http.StatusNotFound, "")
	if e.Error != "That folder no longer exists." {
		t.Errorf("404 error %q", e.Error)
	}
	wantCode(t, send(t, f.s, http.MethodPatch, "/api/staging/abc", `{"title":"x"}`), http.StatusBadRequest, "")

	req := httptest.NewRequest(http.MethodPatch, url, strings.NewReader(`{"title":"Z"}`))
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("without X-SD2NAS: status %d, want 403", rec.Code)
	}
}

func TestStagingRenameBusy409(t *testing.T) {
	f := newFullServer(t)
	a := ingested(t, f)
	url := fmt.Sprintf("/api/staging/%d", a.ID)

	release, err := f.locks.TryLock(staging.HolderIngest, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	e := wantCode(t, send(t, f.s, http.MethodPatch, url, `{"title":"Trip"}`), http.StatusConflict, "folder_busy")
	if e.Holder != "ingest" || e.FolderID != a.ID || e.Error != "2025-05-30 is being imported into. Try again when the import finishes." {
		t.Errorf("body %+v", e)
	}
	release()

	release, err = f.locks.TryLock(staging.HolderOperation, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	e = wantCode(t, send(t, f.s, http.MethodPatch, url, `{"title":"Trip"}`), http.StatusConflict, "folder_busy")
	if e.Holder != "operation" || e.Error != "2025-05-30 is busy with another change. Try again in a moment." {
		t.Errorf("body %+v", e)
	}
}

func TestBusyMessages(t *testing.T) {
	cases := []struct {
		b    staging.BusyError
		want string
	}{
		{staging.BusyError{ID: 1, Name: "2025-05-30", Holder: staging.HolderIngest}, "2025-05-30 is being imported into. Try again when the import finishes."},
		{staging.BusyError{ID: 1, Name: "2025-05-30", Holder: staging.HolderOperation}, "2025-05-30 is busy with another change. Try again in a moment."},
		{staging.BusyError{ID: 1, Name: "2025-05-30", Holder: staging.HolderRescan}, "2025-05-30 is being rescanned. Try again in a moment."},
		{staging.BusyError{ID: 1, Name: "2025-05-30", Holder: staging.HolderPromote, Queued: true}, "2025-05-30 is waiting to be promoted."},
		{staging.BusyError{ID: 1, Name: "2025-05-30", Holder: staging.HolderPromote}, "2025-05-30 is being promoted."},
		{staging.BusyError{ID: 7, Holder: staging.HolderRescan}, "Folder 7 is being rescanned. Try again in a moment."},
	}
	for _, c := range cases {
		if got := busyMessage(&c.b); got != c.want {
			t.Errorf("%+v: %q, want %q", c.b, got, c.want)
		}
	}

	rec := httptest.NewRecorder()
	writeOpError(rec, nil, "folder", "x", &staging.BusyError{Name: "Staging/2025-05-30", Holder: staging.HolderRescan})
	if strings.Contains(rec.Body.String(), "folder_id") {
		t.Errorf("a name key must not carry folder_id: %s", rec.Body)
	}
	wantCode(t, rec, http.StatusConflict, "folder_busy")
}

func TestStagingRenameNameTaken(t *testing.T) {
	f := newFullServer(t)
	a := ingested(t, f)
	if err := os.MkdirAll(filepath.Join(f.photos, "Staging", "2025-05-30 - Trip"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := wantCode(t, send(t, f.s, http.MethodPatch, fmt.Sprintf("/api/staging/%d", a.ID), `{"title":"Trip"}`), http.StatusConflict, "name_taken")
	if e.Error != "A folder with that name already exists in Staging. Pick another title." {
		t.Errorf("error %q", e.Error)
	}
}

func TestStagingMergeAPI(t *testing.T) {
	f := newFullServer(t)
	a := ingested(t, f)
	writeJPEG(t, filepath.Join(f.photos, "Staging", "2025-06-01 - Beach", "a.JPG"), 40, 30)
	if _, err := f.svc.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	l := listing(t, f)
	if len(l.Folders) != 2 {
		t.Fatalf("folders %+v", l.Folders)
	}
	var b int64
	for _, fo := range l.Folders {
		if fo.ID != a.ID {
			b = fo.ID
		}
	}

	rec := send(t, f.s, http.MethodPost, "/api/staging/merge", fmt.Sprintf(`{"ids":[%d,%d],"title":"Trip"}`, a.ID, b))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if got := folderIn(t, rec); got.Name != "2025-05-30 - Trip" || got.Files != 3 {
		t.Errorf("folder %+v", got)
	}
	if l := listing(t, f); len(l.Folders) != 1 {
		t.Errorf("listing %+v", l.Folders)
	}
	wantCode(t, send(t, f.s, http.MethodPost, "/api/staging/merge", fmt.Sprintf(`{"ids":[%d]}`, a.ID)), http.StatusBadRequest, "")
}

func TestStagingMergeAPIWholeName(t *testing.T) {
	f := newFullServer(t)
	a := ingested(t, f)
	writeJPEG(t, filepath.Join(f.photos, "Staging", "2025-06-01 - Beach", "a.JPG"), 40, 30)
	if _, err := f.svc.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, fo := range listing(t, f).Folders {
		ids = append(ids, fmt.Sprint(fo.ID))
	}
	body := fmt.Sprintf(`{"ids":[%s],"title":"ignored","name":"2025-06-01 - Trip"}`, strings.Join(ids, ","))
	rec := send(t, f.s, http.MethodPost, "/api/staging/merge", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if got := folderIn(t, rec); got.Name != "2025-06-01 - Trip" || got.Files != 3 {
		t.Errorf("folder %+v", got)
	}
	_ = a
	wantCode(t, send(t, f.s, http.MethodPost, "/api/staging/merge", fmt.Sprintf(`{"ids":[%s],"name":"  "}`, strings.Join(ids, ","))), http.StatusBadRequest, "")
}

func TestStagingMoveAndCullAPI(t *testing.T) {
	f := newFullServer(t)
	a := ingested(t, f)
	var jpg, arw string
	for _, file := range folderFiles(t, f, a.ID).Files {
		switch file.Kind {
		case "jpg":
			jpg = file.SHA256
		case "raw":
			arw = file.SHA256
		}
	}
	if jpg == "" || arw == "" {
		t.Fatalf("files %+v", folderFiles(t, f, a.ID).Files)
	}

	rec := send(t, f.s, http.MethodPost, "/api/staging/move", fmt.Sprintf(`{"fileShas":[%q],"newTitle":"Raw"}`, arw))
	if rec.Code != http.StatusOK {
		t.Fatalf("move: status %d, body %s", rec.Code, rec.Body)
	}
	var mv staging.MoveResult
	decodeInto(t, rec, &mv)
	if mv.Folder.Name != "2025-05-30 - Raw" || mv.Moved != 1 || mv.Removed == nil || len(mv.Removed) != 0 {
		t.Errorf("move result %+v", mv)
	}

	if rec := getWith(t, f.s, "/api/thumbs/"+jpg, nil); rec.Code != http.StatusOK {
		t.Fatalf("thumb: status %d, body %.200s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(thumbPath(f, jpg, 400)); err != nil {
		t.Fatalf("thumbnail not cached: %v", err)
	}
	rec = send(t, f.s, http.MethodPost, "/api/staging/cull", fmt.Sprintf(`{"fileShas":[%q]}`, jpg))
	if rec.Code != http.StatusOK {
		t.Fatalf("cull: status %d, body %s", rec.Code, rec.Body)
	}
	var cu staging.CullResult
	decodeInto(t, rec, &cu)
	if cu.Culled != 1 || len(cu.Removed) != 1 || cu.Removed[0] != a.ID {
		t.Errorf("cull result %+v", cu)
	}
	if _, err := os.Stat(thumbPath(f, jpg, 400)); !os.IsNotExist(err) {
		t.Errorf("culled file's thumbnail still cached (stat err %v)", err)
	}

	// Bad requests.
	both := fmt.Sprintf(`{"fileShas":[%q],"toId":%d,"newTitle":"x"}`, arw, mv.Folder.ID)
	wantCode(t, send(t, f.s, http.MethodPost, "/api/staging/move", both), http.StatusBadRequest, "")
	wantCode(t, send(t, f.s, http.MethodPost, "/api/staging/move", fmt.Sprintf(`{"fileShas":[%q]}`, arw)), http.StatusBadRequest, "")
	wantCode(t, send(t, f.s, http.MethodPost, "/api/staging/move", `{"fileShas":["nope"],"newTitle":"x"}`), http.StatusBadRequest, "")
	unknown := strings.Repeat("ab", 32)
	wantCode(t, send(t, f.s, http.MethodPost, "/api/staging/move", fmt.Sprintf(`{"fileShas":[%q],"newTitle":"x"}`, unknown)), http.StatusBadRequest, "")
	wantCode(t, send(t, f.s, http.MethodPost, "/api/staging/cull", fmt.Sprintf(`{"fileShas":[%q]}`, unknown)), http.StatusBadRequest, "")

	many := make([]string, staging.MaxFiles+1)
	for i := range many {
		many[i] = unknown
	}
	enc, err := json.Marshal(map[string]any{"fileShas": many})
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, send(t, f.s, http.MethodPost, "/api/staging/cull", string(enc)), http.StatusBadRequest, "")

	huge := `{"fileShas":["` + strings.Repeat("a", 4<<20) + `"]}`
	wantCode(t, send(t, f.s, http.MethodPost, "/api/staging/cull", huge), http.StatusBadRequest, "")
}

// Every handler-level 400 is a plain sentence: capitalised, ending with a full stop.
func TestStagingBadRequestSentences(t *testing.T) {
	f := newFullServer(t)
	for _, c := range []struct{ method, url, body, want string }{
		{http.MethodPatch, "/api/staging/abc", `{"title":"x"}`, "The folder id must be a positive whole number."},
		{http.MethodPatch, "/api/staging/1", `{`, "The request body is not valid JSON."},
		{http.MethodPatch, "/api/staging/1", `{"title":"x","extra":1}`, "The request has a field this server does not know."},
		{http.MethodPatch, "/api/staging/1", `{}`, "Give a name, or a title (an empty one removes it)."},
		{http.MethodPost, "/api/staging/cull", `{"fileShas":["` + strings.Repeat("a", 5<<20) + `"]}`, "The request is too large."},
		{http.MethodPost, "/api/staging/move", `{"fileShas":[]}`, "Give exactly one of toId and newTitle."},
		{http.MethodPost, "/api/staging/move", `{"fileShas":[],"toId":0}`, "The destination folder id must be a positive whole number."},
		{http.MethodPost, "/api/staging/move", `{"fileShas":[],"newTitle":" "}`, "Give the new folder a title."},
		{http.MethodPost, "/api/staging/promote", `[]`, "The request body is not valid JSON."},
		{http.MethodPost, "/api/ingest/1/forget", `{"deleteFiles":true}x`, "The request body must hold exactly one JSON value."},
	} {
		e := wantCode(t, send(t, f.s, c.method, c.url, c.body), http.StatusBadRequest, "")
		if e.Error != c.want {
			t.Errorf("%s %s %.30q: error %q, want %q", c.method, c.url, c.body, e.Error, c.want)
		}
	}
}

func TestStagingDiscardAPI(t *testing.T) {
	f := newFullServer(t)
	a := ingested(t, f)
	rec := send(t, f.s, http.MethodDelete, fmt.Sprintf("/api/staging/%d", a.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"culled":2,"removed":true,"kept":[]}` {
		t.Errorf("body %s", got)
	}
	wantCode(t, send(t, f.s, http.MethodGet, fmt.Sprintf("/api/staging/%d/files", a.ID), ""), http.StatusNotFound, "")

	// An untracked file keeps its folder.
	dir := filepath.Join(f.photos, "Staging", "2025-06-01 - Beach")
	writeJPEG(t, filepath.Join(dir, "a.JPG"), 40, 30)
	if _, err := f.svc.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := listing(t, f)
	if len(l.Folders) != 1 {
		t.Fatalf("folders %+v", l.Folders)
	}
	rec = send(t, f.s, http.MethodDelete, fmt.Sprintf("/api/staging/%d", l.Folders[0].ID), "")
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusOK || got != `{"culled":1,"removed":false,"kept":["notes.txt"]}` {
		t.Errorf("status %d, body %s", rec.Code, got)
	}
}

// stagingVersions returns the version of every staging.changed frame.
func stagingVersions(frames []string) []uint64 {
	var out []uint64
	for _, fr := range frames {
		data, ok := strings.CutPrefix(fr, "event: staging.changed\ndata: ")
		if !ok {
			continue
		}
		var st staging.Status
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &st) == nil {
			out = append(out, st.Version)
		}
	}
	return out
}

func TestStagingChangedEvent(t *testing.T) {
	f := newFullServer(t)
	a := ingested(t, f)
	before := listing(t, f).Version
	frames := collectFrames(t, f.s)

	if rec := send(t, f.s, http.MethodPatch, fmt.Sprintf("/api/staging/%d", a.ID), `{"title":"Trip"}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	pollUntil(t, "a staging.changed frame", func() bool {
		for _, v := range stagingVersions(frames()) {
			if v > before {
				return true
			}
		}
		return false
	})
	for _, fr := range frames() {
		if strings.HasPrefix(fr, "event: staging.changed") && !strings.Contains(fr, `"rescanning":false`) {
			t.Errorf("frame %q", fr)
		}
	}

	url := eventsServer(t, f.s) + "/api/events"
	stream, cancel := sseFrames(t, url)
	defer cancel()
	now := listing(t, f).Version
	deadline := time.After(10 * time.Second)
	for {
		select {
		case fr, ok := <-stream:
			if !ok {
				t.Fatal("stream ended")
			}
			if vs := stagingVersions([]string{fr + "\n"}); len(vs) == 1 {
				if vs[0] != now {
					t.Errorf("snapshot version %d, want %d", vs[0], now)
				}
				return
			}
		case <-deadline:
			t.Fatal("no staging.changed snapshot on connect")
		}
	}
}

func TestStagingTouchedWhenImportFinishes(t *testing.T) {
	f := newFullServer(t)
	frames := collectFrames(t, f.s)
	if rec := post(t, f.s, "?wait=1", cardBody(f.card)); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	pollUntil(t, "staging.changed after the import", func() bool {
		return len(stagingVersions(frames())) > 0
	})
}

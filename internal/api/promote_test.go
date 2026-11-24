package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aspiziri/SD2NAS/internal/staging"
)

// promoteFrames returns the promote.progress frames in got, decoded.
func promoteFrames(t *testing.T, got []string) []staging.PromoteQueue {
	t.Helper()
	var out []staging.PromoteQueue
	for _, fr := range got {
		data, ok := strings.CutPrefix(fr, "event: promote.progress\ndata: ")
		if !ok {
			continue
		}
		var q staging.PromoteQueue
		if err := json.Unmarshal([]byte(strings.TrimSuffix(data, "\n\n")), &q); err != nil {
			t.Fatalf("frame %q: %v", fr, err)
		}
		out = append(out, q)
	}
	return out
}

// waitPromoted polls GET /api/staging/promote until the queue is idle with
// a finished job, and returns it.
func waitPromoted(t *testing.T, f fullServer) staging.PromoteQueue {
	t.Helper()
	var q staging.PromoteQueue
	pollUntil(t, "the promote queue to go idle", func() bool {
		rec := send(t, f.s, http.MethodGet, "/api/staging/promote", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("promote status: %d, body %s", rec.Code, rec.Body)
		}
		q = staging.PromoteQueue{}
		decodeInto(t, rec, &q)
		return q.Running == nil && len(q.Queued) == 0 && q.Last != nil
	})
	return q
}

func TestPromoteAPI(t *testing.T) {
	f := newFullServer(t)
	frames := collectFrames(t, f.s)
	a := ingested(t, f)
	ff := folderFiles(t, f, a.ID)
	var jpg, arw, jpgPath, arwPath string
	for _, file := range ff.Files {
		switch file.Kind {
		case "jpg":
			jpg, jpgPath = file.SHA256, file.Path
		case "raw":
			arw, arwPath = file.SHA256, file.Path
		}
	}
	if jpg == "" || arw == "" {
		t.Fatalf("files %+v", ff.Files)
	}

	rec := send(t, f.s, http.MethodPost, "/api/staging/promote", fmt.Sprintf(`{"ids":[%d],"pattern":""}`, a.ID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("promote: status %d, body %s", rec.Code, rec.Body)
	}
	var enq struct {
		Job staging.PromoteJob `json:"job"`
	}
	decodeInto(t, rec, &enq)
	if (enq.Job.State != staging.PromoteQueued && enq.Job.State != staging.PromoteRunning) ||
		len(enq.Job.Folders) != 1 || enq.Job.Folders[0].Target != "Archive/2025-05-30" {
		t.Fatalf("job %+v", enq.Job)
	}

	q := waitPromoted(t, f)
	if q.Last.ID != enq.Job.ID || q.Last.State != staging.PromoteDone {
		t.Fatalf("last = %+v", q.Last)
	}
	for _, p := range []string{jpgPath, arwPath} {
		if _, err := os.Stat(filepath.Join(f.photos, "Archive", "2025-05-30", filepath.FromSlash(p))); err != nil {
			t.Errorf("not promoted: %v", err)
		}
	}
	if l := listing(t, f); len(l.Folders) != 0 {
		t.Errorf("staging still lists %+v", l.Folders)
	}
	if rec := getWith(t, f.s, "/api/thumbs/"+jpg, nil); rec.Code != http.StatusOK {
		t.Errorf("thumb after promote: status %d, body %.200s", rec.Code, rec.Body)
	}

	var sawRunning, sawDone bool
	pf := promoteFrames(t, frames())
	for _, q := range pf {
		if q.Running != nil && q.Running.ID == enq.Job.ID {
			sawRunning = true
		}
		if q.Last != nil && q.Last.ID == enq.Job.ID && q.Last.State == staging.PromoteDone {
			sawDone = true
		}
		if q.Queued == nil {
			t.Errorf("queued is null in a frame: %+v", q)
		}
	}
	if !sawRunning || !sawDone || len(pf) >= 12 {
		t.Errorf("promote.progress frames: running %v, done %v, count %d", sawRunning, sawDone, len(pf))
	}
}

func TestPromoteAPIErrors(t *testing.T) {
	f := newFullServer(t)
	a := ingested(t, f)

	for _, body := range []string{`{"ids":[]}`, `{"ids":[],"pattern":""}`, `nope`, `{"ids":[1],"extra":1}`} {
		e := wantCode(t, send(t, f.s, http.MethodPost, "/api/staging/promote", body), http.StatusBadRequest, "")
		if strings.Contains(e.Error, "staging:") {
			t.Errorf("%s: Go prefix in %q", body, e.Error)
		}
	}
	e := wantCode(t, send(t, f.s, http.MethodPost, "/api/staging/promote", `{"ids":[999]}`), http.StatusNotFound, "")
	if e.Error != "That folder no longer exists." {
		t.Errorf("404 error %q", e.Error)
	}
	wantCode(t, send(t, f.s, http.MethodPost, "/api/staging/promote", fmt.Sprintf(`{"ids":[%d],"pattern":"{day}"}`, a.ID)), http.StatusBadRequest, "")

	url := "/api/staging/promote"
	body := fmt.Sprintf(`{"ids":[%d],"pattern":""}`, a.ID)
	release, err := f.locks.TryLock(staging.HolderIngest, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	e = wantCode(t, send(t, f.s, http.MethodPost, url, body), http.StatusConflict, "folder_busy")
	if e.Holder != "ingest" {
		t.Errorf("body %+v", e)
	}
	release()

	release, err = f.locks.TryLock(staging.HolderPromote, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	e = wantCode(t, send(t, f.s, http.MethodPost, url, body), http.StatusConflict, "folder_busy")
	if e.Holder != "promote" || e.Error != "2025-05-30 is being promoted." {
		t.Errorf("body %+v", e)
	}
	release()

	rec := send(t, f.s, http.MethodGet, url, "")
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusOK || got != `{"running":null,"queued":[],"last":null}` {
		t.Errorf("idle status %d: %s", rec.Code, got)
	}
}

func TestPromoteQueueAPI(t *testing.T) {
	f := newFullServer(t)
	a := ingested(t, f)
	rec := send(t, f.s, http.MethodPost, "/api/staging/promote", fmt.Sprintf(`{"ids":[%d]}`, a.ID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("promote: status %d, body %s", rec.Code, rec.Body)
	}
	waitPromoted(t, f)

	e := wantCode(t, send(t, f.s, http.MethodDelete, "/api/staging/promote/1", ""), http.StatusConflict, "promote_started")
	if e.Error != "That promote has already started or finished, so it can't be cancelled." {
		t.Errorf("error %q", e.Error)
	}
	e = wantCode(t, send(t, f.s, http.MethodDelete, "/api/staging/promote/999", ""), http.StatusNotFound, "")
	if e.Error != "That promote no longer exists." {
		t.Errorf("error %q", e.Error)
	}
	wantCode(t, send(t, f.s, http.MethodDelete, "/api/staging/promote/abc", ""), http.StatusBadRequest, "")

	req := httptest.NewRequest(http.MethodDelete, "/api/staging/promote/1", nil)
	rec = httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("without X-SD2NAS: status %d, want 403", rec.Code)
	}
}

func TestPromoteSnapshot(t *testing.T) {
	f := newFullServer(t)
	base := eventsServer(t, f.s)

	snapshot := func() string {
		frames, cancel := sseFrames(t, base+"/api/events")
		defer cancel()
		for {
			fr := nextFrame(t, frames)
			if data, ok := strings.CutPrefix(fr, "event: promote.progress\ndata: "); ok {
				return data
			}
		}
	}
	if got, want := snapshot(), `{"running":null,"queued":[],"last":null}`; got != want {
		t.Errorf("idle snapshot %q, want %q", got, want)
	}

	a := ingested(t, f)
	rec := send(t, f.s, http.MethodPost, "/api/staging/promote", fmt.Sprintf(`{"ids":[%d]}`, a.ID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("promote: status %d, body %s", rec.Code, rec.Body)
	}
	q := waitPromoted(t, f)
	var got staging.PromoteQueue
	if err := json.Unmarshal([]byte(snapshot()), &got); err != nil {
		t.Fatal(err)
	}
	if got.Last == nil || got.Last.ID != q.Last.ID || got.Last.State != staging.PromoteDone {
		t.Errorf("snapshot last = %+v", got.Last)
	}
}

func TestRescanAPI(t *testing.T) {
	f := newFullServer(t)
	frames := collectFrames(t, f.s)
	a := ingested(t, f)
	writeJPEG(t, filepath.Join(f.photos, "Staging", a.Name, "smb.JPG"), 40, 30) // as if copied over SMB

	rec := send(t, f.s, http.MethodPost, "/api/staging/rescan", "")
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusAccepted || got != `{"started":true}` {
		t.Fatalf("rescan: status %d, body %s", rec.Code, got)
	}
	pollUntil(t, "the rescan to finish", func() bool {
		st := f.svc.Status()
		return !st.Rescanning && st.LastRescan != nil
	})
	if n := len(folderFiles(t, f, a.ID).Files); n != 3 {
		t.Errorf("listing shows %d files, want 3", n)
	}

	var sawRescanning, sawAdded bool
	for _, fr := range frames() {
		data, ok := strings.CutPrefix(fr, "event: staging.changed\ndata: ")
		if !ok {
			continue
		}
		var st staging.Status
		if err := json.Unmarshal([]byte(strings.TrimSuffix(data, "\n\n")), &st); err != nil {
			t.Fatal(err)
		}
		if st.Rescanning {
			sawRescanning = true
		}
		if st.LastRescan != nil && st.LastRescan.AddedFiles == 1 {
			sawAdded = true
		}
	}
	if !sawRescanning || !sawAdded {
		t.Errorf("staging.changed frames: rescanning %v, added %v", sawRescanning, sawAdded)
	}
}

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/indexer"
	"github.com/aspiziri/SD2NAS/internal/staging"
)

func TestArchiveDirsMatch(t *testing.T) {
	if indexer.ArchiveDir != staging.ArchiveDir {
		t.Errorf("indexer.ArchiveDir %q, staging.ArchiveDir %q", indexer.ArchiveDir, staging.ArchiveDir)
	}
	if indexer.StagingDir != staging.Dir {
		t.Errorf("indexer.StagingDir %q, staging.Dir %q", indexer.StagingDir, staging.Dir)
	}
	if catalog.ArchiveRoot != indexer.ArchiveDir+"/" {
		t.Errorf("catalog.ArchiveRoot %q, want %q", catalog.ArchiveRoot, indexer.ArchiveDir+"/")
	}
}

// putArchive writes photos/Archive/<rel> with a whole-second mtime.
func putArchive(t *testing.T, f fullServer, rel, content string) {
	t.Helper()
	p := filepath.Join(f.photos, "Archive", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Unix(time.Now().Add(-time.Hour).Unix(), 0)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func archiveStats(t *testing.T, f fullServer) map[string]any {
	t.Helper()
	rec := send(t, f.s, http.MethodGet, "/api/archive/stats", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats: status %d, body %s", rec.Code, rec.Body)
	}
	var m map[string]any
	decodeInto(t, rec, &m)
	return m
}

// waitIdle polls the stats until no pass runs and returns them.
func waitIdle(t *testing.T, f fullServer) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		m := archiveStats(t, f)
		if m["running"] == nil {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pass did not finish: %v", m["running"])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func wantStarted(t *testing.T, f fullServer, body string, started bool) {
	t.Helper()
	rec := send(t, f.s, http.MethodPost, "/api/archive/rescan", body)
	want := fmt.Sprintf(`{"started":%v}`, started)
	if rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("rescan %q: status %d, body %s, want 202 %s", body, rec.Code, rec.Body, want)
	}
}

// scanned runs one pass through the API and returns the idle stats.
func scanned(t *testing.T, f fullServer) map[string]any {
	t.Helper()
	wantStarted(t, f, "", true)
	return waitIdle(t, f)
}

func TestArchiveStatsAPI(t *testing.T) {
	f := newFullServer(t)
	putArchive(t, f, "S/a.jpg", "aaa")
	putArchive(t, f, "S/b.jpg", "bbbb")
	m := scanned(t, f)
	if m["files"] != float64(2) {
		t.Errorf("files %v, want 2", m["files"])
	}
	last, _ := m["last"].(map[string]any)
	if last["added"] != float64(2) {
		t.Errorf("last.added %v, want 2 (%v)", last["added"], m["last"])
	}
	if m["rescan_at"] != "03:00" {
		t.Errorf("rescan_at %v", m["rescan_at"])
	}
	if bd, _ := m["backup_dir"].(string); !strings.HasSuffix(bd, filepath.Join("Management", "SD2NAS")) {
		t.Errorf("backup_dir %q", bd)
	}
	skip, _ := json.Marshal(m["skip_rules"])
	if string(skip) != `["Output/","CaptureOne/","*.cosessiondb"]` {
		t.Errorf("skip_rules %s", skip)
	}
	for _, k := range []string{"missing_files", "recent"} {
		if l, ok := m[k].([]any); !ok || len(l) != 0 {
			t.Errorf("%s = %#v, want []", k, m[k])
		}
	}
}

func TestArchiveRescanBusyAndPause(t *testing.T) {
	f := newFullServer(t)
	for i := range 3 {
		putArchive(t, f, fmt.Sprintf("S/%d.jpg", i), fmt.Sprint("content", i))
	}
	f.busy.Store(true)
	wantStarted(t, f, "", true)
	wantStarted(t, f, "", false)
	pausedBy := func() any {
		r, _ := archiveStats(t, f)["running"].(map[string]any)
		return r["paused_by"]
	}
	deadline := time.Now().Add(10 * time.Second)
	for pausedBy() != "busy" {
		if time.Now().After(deadline) {
			t.Fatal("the pass never paused for the busy import")
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec := send(t, f.s, http.MethodPost, "/api/archive/pause", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("pause: %d %s", rec.Code, rec.Body)
	}
	var st indexer.Status
	decodeInto(t, rec, &st)
	if st.Running == nil || st.Running.PausedBy != "user" {
		t.Errorf("pause: running %+v", st.Running)
	}
	if rec = send(t, f.s, http.MethodPost, "/api/archive/resume", ""); rec.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body)
	}
	f.busy.Store(false)
	waitIdle(t, f)
	for _, p := range []string{"pause", "resume"} {
		e := wantCode(t, send(t, f.s, http.MethodPost, "/api/archive/"+p, ""), http.StatusConflict, "not_running")
		if e.Error != "No scan is running." {
			t.Errorf("%s: error %q", p, e.Error)
		}
	}
}

func TestArchiveRescanFull(t *testing.T) {
	f := newFullServer(t)
	putArchive(t, f, "S/a.jpg", "aaa")
	putArchive(t, f, "S/b.jpg", "bbb2")
	scanned(t, f)
	wantStarted(t, f, `{"full":true}`, true)
	last, _ := waitIdle(t, f)["last"].(map[string]any)
	if last["full"] != true || last["unchanged"] != float64(2) {
		t.Errorf("last %v, want a full pass with 2 unchanged", last)
	}
	for body, msg := range map[string]string{
		`{"full":"yes"}`: "The request body is not valid JSON.",
		`{"x":1}`:        "The request has a field this server does not know.",
	} {
		e := wantCode(t, send(t, f.s, http.MethodPost, "/api/archive/rescan", body), http.StatusBadRequest, "")
		if e.Error != msg {
			t.Errorf("%s: error %q, want %q", body, e.Error, msg)
		}
	}
	for _, body := range []string{`{}`, `{"full":false}`} { // normal passes
		wantStarted(t, f, body, true)
		last, _ := waitIdle(t, f)["last"].(map[string]any)
		if last["full"] == true {
			t.Errorf("%s started a full pass", body)
		}
	}
}

func TestArchiveChangedAndAccept(t *testing.T) {
	f := newFullServer(t)
	putArchive(t, f, "S/a.jpg", "aaa")
	scanned(t, f)
	putArchive(t, f, "S/a.jpg", "different") // an edit in place
	scanned(t, f)
	rec := send(t, f.s, http.MethodGet, "/api/archive/changed", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("changed: %d %s", rec.Code, rec.Body)
	}
	var cl indexer.ChangeList
	decodeInto(t, rec, &cl)
	if len(cl.Changes) != 1 || cl.LatestID < 1 || cl.Total != 1 {
		t.Fatalf("changes %+v", cl)
	}
	rec = send(t, f.s, http.MethodPost, "/api/archive/changed/accept", fmt.Sprintf(`{"upTo":%d}`, cl.LatestID))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"accepted":1}` {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body)
	}
	for body, msg := range map[string]string{
		`{}`:               "upTo must be a positive whole number.",
		`{"upTo":null}`:    "upTo must be a positive whole number.",
		`{"upTo":0}`:       "upTo must be a positive whole number.",
		`{"upTo":"x"}`:     "The request body is not valid JSON.",
		`{"upTo":1,"x":1}`: "The request has a field this server does not know.",
	} {
		e := wantCode(t, send(t, f.s, http.MethodPost, "/api/archive/changed/accept", body), http.StatusBadRequest, "")
		if e.Error != msg {
			t.Errorf("%s: error %q, want %q", body, e.Error, msg)
		}
	}
}

func TestArchiveAfterClose(t *testing.T) {
	f := newFullServer(t)
	f.s.Close()
	for _, p := range []string{"changed/accept", "pause", "resume"} {
		e := wantCode(t, send(t, f.s, http.MethodPost, "/api/archive/"+p, `{"upTo":1}`), http.StatusServiceUnavailable, "")
		if e.Error != "SD2NAS is shutting down" {
			t.Errorf("%s: error %q", p, e.Error)
		}
	}
	wantStarted(t, f, "", false)
}

func TestIndexerProgressEvent(t *testing.T) {
	f := newFullServer(t)
	for i := range 3 {
		putArchive(t, f, fmt.Sprintf("S/%d.jpg", i), fmt.Sprint("content", i))
	}
	// A real connection gets the snapshot first.
	sse, cancel := sseFrames(t, eventsServer(t, f.s)+"/api/events")
	defer cancel()
	var snap string
	for snap == "" {
		if fr := nextFrame(t, sse); strings.Contains(fr, "event: "+EventIndexerProgress) {
			snap = fr
		}
	}
	if !strings.Contains(snap, `"version"`) {
		t.Errorf("snapshot %q has no version", snap)
	}

	frames := collectFrames(t, f.s)
	has := func(sub string) bool {
		for _, fr := range frames() {
			if strings.Contains(fr, "event: "+EventIndexerProgress) && strings.Contains(fr, sub) {
				return true
			}
		}
		return false
	}
	waitFor := func(what, sub string) {
		t.Helper()
		pollUntil(t, what, func() bool { return has(sub) })
	}
	f.busy.Store(true)
	wantStarted(t, f, "", true)
	waitFor("a running frame", `"running":{`)
	if rec := send(t, f.s, http.MethodPost, "/api/archive/pause", ""); rec.Code != http.StatusOK {
		t.Fatalf("pause: %d %s", rec.Code, rec.Body)
	}
	waitFor("a paused frame", `"paused_by":"user"`)
	if rec := send(t, f.s, http.MethodPost, "/api/archive/resume", ""); rec.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body)
	}
	waitFor("a busy frame after resume", `"paused_by":"busy"`)
	f.busy.Store(false)
	waitIdle(t, f)
	waitFor("a finished frame", `"running":null`)
}

// A client connecting while publishes happen must not deadlock with the
// indexer's lock (its OnChange callback runs under it).
func TestIndexerSnapshotDuringPublishes(t *testing.T) {
	f := newFullServer(t)
	for i := range 20 {
		putArchive(t, f, fmt.Sprintf("S/%d.jpg", i), fmt.Sprint("content", i))
	}
	f.busy.Store(true)
	wantStarted(t, f, "", true)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			send(t, f.s, http.MethodPost, "/api/archive/pause", "")
			send(t, f.s, http.MethodPost, "/api/archive/resume", "")
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			ch, unsubscribe := f.s.hub.subscribe()
			if _, err := f.s.hub.snapshotFrames(); err != nil {
				t.Errorf("snapshot: %v", err)
			}
			unsubscribe()
			_ = ch
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("connecting subscribers deadlocked against indexer publishes")
	}
	close(stop)
	wg.Wait()
	f.busy.Store(false)
	waitIdle(t, f)
}

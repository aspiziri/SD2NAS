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
	var cl map[string]any
	decodeInto(t, rec, &cl)
	changes, _ := cl["changes"].([]any)
	latest, _ := cl["latest_id"].(float64)
	if _, ok := cl["version"].(float64); !ok || len(changes) != 1 || latest < 1 || cl["total"] != float64(1) {
		t.Fatalf("changed body %s", rec.Body)
	}
	ch, _ := changes[0].(map[string]any)
	for _, k := range []string{"id", "rel_path", "old_sha256", "sha256", "old_size", "size", "indexed_at", "changed_at"} {
		if _, ok := ch[k]; !ok {
			t.Errorf("change has no %q field: %v", k, ch)
		}
	}
	rec = send(t, f.s, http.MethodPost, "/api/archive/changed/accept", fmt.Sprintf(`{"upTo":%d}`, int64(latest)))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"accepted":1}` {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body)
	}
	for body, msg := range map[string]string{
		`{}`:               "upTo must be a positive whole number.",
		`{"upTo":null}`:    "upTo must be a positive whole number.",
		`{"upTo":0}`:       "upTo must be a positive whole number.",
		`{"upTo":"x"}`:     "upTo must be a positive whole number.",
		`{"upTo":1,"x":1}`: "The request has a field this server does not know.",
	} {
		e := wantCode(t, send(t, f.s, http.MethodPost, "/api/archive/changed/accept", body), http.StatusBadRequest, "")
		if e.Error != msg {
			t.Errorf("%s: error %q, want %q", body, e.Error, msg)
		}
	}
}

// fileList returns stats[key] (missing_files or ignored_files) as path -> sha256.
func fileList(t *testing.T, m map[string]any, key string) map[string]string {
	t.Helper()
	l, ok := m[key].([]any)
	if !ok {
		t.Fatalf("%s = %#v, want a list", key, m[key])
	}
	out := map[string]string{}
	for _, e := range l {
		f, _ := e.(map[string]any)
		rel, _ := f["rel_path"].(string)
		out[rel], _ = f["sha256"].(string)
	}
	return out
}

func TestArchiveIgnoreMissing(t *testing.T) {
	f := newFullServer(t)
	putArchive(t, f, "S/a.jpg", "aaa")
	putArchive(t, f, "S/b.jpg", "bbbb")
	scanned(t, f)
	for _, n := range []string{"a.jpg", "b.jpg"} {
		if err := os.Remove(filepath.Join(f.photos, "Archive", "S", n)); err != nil {
			t.Fatal(err)
		}
	}
	m := scanned(t, f)
	missing := fileList(t, m, "missing_files")
	if m["missing"] != float64(2) || len(missing) != 2 || m["ignored"] != float64(0) || len(fileList(t, m, "ignored_files")) != 0 {
		t.Fatalf("before ignore: missing %v %v, ignored %v %v", m["missing"], m["missing_files"], m["ignored"], m["ignored_files"])
	}
	a := missing["Archive/S/a.jpg"]
	version := m["version"]
	rec := send(t, f.s, http.MethodPost, "/api/archive/missing/ignore", `{"sha256":"`+a+`"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ignore: %d %s", rec.Code, rec.Body)
	}
	check := func(when string, wantMissing, wantIgnored []string) map[string]any {
		t.Helper()
		m := archiveStats(t, f)
		gotM, gotI := fileList(t, m, "missing_files"), fileList(t, m, "ignored_files")
		if m["missing"] != float64(len(wantMissing)) || m["ignored"] != float64(len(wantIgnored)) || len(gotM) != len(wantMissing) || len(gotI) != len(wantIgnored) {
			t.Fatalf("%s: missing %v %v, ignored %v %v; want %v, %v", when, m["missing"], gotM, m["ignored"], gotI, wantMissing, wantIgnored)
		}
		for _, p := range wantMissing {
			if _, ok := gotM[p]; !ok {
				t.Errorf("%s: %s not missing (%v)", when, p, gotM)
			}
		}
		for _, p := range wantIgnored {
			if _, ok := gotI[p]; !ok {
				t.Errorf("%s: %s not ignored (%v)", when, p, gotI)
			}
		}
		return m
	}
	m = check("after ignore", []string{"Archive/S/b.jpg"}, []string{"Archive/S/a.jpg"})
	if m["version"] == version {
		t.Errorf("version %v did not change, so open pages would not reload", version)
	}
	// It survives a rescan and a full re-index.
	scanned(t, f)
	check("after a rescan", []string{"Archive/S/b.jpg"}, []string{"Archive/S/a.jpg"})
	wantStarted(t, f, `{"full":true}`, true)
	waitIdle(t, f)
	check("after a re-index", []string{"Archive/S/b.jpg"}, []string{"Archive/S/a.jpg"})

	// Ignoring twice, or un-ignoring a file that is missing, is a conflict.
	e := wantCode(t, send(t, f.s, http.MethodPost, "/api/archive/missing/ignore", `{"sha256":"`+a+`"}`), http.StatusConflict, "wrong_state")
	if e.Error != "That file is no longer missing. Reload the page." {
		t.Errorf("ignore twice: error %q", e.Error)
	}
	e = wantCode(t, send(t, f.s, http.MethodPost, "/api/archive/missing/unignore", `{"sha256":"`+missing["Archive/S/b.jpg"]+`"}`), http.StatusConflict, "wrong_state")
	if e.Error != "That file is no longer ignored. Reload the page." {
		t.Errorf("un-ignore a missing file: error %q", e.Error)
	}

	if rec := send(t, f.s, http.MethodPost, "/api/archive/missing/unignore", `{"sha256":"`+a+`"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("un-ignore: %d %s", rec.Code, rec.Body)
	}
	check("after un-ignore", []string{"Archive/S/a.jpg", "Archive/S/b.jpg"}, nil)

	// An ignored file put back is archived again.
	if rec := send(t, f.s, http.MethodPost, "/api/archive/missing/ignore", `{"sha256":"`+a+`"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("ignore again: %d %s", rec.Code, rec.Body)
	}
	putArchive(t, f, "S/a.jpg", "aaa")
	scanned(t, f)
	m = check("after putting it back", []string{"Archive/S/b.jpg"}, nil)
	if m["files"] != float64(1) {
		t.Errorf("files %v, want 1 (a.jpg archived again)", m["files"])
	}
}

func TestArchiveIgnoreBadRequests(t *testing.T) {
	f := newFullServer(t)
	for _, p := range []string{"ignore", "unignore"} {
		for body, msg := range map[string]string{
			`{}`:                "sha256 must be 64 lowercase hex digits.",
			`{"sha256":"xyz"}`:  "sha256 must be 64 lowercase hex digits.",
			`{"sha256":1}`:      "The request body is not valid JSON.",
			`{"sha256":"a","x"`: "The request body is not valid JSON.",
			`{"x":1}`:           "The request has a field this server does not know.",
		} {
			e := wantCode(t, send(t, f.s, http.MethodPost, "/api/archive/missing/"+p, body), http.StatusBadRequest, "")
			if e.Error != msg {
				t.Errorf("%s %s: error %q, want %q", p, body, e.Error, msg)
			}
		}
		e := wantCode(t, send(t, f.s, http.MethodPost, "/api/archive/missing/"+p, `{"sha256":"`+strings.Repeat("ab", 32)+`"}`), http.StatusNotFound, "")
		if e.Error != "That file is no longer in the index." {
			t.Errorf("%s unknown: error %q", p, e.Error)
		}
	}
}

func TestArchiveAfterClose(t *testing.T) {
	f := newFullServer(t)
	f.s.Close()
	for p, body := range map[string]string{
		"changed/accept":   `{"upTo":1}`,
		"pause":            `{"upTo":1}`,
		"resume":           `{"upTo":1}`,
		"missing/ignore":   `{"sha256":"` + strings.Repeat("ab", 32) + `"}`,
		"missing/unignore": `{"sha256":"` + strings.Repeat("ab", 32) + `"}`,
	} {
		e := wantCode(t, send(t, f.s, http.MethodPost, "/api/archive/"+p, body), http.StatusServiceUnavailable, "")
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
	before := len(frames())
	if rec := send(t, f.s, http.MethodPost, "/api/archive/resume", ""); rec.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body)
	}
	pollUntil(t, "a busy frame after resume", func() bool {
		for _, fr := range frames()[before:] {
			if strings.Contains(fr, "event: "+EventIndexerProgress) && strings.Contains(fr, `"paused_by":"busy"`) {
				return true
			}
		}
		return false
	})
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
		close(stop) // let the pause/resume goroutine end
		t.Fatal("connecting subscribers deadlocked against indexer publishes")
	}
	close(stop)
	wg.Wait()
	f.busy.Store(false)
	waitIdle(t, f)
}

func TestAcceptUpToNotWhole(t *testing.T) {
	f := newFullServer(t)
	const sentence = "upTo must be a positive whole number."
	for _, body := range []string{`{"upTo":1.5}`, `{"upTo":"3"}`, `{"upTo":true}`, `{"upTo":0}`, `{"upTo":null}`, `{}`} {
		e := wantCode(t, send(t, f.s, http.MethodPost, "/api/archive/changed/accept", body), http.StatusBadRequest, "")
		if e.Error != sentence {
			t.Errorf("%s: error %q, want %q", body, e.Error, sentence)
		}
	}
	if rec := send(t, f.s, http.MethodPost, "/api/archive/changed/accept", `{"upTo":2}`); rec.Code != http.StatusOK {
		t.Errorf("upTo 2: %d %s", rec.Code, rec.Body)
	}
	e := wantCode(t, send(t, f.s, http.MethodPost, "/api/archive/changed/accept", `{"upTo":`), http.StatusBadRequest, "")
	if e.Error != "The request body is not valid JSON." {
		t.Errorf("bad JSON: error %q", e.Error)
	}
}

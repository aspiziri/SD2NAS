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
)

// missingImport imports the SONY card, promotes its JPG by hand and lets
// the archive copy vanish (missing), then imports the card again: that
// second import (returned) found the JPG missing from the archive.
func missingImport(t *testing.T, f fullServer) (viewForTest, string) {
	t.Helper()
	ctx := t.Context()
	importCard(t, f)
	var jpg, rel string
	for _, file := range folderFiles(t, f, listing(t, f).Folders[0].ID).Files {
		if file.Kind == "jpg" {
			jpg = file.SHA256
		}
	}
	a, err := f.db.AssetBySHA(ctx, jpg)
	if err != nil {
		t.Fatal(err)
	}
	rel = a.RelPath
	if err := os.Remove(filepath.Join(f.photos, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
	earlier := time.Now().Add(-time.Minute)
	if err := f.db.ArchiveAsset(ctx, jpg, "Archive/2025-05-30 - Trip/x.JPG", earlier); err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkMissing(ctx, jpg, earlier); err != nil {
		t.Fatal(err)
	}
	second := importCard(t, f)
	if row, err := f.db.Ingest(ctx, second.ID); err != nil || row.Counts.Missing != 1 || row.SafeToFormat {
		t.Fatalf("second import = %+v, %v; want 1 missing", row, err)
	}
	return second, jpg
}

type reimportMissingBody struct {
	Requested int          `json:"requested"`
	Started   *viewForTest `json:"started"`
	Message   string       `json:"message"`
}

func reimportMissing(t *testing.T, f fullServer, id int64) reimportMissingBody {
	t.Helper()
	rec := send(t, f.s, http.MethodPost, fmt.Sprintf("/api/ingest/%d/reimport-missing", id), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reimport-missing: status %d, body %s", rec.Code, rec.Body)
	}
	var res reimportMissingBody
	decodeInto(t, rec, &res)
	return res
}

func TestReimportMissingStarts(t *testing.T) {
	f := newFullServer(t)
	var (
		mu       sync.Mutex
		finished = map[int64]bool{}
	)
	f.s.addFinishedHook(func(in catalog.Ingest) {
		mu.Lock()
		defer mu.Unlock()
		finished[in.ID] = true
	})
	second, jpg := missingImport(t, f)

	rec := send(t, f.s, http.MethodGet, fmt.Sprintf("/api/ingest/%d/forget", second.ID), "")
	var sum map[string]any
	decodeInto(t, rec, &sum)
	if sum["missing"] != 1.0 || sum["restorable"] != 1.0 || sum["restore_requested"] != 0.0 {
		t.Errorf("summary before = %s", rec.Body)
	}

	res := reimportMissing(t, f, second.ID)
	if res.Requested != 1 || res.Started == nil || res.Message != "Importing 1 file again from SONY_A7IV." {
		t.Fatalf("response %+v", res)
	}
	pollUntil(t, "ingest.finished for the re-import", func() bool {
		f.s.feed.tick()
		mu.Lock()
		defer mu.Unlock()
		return finished[res.Started.ID]
	})
	row, err := f.db.Ingest(t.Context(), res.Started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Counts.New != 1 || row.Counts.Missing != 0 || !row.SafeToFormat {
		t.Errorf("re-import = %+v, want the JPG new and the card safe to format", row)
	}
	if a, err := f.db.AssetBySHA(t.Context(), jpg); err != nil || a.State != catalog.StateStaged || !strings.HasPrefix(a.RelPath, "Staging/") {
		t.Errorf("asset = %+v, %v; want it staged again", a, err)
	}
	if again := reimportMissing(t, f, second.ID); again.Requested != 0 || again.Started != nil ||
		again.Message != "Nothing to re-import: none of this import's files is still missing from the archive." {
		t.Errorf("after the restore: %+v", again)
	}
}

func TestReimportMissingNotMounted(t *testing.T) {
	f := newFullServer(t)
	second, jpg := missingImport(t, f)
	if err := os.RemoveAll(f.card); err != nil {
		t.Fatal(err)
	}
	res := reimportMissing(t, f, second.ID)
	if res.Requested != 1 || res.Started != nil || res.Message != "1 file will be imported again the next time you insert SONY_A7IV." {
		t.Errorf("response %+v", res)
	}
	if req, err := f.db.RestoreRequested(t.Context()); err != nil || !req[jpg] {
		t.Errorf("RestoreRequested = %v, %v; want the JPG kept for the next import", req, err)
	}
	rec := send(t, f.s, http.MethodGet, fmt.Sprintf("/api/ingest/%d/forget", second.ID), "")
	var sum map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil || sum["restorable"] != 1.0 || sum["restore_requested"] != 1.0 {
		t.Errorf("summary = %s, %v", rec.Body, err)
	}
}

func TestReimportMissingNothing(t *testing.T) {
	f := newFullServer(t)
	first := importCard(t, f)
	res := reimportMissing(t, f, first.ID)
	if res.Requested != 0 || res.Started != nil ||
		res.Message != "Nothing to re-import: none of this import's files is still missing from the archive." {
		t.Errorf("response %+v", res)
	}
}

func TestReimportMissingUnknownID(t *testing.T) {
	f := newFullServer(t)
	e := wantCode(t, send(t, f.s, http.MethodPost, "/api/ingest/999/reimport-missing", ""), http.StatusNotFound, "")
	if e.Error != "That import no longer exists." {
		t.Errorf("error %q", e.Error)
	}
}

func TestReimportMissingBadID(t *testing.T) {
	f := newFullServer(t)
	for _, id := range []string{"abc", "0", "-1", "1.5"} {
		e := wantCode(t, send(t, f.s, http.MethodPost, "/api/ingest/"+id+"/reimport-missing", ""), http.StatusBadRequest, "")
		if e.Error != "The import id must be a positive whole number." {
			t.Errorf("id %s: error %q", id, e.Error)
		}
	}
}

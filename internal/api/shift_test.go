package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aspiziri/SD2NAS/internal/staging"
)

// stagedShas is the hashes of folder id's files, and the name of the first.
func stagedShas(t *testing.T, f fullServer, id int64) ([]string, string) {
	t.Helper()
	var shas []string
	files := folderFiles(t, f, id).Files
	for _, file := range files {
		if file.CaptureTime == "" {
			t.Fatalf("the card's files should all have capture times: %+v", files)
		}
		shas = append(shas, file.SHA256)
	}
	if len(shas) == 0 {
		t.Fatal("no files")
	}
	return shas, files[0].Name
}

func shiftBody(t *testing.T, shas []string, seconds any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"fileShas": shas, "seconds": seconds})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestShiftEndpoint(t *testing.T) {
	f := newFullServer(t)
	a := ingested(t, f)
	shas, _ := stagedShas(t, f, a.ID)
	before := folderFiles(t, f, a.ID).Files

	rec := send(t, f.s, http.MethodPost, "/api/staging/shift", shiftBody(t, shas, 3600))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var raw map[string]json.RawMessage
	decodeInto(t, rec, &raw)
	for _, k := range []string{"shifted", "renamed", "moved", "skipped", "folder", "removed"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("body has no %q: %s", k, rec.Body)
		}
	}
	if _, ok := raw["wanted_name"]; ok {
		t.Errorf("wanted_name should be omitted: %s", rec.Body)
	}
	var res staging.ShiftResult
	decodeInto(t, rec, &res)
	if res.Shifted != len(shas) || res.Moved != 0 || res.Skipped != 0 || res.Removed || res.Folder.ID != a.ID {
		t.Errorf("result %+v", res)
	}
	after := map[string]staging.File{}
	for _, file := range folderFiles(t, f, a.ID).Files {
		after[file.SHA256] = file
	}
	for _, old := range before {
		got := after[old.SHA256]
		if got.CaptureTime == old.CaptureTime || got.CaptureTime == "" {
			t.Errorf("%s: capture %q, was %q", old.Name, got.CaptureTime, old.CaptureTime)
		}
	}
}

func TestShiftEndpointSentences(t *testing.T) {
	f := newFullServer(t)
	a := ingested(t, f)
	shas, _ := stagedShas(t, f, a.ID)
	// A second folder, so one request can name files from two.
	rec := send(t, f.s, http.MethodPost, "/api/staging/move", fmt.Sprintf(`{"fileShas":[%q],"newTitle":"Other"}`, shas[0]))
	if rec.Code != http.StatusOK {
		t.Fatalf("move: status %d, body %s", rec.Code, rec.Body)
	}
	one := shas[1:2]
	_, firstLeft := stagedShas(t, f, a.ID)
	for _, c := range []struct{ name, body, want string }{
		{"missing seconds", fmt.Sprintf(`{"fileShas":[%q]}`, one[0]), "Say how far to shift: seconds is required."},
		{"null seconds", fmt.Sprintf(`{"fileShas":[%q],"seconds":null}`, one[0]), "Say how far to shift: seconds is required."},
		{"zero", shiftBody(t, one, 0), "Shift by at least one second."},
		{"too far", shiftBody(t, one, staging.MaxShift+1), "Shift by at most 100 years."},
		{"too far back", shiftBody(t, one, -staging.MaxShift-1), "Shift by at most 100 years."},
		{"two folders", shiftBody(t, shas, 60), "Shift files from one folder at a time."},
		{"outside", shiftBody(t, one, -staging.MaxShift), "Shifting by that much would put " + firstLeft + " outside 1970–2099."},
		{"no files", shiftBody(t, []string{}, 60), "No files given."},
		{"not a hash", shiftBody(t, []string{"nope"}, 60), `"nope" is not a file hash.`},
		{"unknown field", fmt.Sprintf(`{"fileShas":[%q],"seconds":1,"x":1}`, one[0]), "The request has a field this server does not know."},
		{"fraction", fmt.Sprintf(`{"fileShas":[%q],"seconds":1.5}`, one[0]), "The request body is not valid JSON."},
		{"too large", `{"fileShas":["` + strings.Repeat("a", 5<<20) + `"],"seconds":1}`, "The request is too large."},
	} {
		e := wantCode(t, send(t, f.s, http.MethodPost, "/api/staging/shift", c.body), http.StatusBadRequest, "")
		if e.Error != c.want {
			t.Errorf("%s: error %q, want %q", c.name, e.Error, c.want)
		}
	}
}

func TestShiftEndpointBusy(t *testing.T) {
	f := newFullServer(t)
	a := ingested(t, f)
	shas, _ := stagedShas(t, f, a.ID)
	release, err := f.locks.TryLock(staging.HolderIngest, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	e := wantCode(t, send(t, f.s, http.MethodPost, "/api/staging/shift", shiftBody(t, shas, 60)), http.StatusConflict, "folder_busy")
	if e.FolderID != a.ID || e.Holder != "ingest" {
		t.Errorf("body %+v", e)
	}
}

package api

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/staging"
)

// importCard runs the SONY card import and returns its view.
func importCard(t *testing.T, f fullServer) viewForTest {
	t.Helper()
	rec := post(t, f.s, "?wait=1", cardBody(f.card))
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest: status %d, body %s", rec.Code, rec.Body)
	}
	var v viewForTest
	decodeInto(t, rec, &v)
	return v
}

// stagedFiles lists every regular file under Staging/.
func stagedFiles(t *testing.T, f fullServer) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(filepath.Join(f.photos, "Staging"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}

func TestForgetAPIRetestsCard(t *testing.T) {
	f := newFullServer(t)
	in := importCard(t, f)
	if in.Counts.New != 2 {
		t.Fatalf("first import: %+v", in)
	}
	url := fmt.Sprintf("/api/ingest/%d/forget", in.ID)

	rec := send(t, f.s, http.MethodGet, url, "")
	var sum staging.ForgetSummary
	decodeInto(t, rec, &sum)
	if rec.Code != http.StatusOK || sum.Staged != 2 || sum.Forgotten || sum.Reimportable != 0 {
		t.Fatalf("summary: status %d, %+v", rec.Code, sum)
	}

	rec = send(t, f.s, http.MethodPost, url, `{"deleteFiles":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("forget: status %d, body %s", rec.Code, rec.Body)
	}
	var res struct {
		Result staging.ForgetResult `json:"result"`
		Ingest catalog.Ingest       `json:"ingest"`
	}
	decodeInto(t, rec, &res)
	if res.Result.DeletedFiles != 2 || res.Result.Forgotten != 2 || res.Ingest.ForgottenAt.IsZero() || res.Ingest.ID != in.ID {
		t.Errorf("result %+v", res)
	}
	if files := stagedFiles(t, f); len(files) != 0 {
		t.Errorf("Staging still holds %v", files)
	}

	rec = send(t, f.s, http.MethodGet, "/api/ingests", "")
	var list struct {
		Ingests []struct {
			ID          int64  `json:"id"`
			ForgottenAt string `json:"forgotten_at"`
		} `json:"ingests"`
	}
	decodeInto(t, rec, &list)
	if len(list.Ingests) != 1 || list.Ingests[0].ForgottenAt == "" {
		t.Errorf("history %s", rec.Body)
	}

	e := wantCode(t, send(t, f.s, http.MethodPost, url, `{"deleteFiles":false}`), http.StatusConflict, "already_forgotten")
	if e.Error != "This import was already forgotten." {
		t.Errorf("error %q", e.Error)
	}

	if again := importCard(t, f); again.Counts.New != 2 {
		t.Errorf("after forgetting, the card imports %+v, want 2 new", again)
	}
}

func TestForgetAPIBadRequests(t *testing.T) {
	f := newFullServer(t)
	in := importCard(t, f)
	url := fmt.Sprintf("/api/ingest/%d/forget", in.ID)

	e := wantCode(t, send(t, f.s, http.MethodPost, url, `{}`), http.StatusBadRequest, "")
	if !strings.Contains(e.Error, "deleteFiles") {
		t.Errorf("error %q", e.Error)
	}
	wantCode(t, send(t, f.s, http.MethodPost, url, `{"deleteFiles":true,"x":1}`), http.StatusBadRequest, "")
	wantCode(t, send(t, f.s, http.MethodPost, "/api/ingest/abc/forget", `{"deleteFiles":true}`), http.StatusBadRequest, "")
	wantCode(t, send(t, f.s, http.MethodGet, "/api/ingest/abc/forget", ""), http.StatusBadRequest, "")
	wantCode(t, send(t, f.s, http.MethodPost, "/api/ingest/abc/reimport-culled", ""), http.StatusBadRequest, "")
	e = wantCode(t, send(t, f.s, http.MethodPost, "/api/ingest/999/forget", `{"deleteFiles":true}`), http.StatusNotFound, "")
	if e.Error != "That import no longer exists." {
		t.Errorf("error %q", e.Error)
	}
	wantCode(t, send(t, f.s, http.MethodGet, "/api/ingest/999/forget", ""), http.StatusNotFound, "")
	wantCode(t, send(t, f.s, http.MethodPost, "/api/ingest/999/reimport-culled", ""), http.StatusNotFound, "")
}

func TestForgetAPIRefusesWhileImporting(t *testing.T) {
	f := newFullServer(t)
	in := importCard(t, f)

	release, err := f.mgr.Exclusive()
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	// The hold keeps the import queued, so the manager is busy.
	if rec := post(t, f.s, "", cardBody(f.card)); rec.Code >= 400 {
		t.Fatalf("submit: status %d, body %s", rec.Code, rec.Body)
	}
	e := wantCode(t, send(t, f.s, http.MethodPost, fmt.Sprintf("/api/ingest/%d/forget", in.ID), `{"deleteFiles":true}`), http.StatusConflict, "ingest_running")
	if e.Error != "An import is running or waiting. Try again when it finishes." {
		t.Errorf("error %q", e.Error)
	}
	wantCode(t, send(t, f.s, http.MethodPost, fmt.Sprintf("/api/ingest/%d/reimport-culled", in.ID), ""), http.StatusConflict, "ingest_running")
	release()
	released = true
	pollUntil(t, "the queued import to finish", func() bool {
		rows, err := f.db.Ingests(t.Context(), 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows) == 2 && rows[0].Status.Terminal()
	})
}

func TestReimportCulledAPI(t *testing.T) {
	f := newFullServer(t)
	first := importCard(t, f)
	var jpg string
	for _, file := range folderFiles(t, f, listing(t, f).Folders[0].ID).Files {
		if file.Kind == "jpg" {
			jpg = file.SHA256
		}
	}
	if jpg == "" {
		t.Fatal("no jpg in the first import")
	}
	cull := func() {
		t.Helper()
		rec := send(t, f.s, http.MethodPost, "/api/staging/cull", fmt.Sprintf(`{"fileShas":[%q]}`, jpg))
		if rec.Code != http.StatusOK {
			t.Fatalf("cull: status %d, body %s", rec.Code, rec.Body)
		}
	}
	_ = first
	cull()

	second := importCard(t, f) // the culled JPG is seen_before
	if second.ID != 2 {
		t.Fatalf("second import id %d", second.ID)
	}
	var sum staging.ForgetSummary
	decodeInto(t, send(t, f.s, http.MethodGet, "/api/ingest/2/forget", ""), &sum)
	if sum.Reimportable != 1 {
		t.Fatalf("summary %+v", sum)
	}

	rec := send(t, f.s, http.MethodPost, "/api/ingest/2/reimport-culled", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reimport: status %d, body %s", rec.Code, rec.Body)
	}
	var res struct {
		Forgotten int          `json:"forgotten"`
		Started   *viewForTest `json:"started"`
		Message   string       `json:"message"`
	}
	decodeInto(t, rec, &res)
	if res.Forgotten != 1 || res.Started == nil || !strings.HasPrefix(res.Message, "Importing 1 file again from ") {
		t.Fatalf("response %s", rec.Body)
	}
	pollUntil(t, "the re-import to finish and stage the JPG", func() bool {
		row, err := f.db.Ingest(t.Context(), res.Started.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !row.Status.Terminal() {
			return false
		}
		a, err := f.db.AssetBySHA(t.Context(), jpg)
		return err == nil && a.State == catalog.StateStaged
	})

	// Cull it again; this time the card is gone when "Re-import anyway" runs.
	cull()
	fourth := importCard(t, f)
	if fourth.ID != 4 {
		t.Fatalf("fourth import id %d", fourth.ID)
	}
	if err := os.RemoveAll(f.card); err != nil {
		t.Fatal(err)
	}
	rec = send(t, f.s, http.MethodPost, "/api/ingest/4/reimport-culled", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reimport without the card: status %d, body %s", rec.Code, rec.Body)
	}
	res.Started = nil
	decodeInto(t, rec, &res)
	if res.Forgotten != 1 || res.Started != nil || !strings.Contains(res.Message, "next time") {
		t.Errorf("response %s", rec.Body)
	}

	rec = send(t, f.s, http.MethodPost, "/api/ingest/4/reimport-culled", "")
	res.Started = nil
	decodeInto(t, rec, &res)
	if rec.Code != http.StatusOK || res.Forgotten != 0 || res.Started != nil || !strings.Contains(res.Message, "Nothing to re-import") {
		t.Errorf("nothing culled: status %d, body %s", rec.Code, rec.Body)
	}
}

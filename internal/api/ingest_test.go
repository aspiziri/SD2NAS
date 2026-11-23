package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/ingest"
	"github.com/aspiziri/SD2NAS/internal/media/mediatest"
)

func plenty(string) (uint64, error) { return 1e12, nil }

// newIngestServer serves the ingest API over a fresh catalog, with the
// committed SONY_A7IV card copied under a temp cards root.
func newIngestServer(t *testing.T, free func(string) (uint64, error), maxWait time.Duration) (s *Server, card string) {
	t.Helper()
	cards, photos, config := t.TempDir(), t.TempDir(), t.TempDir()
	card, err := mediatest.CopyCard(filepath.Join("..", "..", "testdata"), "SONY_A7IV", cards)
	if err != nil {
		t.Fatal(err)
	}
	db, err := catalog.Open(filepath.Join(config, "sd2nas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	runner := ingest.NewRunner(ingest.Config{Catalog: db, PhotosRoot: photos, FreeSpace: free, Log: slog.New(slog.DiscardHandler)})
	m := ingest.NewManager(runner, cards)
	m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	s = New("test", nil)
	s.MountIngest(m, db, maxWait)
	t.Cleanup(s.Close)
	return s, card
}

func post(t *testing.T, s *Server, query, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/ingest"+query, strings.NewReader(body))
	req.Header.Set("X-SD2NAS", "1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func cardBody(card string) string {
	return fmt.Sprintf(`{"path":%q,"label":"SONY_A7IV","uuid":"1234-ABCD"}`, card)
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

func pollUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type viewForTest struct {
	ID           int64  `json:"id"`
	Status       string `json:"status"`
	SafeToFormat bool   `json:"safe_to_format"`
	Level        string `json:"level"`
	Counts       struct {
		New int `json:"new"`
	} `json:"counts"`
	Progress *struct {
		Phase string `json:"phase"`
	} `json:"progress"`
	Files []struct {
		Outcome string `json:"outcome"`
		RelPath string `json:"rel_path"`
	} `json:"files"`
}

func TestIngestWaitText(t *testing.T) {
	s, card := newIngestServer(t, plenty, time.Minute)
	for _, want := range []string{
		"normal\nSONY_A7IV: 2 new, 0 already known. Card safe to format.\n",
		"normal\nSONY_A7IV: 0 new, 2 already known. Card safe to format.\n",
	} {
		rec := post(t, s, "?wait=1&format=text", cardBody(card))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %q", rec.Code, rec.Body)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
			t.Errorf("content type %q", ct)
		}
		if rec.Body.String() != want {
			t.Errorf("body %q, want %q", rec.Body, want)
		}
	}
}

func TestIngestWaitJSON(t *testing.T) {
	s, card := newIngestServer(t, plenty, time.Minute)
	rec := post(t, s, "?wait=1", cardBody(card))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var v viewForTest
	decodeInto(t, rec, &v)
	if v.Status != "done" || !v.SafeToFormat || v.Counts.New != 2 || v.Level != "normal" || v.ID <= 0 {
		t.Errorf("view: %+v", v)
	}
}

func TestIngestNoWait(t *testing.T) {
	s, card := newIngestServer(t, plenty, time.Minute)
	rec := post(t, s, "", cardBody(card))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var v viewForTest
	decodeInto(t, rec, &v)
	if v.ID <= 0 || (v.Status != "queued" && v.Status != "running" && v.Status != "done") {
		t.Fatalf("view: %+v", v)
	}
	var detail viewForTest
	var raw map[string]json.RawMessage
	pollUntil(t, "ingest to finish", func() bool {
		r := get(t, s, fmt.Sprintf("/api/ingests/%d", v.ID))
		detail = viewForTest{}
		decodeInto(t, r, &detail)
		raw = nil
		decodeInto(t, r, &raw)
		return detail.Status == "done"
	})
	if _, ok := raw["files"]; ok {
		t.Errorf("files should be omitted when every file is new: %s", raw["files"])
	}
	if detail.Counts.New != 2 {
		t.Errorf("counts.new = %d", detail.Counts.New)
	}
}

func TestIngestRefusedIs507(t *testing.T) {
	s, card := newIngestServer(t, func(string) (uint64, error) { return 0, nil }, time.Minute)
	rec := post(t, s, "?wait=1&format=text", cardBody(card))
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body)
	}
	lines := strings.Split(rec.Body.String(), "\n")
	if lines[0] != "alert" || !strings.HasPrefix(lines[1], "SONY_A7IV: not imported, staging would drop below the 50.0 GB free-space reserve") {
		t.Errorf("body %q", rec.Body)
	}
}

func TestIngestBadRequests(t *testing.T) {
	s, card := newIngestServer(t, plenty, time.Minute)
	req := httptest.NewRequest(http.MethodPost, "/api/ingest", strings.NewReader(cardBody(card)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no header: status %d", rec.Code)
	}
	outside := t.TempDir()
	for name, body := range map[string]string{
		"malformed":     `{`,
		"unknown field": `{"path":"x","extra":1}`,
		"empty path":    `{"path":""}`,
		"outside cards": fmt.Sprintf(`{"path":%q}`, outside),
	} {
		rec := post(t, s, "", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, body %s", name, rec.Code, rec.Body)
			continue
		}
		var e struct {
			Error string `json:"error"`
		}
		decodeInto(t, rec, &e)
		if e.Error == "" {
			t.Errorf("%s: no error in %s", name, rec.Body)
		}
	}
	var list struct {
		Ingests []json.RawMessage `json:"ingests"`
	}
	decodeInto(t, get(t, s, "/api/ingests"), &list)
	if len(list.Ingests) != 0 {
		t.Errorf("ingest rows were created: %v", list.Ingests)
	}
}

func TestIngestWaitTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s, card := newIngestServer(t, func(string) (uint64, error) { <-release; return 1e12, nil }, 50*time.Millisecond)
	rec := post(t, s, "?wait=1&format=text", cardBody(card))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body)
	}
	if want := "alert\nSONY_A7IV: still importing after 50ms. Card left mounted.\n"; rec.Body.String() != want {
		t.Errorf("body %q, want %q", rec.Body, want)
	}
	if rec := get(t, s, "/healthz"); rec.Code == 0 {
		t.Error("healthz did not answer")
	}
	pollUntil(t, "the job to be classifying", func() bool {
		rec := get(t, s, "/api/ingest/current")
		if rec.Code != http.StatusOK {
			return false
		}
		var cur struct {
			Running *viewForTest `json:"running"`
		}
		decodeInto(t, rec, &cur)
		return cur.Running != nil && cur.Running.Progress != nil && cur.Running.Progress.Phase == "classifying"
	})
}

func TestIngestHistory(t *testing.T) {
	s, card := newIngestServer(t, plenty, time.Minute)
	for range 2 {
		if rec := post(t, s, "?wait=1", cardBody(card)); rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body)
		}
	}
	ids := func(query string) string {
		t.Helper()
		rec := get(t, s, "/api/ingests"+query)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, body %s", query, rec.Code, rec.Body)
		}
		var list struct {
			Ingests []viewForTest `json:"ingests"`
		}
		decodeInto(t, rec, &list)
		var out []int64
		for _, v := range list.Ingests {
			out = append(out, v.ID)
		}
		return fmt.Sprint(out)
	}
	for query, want := range map[string]string{"": "[2 1]", "?limit=1": "[2]", "?before=2": "[1]"} {
		if got := ids(query); got != want {
			t.Errorf("ids(%q) = %s, want %s", query, got, want)
		}
	}
	if rec := get(t, s, "/api/ingests?limit=x"); rec.Code != http.StatusBadRequest {
		t.Errorf("limit=x: status %d", rec.Code)
	}
	rec := get(t, s, "/api/ingests/2")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: status %d, body %s", rec.Code, rec.Body)
	}
	var v viewForTest
	decodeInto(t, rec, &v)
	if len(v.Files) != 2 {
		t.Fatalf("files: %+v", v.Files)
	}
	for _, f := range v.Files {
		if f.Outcome != "in_staging" || f.RelPath == "" {
			t.Errorf("file: %+v", f)
		}
	}
	if rec := get(t, s, "/api/ingests/999"); rec.Code != http.StatusNotFound {
		t.Errorf("999: status %d", rec.Code)
	}
	if rec := get(t, s, "/api/ingests/abc"); rec.Code != http.StatusBadRequest {
		t.Errorf("abc: status %d", rec.Code)
	}
}

func TestIngestCurrentIdle(t *testing.T) {
	s, _ := newIngestServer(t, plenty, time.Minute)
	rec := get(t, s, "/api/ingest/current")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var cur struct {
		Running *viewForTest `json:"running"`
		Queued  []any        `json:"queued"`
	}
	decodeInto(t, rec, &cur)
	if cur.Running != nil || cur.Queued == nil || len(cur.Queued) != 0 {
		t.Errorf("current: %+v", cur)
	}
}

package api

import (
	"context"
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

// wantAlert checks rec is the card script's two-line alert with status code.
func wantAlert(t *testing.T, rec *httptest.ResponseRecorder, code int) string {
	t.Helper()
	if rec.Code != code {
		t.Errorf("status %d, want %d (body %q)", rec.Code, code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content type %q, want text/plain", ct)
	}
	body := rec.Body.String()
	lines := strings.Split(body, "\n")
	if len(lines) != 3 || lines[0] != ingest.LevelAlert || lines[1] == "" || lines[2] != "" {
		t.Errorf("body %q is not \"alert\n<message>\n\"", body)
		return ""
	}
	return lines[1]
}

// wantJSONError checks rec is a JSON error with status code.
func wantJSONError(t *testing.T, rec *httptest.ResponseRecorder, code int) string {
	t.Helper()
	if rec.Code != code {
		t.Errorf("status %d, want %d (body %q)", rec.Code, code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q, want application/json", ct)
	}
	var e struct {
		Error string `json:"error"`
	}
	decodeInto(t, rec, &e)
	if e.Error == "" {
		t.Errorf("no error in %q", rec.Body)
	}
	return e.Error
}

func TestWriteFail(t *testing.T) {
	codes := []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError, http.StatusServiceUnavailable}
	for _, code := range codes {
		for _, query := range []string{"", "?format=json", "?format=text", "?format=text&wait=1"} {
			t.Run(fmt.Sprintf("%d%s", code, query), func(t *testing.T) {
				rec := httptest.NewRecorder()
				writeFail(rec, httptest.NewRequest(http.MethodPost, "/api/ingest"+query, nil), code, "it\nbroke\r\n")
				if rec.Header().Get("Cache-Control") != "no-store" {
					t.Errorf("Cache-Control %q", rec.Header().Get("Cache-Control"))
				}
				if strings.Contains(query, "format=text") {
					if msg := wantAlert(t, rec, code); msg != "it broke" {
						t.Errorf("message %q, want %q", msg, "it broke")
					}
					return
				}
				if msg := wantJSONError(t, rec, code); msg != "it\nbroke\r\n" {
					t.Errorf("JSON error %q, want the message unchanged", msg)
				}
			})
		}
	}
}

func TestFinishedCode(t *testing.T) {
	for _, tc := range []struct {
		status catalog.IngestStatus
		want   int
	}{
		{catalog.StatusDone, http.StatusOK},
		{catalog.StatusRefused, http.StatusInsufficientStorage},
		{catalog.StatusFailed, http.StatusInternalServerError},
		{catalog.StatusCancelled, http.StatusServiceUnavailable},
		{catalog.StatusQueued, http.StatusInternalServerError},
		{catalog.StatusRunning, http.StatusInternalServerError},
		{"bogus", http.StatusInternalServerError},
		{"", http.StatusInternalServerError},
	} {
		if got := finishedCode(tc.status); got != tc.want {
			t.Errorf("finishedCode(%q) = %d, want %d", tc.status, got, tc.want)
		}
	}
}

func TestIngestErrorsText(t *testing.T) {
	s, card := newIngestServer(t, plenty, time.Minute)
	outside := t.TempDir()

	t.Run("guard 403", func(t *testing.T) {
		for _, query := range []string{"?format=text", "?wait=1&format=text"} {
			req := httptest.NewRequest(http.MethodPost, "/api/ingest"+query, strings.NewReader(cardBody(card)))
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if msg := wantAlert(t, rec, http.StatusForbidden); msg != "missing X-SD2NAS: 1 header" {
				t.Errorf("%s: message %q", query, msg)
			}
			req = httptest.NewRequest(http.MethodPost, "/api/ingest"+query, strings.NewReader(cardBody(card)))
			req.Header.Set("X-SD2NAS", "1")
			req.Header.Set("Origin", "http://evil.example")
			rec = httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if msg := wantAlert(t, rec, http.StatusForbidden); msg != "cross-origin request refused" {
				t.Errorf("%s cross-origin: message %q", query, msg)
			}
		}
	})
	for name, tc := range map[string]struct{ query, body string }{
		"malformed":     {"?wait=1&format=text", `{`},
		"unknown field": {"?wait=1&format=text", `{"path":"x","extra":1}`},
		"empty path":    {"?wait=1&format=text", `{"path":""}`},
		"outside cards": {"?wait=1&format=text", fmt.Sprintf(`{"path":%q}`, outside)},
		"bad wait":      {"?wait=yes&format=text", cardBody(card)},
	} {
		t.Run(name, func(t *testing.T) {
			wantAlert(t, post(t, s, tc.query, tc.body), http.StatusBadRequest)
		})
	}
}

func TestIngestBadWait(t *testing.T) {
	s, card := newIngestServer(t, plenty, time.Minute)
	for _, wait := range []string{"yes", "2", "", "TRUE", "on"} {
		t.Run(wait, func(t *testing.T) {
			if msg := wantJSONError(t, post(t, s, "?wait="+wait, cardBody(card)), http.StatusBadRequest); !strings.Contains(msg, "wait") {
				t.Errorf("error %q does not mention wait", msg)
			}
			if msg := wantAlert(t, post(t, s, "?format=text&wait="+wait, cardBody(card)), http.StatusBadRequest); !strings.Contains(msg, "wait") {
				t.Errorf("text message %q does not mention wait", msg)
			}
		})
	}
	var list struct {
		Ingests []any `json:"ingests"`
	}
	decodeInto(t, get(t, s, "/api/ingests"), &list)
	if len(list.Ingests) != 0 {
		t.Fatalf("a bad wait created ingests: %v", list.Ingests)
	}
	for _, query := range []string{"", "?wait=0", "?wait=false"} {
		rec := post(t, s, query, cardBody(card))
		if rec.Code != http.StatusAccepted {
			t.Errorf("%q: status %d, want 202 (body %s)", query, rec.Code, rec.Body)
		}
	}
	rec := post(t, s, "?wait=true", cardBody(card))
	if rec.Code != http.StatusOK {
		t.Errorf("wait=true: status %d, want 200 (body %s)", rec.Code, rec.Body)
	}
}

// newBrokenIngestServer serves the ingest API over a manager that is already
// closed (Submit returns ErrClosed) or over a closed catalog (Submit fails
// to record the ingest).
func newBrokenIngestServer(t *testing.T, closeManager bool) (s *Server, card string) {
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
	m := ingest.NewManager(ingest.NewRunner(ingest.Config{Catalog: db, PhotosRoot: photos, FreeSpace: plenty,
		Log: slog.New(slog.DiscardHandler)}), cards)
	if closeManager {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s = New("test", nil)
	s.MountIngest(m, db, time.Minute)
	t.Cleanup(s.Close)
	return s, card
}

func TestIngestSubmitErrors(t *testing.T) {
	for _, tc := range []struct {
		name         string
		closeManager bool
		code         int
	}{
		{"closed manager", true, http.StatusServiceUnavailable},
		{"catalog error", false, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, card := newBrokenIngestServer(t, tc.closeManager)
			wantJSONError(t, post(t, s, "?wait=1", cardBody(card)), tc.code)
			wantAlert(t, post(t, s, "?wait=1&format=text", cardBody(card)), tc.code)
		})
	}
}

func TestIngestWaitTimeoutJSON(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s, card := newIngestServer(t, func(string) (uint64, error) { <-release; return 1e12, nil }, 50*time.Millisecond)
	rec := post(t, s, "?wait=1", cardBody(card))
	if msg := wantJSONError(t, rec, http.StatusGatewayTimeout); msg != "SONY_A7IV: still importing after 50ms. Card left mounted." {
		t.Errorf("error %q", msg)
	}
	var v struct {
		ID int64 `json:"id"`
	}
	decodeInto(t, rec, &v)
	if v.ID <= 0 {
		t.Errorf("no job id in %s", rec.Body)
	}
}

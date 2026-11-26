package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/ingest"
	"github.com/aspiziri/SD2NAS/internal/media/mediatest"
	"github.com/aspiziri/SD2NAS/internal/notices"
	"github.com/aspiziri/SD2NAS/internal/staging"
)

func newNoticeServer(t *testing.T, queued int) (*Server, *notices.Service) {
	t.Helper()
	db, err := catalog.Open(filepath.Join(t.TempDir(), "sd2nas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	levels := []string{catalog.NoticeWarning, catalog.NoticeAlert}
	for i := range queued {
		_, err := db.AddNotification(context.Background(), catalog.Notification{
			Key: "k" + string(rune('a'+i)), Level: levels[i%2], Message: "message " + string(rune('a'+i)),
			CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	n, err := notices.New(notices.Config{DB: db, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	s := New("test", nil)
	s.SetLogger(slog.New(slog.DiscardHandler))
	s.MountNotifications(n)
	t.Cleanup(s.Close)
	return s, n
}

func TestSettingsPutNudgesNotices(t *testing.T) {
	f := newFullServer(t)
	listed := make(chan struct{}, 10)
	var days atomic.Int64
	noon := time.Date(2026, 11, 23, 12, 0, 0, 0, time.Local)
	n, err := notices.New(notices.Config{DB: f.db, Log: slog.New(slog.DiscardHandler), Every: time.Hour,
		// every check falls on a new day, after the notice time, so every check lists staging
		Now: func() time.Time { return noon.AddDate(0, 0, int(days.Add(1))) },
		Staging: func(context.Context) (staging.Listing, error) {
			select {
			case listed <- struct{}{}:
			default:
			}
			return staging.Listing{}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	f.s.MountNotifications(n) // f.s.Close closes n
	n.Start()
	select {
	case <-listed:
	case <-time.After(10 * time.Second):
		t.Fatal("the first check did not list staging within 10 s")
	}
	if rec := send(t, f.s, http.MethodPut, "/api/settings", `{"staging_notice_at":"00:00"}`); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	select {
	case <-listed: // within an hour no tick could have caused it
	case <-time.After(10 * time.Second):
		t.Fatal("PUT /api/settings did not make the notices loop check within 10 s")
	}
}

func noticeReq(s *Server, method, target string, guarded bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if guarded {
		req.Header.Set("X-SD2NAS", "1")
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestNotificationsAPI(t *testing.T) {
	s, _ := newNoticeServer(t, 2)

	rec := noticeReq(s, http.MethodGet, "/api/notifications", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("pending: status %d, body %s", rec.Code, rec.Body)
	}
	var pend struct {
		Pending []catalog.Notification `json:"pending"`
	}
	decodeInto(t, rec, &pend)
	if len(pend.Pending) != 2 {
		t.Fatalf("pending = %+v", pend.Pending)
	}
	for _, n := range pend.Pending {
		if n.ID == 0 || n.Key == "" || n.Level == "" || n.Message == "" || n.CreatedAt.IsZero() {
			t.Errorf("incomplete notice %+v", n)
		}
	}
	var raw map[string][]map[string]any
	decodeInto(t, rec, &raw)
	for _, f := range []string{"id", "key", "level", "message", "created_at"} {
		if _, ok := raw["pending"][0][f]; !ok {
			t.Errorf("pending item lacks %q", f)
		}
	}

	rec = noticeReq(s, http.MethodPost, "/api/notifications/take?format=text", true)
	want := pend.Pending[0].Level + "\n" + pend.Pending[0].Message + "\n" +
		pend.Pending[1].Level + "\n" + pend.Pending[1].Message + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("take: status %d, body %q, want %q", rec.Code, rec.Body, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}

	rec = noticeReq(s, http.MethodPost, "/api/notifications/take?format=text", true)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("second take: status %d, body %q", rec.Code, rec.Body)
	}
	rec = noticeReq(s, http.MethodPost, "/api/notifications/take", true)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"notifications":[]}` {
		t.Errorf("JSON take: status %d, body %q", rec.Code, rec.Body)
	}
	rec = noticeReq(s, http.MethodGet, "/api/notifications", false)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"pending":[]}` {
		t.Errorf("pending after take: status %d, body %q", rec.Code, rec.Body)
	}
}

func TestNotificationsTakeGuard(t *testing.T) {
	s, _ := newNoticeServer(t, 1)
	rec := noticeReq(s, http.MethodPost, "/api/notifications/take?format=text", false)
	if rec.Code != http.StatusForbidden {
		t.Errorf("unguarded take: status %d, body %q", rec.Code, rec.Body)
	}
	rec = noticeReq(s, http.MethodGet, "/api/notifications", false)
	var pend struct {
		Pending []catalog.Notification `json:"pending"`
	}
	decodeInto(t, rec, &pend)
	if len(pend.Pending) != 1 {
		t.Errorf("a refused take removed the notice: %+v", pend.Pending)
	}
}

func TestNotificationsAfterClose(t *testing.T) {
	s, _ := newNoticeServer(t, 1)
	s.Close()
	rec := noticeReq(s, http.MethodGet, "/api/notifications", false)
	if rec.Code != http.StatusServiceUnavailable || strings.TrimSpace(rec.Body.String()) != `{"error":"SD2NAS is shutting down"}` {
		t.Errorf("pending: status %d, body %q", rec.Code, rec.Body)
	}
	rec = noticeReq(s, http.MethodPost, "/api/notifications/take?format=text", true)
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "alert\nSD2NAS is shutting down\n" {
		t.Errorf("take: status %d, body %q", rec.Code, rec.Body)
	}
}

func TestNotificationsCloseFirst(t *testing.T) {
	db, err := catalog.Open(filepath.Join(t.TempDir(), "sd2nas.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	n, err := notices.New(notices.Config{DB: db, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	s := New("test", nil)
	var order []string
	s.closers = append(s.closers, func() {
		_, err := n.Take(context.Background())
		if !errors.Is(err, notices.ErrClosed) {
			t.Errorf("an earlier closer ran after notices closed: Take err = %v, want ErrClosed", err)
		}
		order = append(order, "earlier")
	})
	s.MountNotifications(n)
	s.Close()
	if len(order) != 1 {
		t.Fatalf("closers ran %v", order)
	}
}

func newSubmitServer(t *testing.T) (*Server, string) {
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
	runner := ingest.NewRunner(ingest.Config{Catalog: db, PhotosRoot: photos, FreeSpace: plenty, Log: slog.New(slog.DiscardHandler)})
	m := ingest.NewManager(runner, cards)
	m.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	s := New("test", nil)
	s.SetLogger(slog.New(slog.DiscardHandler))
	s.feedInterval = time.Hour
	s.MountIngest(m, db, time.Minute)
	t.Cleanup(s.Close)
	return s, card
}

func TestSubmitIngestWatched(t *testing.T) {
	s, card := newSubmitServer(t)
	got := make(chan int64, 4)
	s.addFinishedHook(func(in catalog.Ingest) { got <- in.ID })
	j, err := s.SubmitIngest(context.Background(), ingest.Request{Path: card})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-j.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the import did not finish")
	}
	s.feed.tick()
	select {
	case id := <-got:
		if id != j.ID() {
			t.Errorf("hook got ingest %d, want %d", id, j.ID())
		}
	default:
		t.Fatal("finished hook did not run for a job no tick saw running")
	}
}

func TestSubmitIngestWithoutMount(t *testing.T) {
	j, err := New("test", nil).SubmitIngest(context.Background(), ingest.Request{Path: "/x"})
	if err == nil || j != nil {
		t.Errorf("SubmitIngest = %v, %v; want an error", j, err)
	}
}

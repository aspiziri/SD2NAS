package api

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/ingest"
	"github.com/aspiziri/SD2NAS/internal/media/mediatest"
	"github.com/aspiziri/SD2NAS/internal/staging"
	"github.com/aspiziri/SD2NAS/internal/thumbs"
)

// fullServer serves ingest and staging (and, from Task 7, thumbnails) over
// a fresh catalog, with the committed SONY_A7IV card under a temp cards root.
type fullServer struct {
	s                    *Server
	card, photos, config string
	db                   *catalog.DB
	cache                *thumbs.Cache
	locks                *staging.Locks
	svc                  *staging.Service
	mgr                  *ingest.Manager
}

func newFullServer(t *testing.T) fullServer {
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
	discard := slog.New(slog.DiscardHandler)
	locks := staging.NewLocks()
	runner := ingest.NewRunner(ingest.Config{Catalog: db, PhotosRoot: photos, FreeSpace: plenty, Log: discard, Locks: locks})
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
	s.MountIngest(m, db, time.Minute)
	cache, err := thumbs.New(thumbs.Config{Dir: filepath.Join(config, "thumbs"), Log: discard})
	if err != nil {
		t.Fatal(err)
	}
	svc := staging.NewService(staging.Config{DB: db, PhotosRoot: photos, Locks: locks, Log: discard,
		Removed: func(shas []string) {
			for _, sha := range shas {
				if err := cache.Remove(sha); err != nil {
					t.Errorf("remove thumbnails of %s: %v", sha, err)
				}
			}
		}})
	s.MountStaging(svc) // s.Close closes svc
	s.MountForget(m, db, svc)
	s.MountThumbs(db, cache, photos)
	t.Cleanup(s.Close)
	return fullServer{s: s, card: card, photos: photos, config: config, db: db, cache: cache, locks: locks, svc: svc, mgr: m}
}

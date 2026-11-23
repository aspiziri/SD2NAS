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
	s.MountIngest(m, db, time.Minute)
	s.MountStaging(staging.New(db, photos))
	cache, err := thumbs.New(thumbs.Config{Dir: filepath.Join(config, "thumbs"), Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	s.MountThumbs(db, cache, photos)
	t.Cleanup(s.Close)
	return fullServer{s: s, card: card, photos: photos, config: config, db: db, cache: cache}
}

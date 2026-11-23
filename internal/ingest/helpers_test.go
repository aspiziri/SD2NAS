package ingest

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	mt "github.com/aspiziri/SD2NAS/internal/media/mediatest"
)

// writeFile writes root/rel (slash-separated), creating parent folders.
func writeFile(t *testing.T, root, rel string, data []byte) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o666); err != nil {
		t.Fatal(err)
	}
	return p
}

var testNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

type env struct {
	r      *Runner
	cat    *catalog.DB
	photos string
	cards  string
	free   uint64 // what FreeSpace reports
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfgDir := t.TempDir()
	cat, err := catalog.Open(filepath.Join(cfgDir, "sd2nas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	e := &env{cat: cat, photos: t.TempDir(), cards: t.TempDir(), free: 1e12}
	e.r = NewRunner(Config{
		Catalog:    cat,
		PhotosRoot: e.photos,
		Now:        func() time.Time { return testNow },
		FreeSpace:  func(string) (uint64, error) { return e.free, nil },
		Log:        slog.New(slog.DiscardHandler),
	})
	return e
}

// fixtureCard copies a committed card (SONY_A7IV or GOPRO) into e.cards.
func (e *env) fixtureCard(t *testing.T, label string) string {
	t.Helper()
	card, err := mt.CopyCard(filepath.Join("..", "..", "testdata"), label, e.cards)
	if err != nil {
		t.Fatal(err)
	}
	return card
}

// run creates the queued row for card and runs it to the end.
func (e *env) run(t *testing.T, card string) catalog.Ingest {
	t.Helper()
	in, err := e.cat.CreateIngest(t.Context(), card, filepath.Base(card), "1234-ABCD", testNow)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.r.Run(t.Context(), in, NewProgress())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// staged lists every regular file under photos/Staging (partials included)
// as slash paths relative to photos, sorted. It returns nil when there are
// no regular files, whether or not Staging or folders inside it exist.
func (e *env) staged(t *testing.T) []string {
	t.Helper()
	var out []string
	root := filepath.Join(e.photos, StagingDir)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root && os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(e.photos, p)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// files maps CardPath -> IngestFile for an ingest.
func (e *env) files(t *testing.T, id int64) map[string]catalog.IngestFile {
	t.Helper()
	rows, err := e.cat.IngestFiles(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	m := make(map[string]catalog.IngestFile, len(rows))
	for _, f := range rows {
		m[f.CardPath] = f
	}
	return m
}

// writeJPEG writes a synthesized JPEG carrying s at card/rel.
func writeJPEG(t *testing.T, card, rel string, s mt.ExifSpec) string {
	t.Helper()
	data, err := mt.JPEG(s)
	if err != nil {
		t.Fatal(err)
	}
	return writeFile(t, card, rel, data)
}

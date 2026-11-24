package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

func setup(t *testing.T) (*Service, *catalog.DB, string) {
	t.Helper()
	photos := t.TempDir()
	config := t.TempDir()
	db, err := catalog.Open(filepath.Join(config, "sd2nas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	s, err := New(Config{DB: db, PhotosRoot: photos})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, db, photos
}

// put writes photos/rel with the given content and whole-second mtime.
func put(t *testing.T, photos, rel, content string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(photos, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Unix(mtime.Unix(), 0)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	return p
}

func sum(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func sortStrings(s []string) { sort.Strings(s) }

// fakeLocks is a Locks over lower-cased held names, ancestor-aware like NameHeld.
type fakeLocks struct {
	mu   sync.Mutex
	held map[string]bool
}

func newFakeLocks() *fakeLocks { return &fakeLocks{held: map[string]bool{}} }

func (l *fakeLocks) hold(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held[strings.ToLower(strings.Trim(name, "/"))] = true
}

func (l *fakeLocks) release(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, strings.ToLower(strings.Trim(name, "/")))
}

func (l *fakeLocks) NameHeld(rel string) bool {
	k := strings.ToLower(strings.Trim(rel, "/"))
	l.mu.Lock()
	defer l.mu.Unlock()
	for k != "" {
		if l.held[k] {
			return true
		}
		i := strings.LastIndex(k, "/")
		if i < 0 {
			return false
		}
		k = k[:i]
	}
	return false
}

// listingResult runs a pass's listing only and returns what it found.
func listingResult(t *testing.T, s *Service, set catalog.Settings) ([]file, *ScanResult) {
	t.Helper()
	r := &ScanResult{}
	sc, err := s.newScan(t.Context(), set, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.walk(filepath.Join(s.photosRoot, ArchiveDir), ArchiveDir, 0); err != nil {
		t.Fatal(err)
	}
	return sc.fresh, r
}

func listing(t *testing.T, s *Service, set catalog.Settings) []file {
	t.Helper()
	got, _ := listingResult(t, s, set)
	return got
}

package staging

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

// opsNow is the service's clock in operation tests.
var opsNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// opsEnv is a Service over a temp photos share and catalog.
type opsEnv struct {
	svc     *Service
	db      *catalog.DB
	photos  string
	locks   *Locks
	mu      sync.Mutex
	removed []string // hashes passed to Config.Removed
	changes []Status // statuses passed to OnChange
}

func newOpsEnv(t *testing.T) *opsEnv {
	t.Helper()
	photos, config := t.TempDir(), t.TempDir()
	db, err := catalog.Open(filepath.Join(config, "sd2nas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	e := &opsEnv{db: db, photos: photos, locks: NewLocks()}
	e.svc = NewService(Config{DB: db, PhotosRoot: photos, Locks: e.locks, Now: func() time.Time { return opsNow },
		Log: slog.New(slog.DiscardHandler), Removed: func(shas []string) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.removed = append(e.removed, shas...)
		}})
	e.svc.OnChange(func(st Status) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.changes = append(e.changes, st)
	})
	t.Cleanup(e.svc.Close) // runs before the catalog closes
	return e
}

func shaOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// write writes content at rel (slash path below the photos root) and returns the OS path.
func (e *opsEnv) write(t *testing.T, rel, content string) string {
	t.Helper()
	p := filepath.Join(e.photos, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o666); err != nil {
		t.Fatal(err)
	}
	return p
}

// folder creates a staging folder row and its directory.
func (e *opsEnv) folder(t *testing.T, name string, dates ...string) catalog.StagingFolder {
	t.Helper()
	f, err := e.db.CreateStagingFolder(t.Context(), name, dates, opsNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.photos, Dir, name), 0o777); err != nil {
		t.Fatal(err)
	}
	return f
}

// stage writes content at rel inside folder f (e.g. "Capture/a.ARW") and
// records it as a staged asset; it returns the content hash.
func (e *opsEnv) stage(t *testing.T, f catalog.StagingFolder, rel, content string, capture time.Time) string {
	t.Helper()
	relPath := Dir + "/" + f.Name + "/" + rel
	e.write(t, relPath, content)
	kind, ok := media.DefaultRules().KindOf(rel)
	if !ok {
		t.Fatalf("%s is not a media file", rel)
	}
	sha := shaOf(content)
	a := catalog.Asset{SHA256: sha, Size: int64(len(content)), Kind: kind, CaptureTime: capture,
		State: catalog.StateStaged, RelPath: relPath, UpdatedAt: opsNow}
	if err := e.db.InsertAsset(t.Context(), a, shaOf("fp:"+content)); err != nil {
		t.Fatal(err)
	}
	return sha
}

// files lists every regular file below the photos root as sorted slash paths.
func (e *opsEnv) files(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(e.photos, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
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
	slices.Sort(out)
	return out
}

func (e *opsEnv) asset(t *testing.T, sha string) catalog.Asset {
	t.Helper()
	a, err := e.db.AssetBySHA(t.Context(), sha)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// version is the last status OnChange saw (0 if none).
func (e *opsEnv) version() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.changes) == 0 {
		return 0
	}
	return e.changes[len(e.changes)-1].Version
}

func (e *opsEnv) removedHashes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := slices.Clone(e.removed)
	slices.Sort(out)
	return out
}

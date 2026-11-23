package ingest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/aspiziri/SD2NAS/internal/media"
)

// mkdirDurable creates dir and any missing parents, then makes each new
// folder durable by syncing its parent. Existing folders are left alone.
func mkdirDurable(dir string) error {
	var missing []string // deepest first
	for p := filepath.Clean(dir); ; {
		info, err := os.Stat(p)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("mkdir %s: %s is not a directory", dir, p)
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		p := missing[i]
		if err := os.Mkdir(p, 0o777); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := media.SyncDir(filepath.Dir(p)); err != nil {
			return err
		}
	}
	return nil
}

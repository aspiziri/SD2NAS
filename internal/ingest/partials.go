package ingest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aspiziri/SD2NAS/internal/media"
)

// CleanPartials deletes the *.partial files an interrupted copy left below
// stagingRoot and returns them as slash-separated paths relative to
// stagingRoot, sorted. A missing stagingRoot is not an error.
func CleanPartials(stagingRoot string) ([]string, error) {
	if _, err := os.Stat(stagingRoot); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var removed []string
	err := filepath.WalkDir(stagingRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), media.PartialSuffix) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		rel, err := filepath.Rel(stagingRoot, path)
		if err != nil {
			return err
		}
		removed = append(removed, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(removed)
	if err != nil {
		return removed, fmt.Errorf("clean partials: %w", err)
	}
	return removed, nil
}

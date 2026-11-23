package media

import (
	"os"
	"path/filepath"
	"testing"
)

// fixture returns the path of a committed file under the repo's testdata/;
// rel is slash-separated.
func fixture(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.Join("..", "..", "testdata", filepath.FromSlash(rel))
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture %s: %v", rel, err)
	}
	return p
}

package ingest

import (
	"os"
	"path/filepath"
	"testing"
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

package ingest

import (
	"path/filepath"
	"testing"
)

func TestFreeSpace(t *testing.T) {
	dir := t.TempDir()
	n, err := FreeSpace(dir)
	if err != nil || n == 0 {
		t.Errorf("FreeSpace(dir) = %d, %v; want > 0, nil", n, err)
	}
	if _, err := FreeSpace(filepath.Join(dir, "missing")); err == nil {
		t.Error("FreeSpace(missing) = nil error, want an error")
	}
}

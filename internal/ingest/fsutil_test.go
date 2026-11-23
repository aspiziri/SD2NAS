package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMkdirDurable(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "a", "b", "c")
	if err := mkdirDurable(target); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("target not a directory: %v", err)
	}
	if err := mkdirDurable(target); err != nil {
		t.Errorf("existing directory: %v", err)
	}
	file := writeFile(t, base, "a/file", []byte("x"))
	if err := mkdirDurable(filepath.Join(file, "d")); err == nil {
		t.Error("below a regular file: err = nil, want an error")
	}
}

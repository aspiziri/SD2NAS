package media

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestSyncDir(t *testing.T) {
	dir := t.TempDir()
	if err := SyncDir(dir); err != nil {
		t.Errorf("SyncDir(dir) = %v, want nil", err)
	}
	if err := SyncDir(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("SyncDir(missing) = %v, want fs.ErrNotExist", err)
	}
	file := writeSourceFile(t, dir, "f.txt", "x")
	if err := SyncDir(file); err == nil {
		t.Error("SyncDir(regular file) = nil, want an error")
	}
}

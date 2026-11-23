package media

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixedMtime is the mtime every helper-written file gets, so each one
// resolves to the same capture date (and thus the same destination name)
// when it has no embedded time. Ported from CameraImport's importer_test.go.
var fixedMtime = time.Date(2026, 1, 2, 15, 4, 5, 0, time.Local)

// writeBytes writes dir/name and sets its atime and mtime to fixedMtime.
func writeBytes(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, fixedMtime, fixedMtime); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeSourceFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	return writeBytes(t, dir, name, []byte(content))
}

// pattern returns n bytes where b[i] = byte(i % 251).
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

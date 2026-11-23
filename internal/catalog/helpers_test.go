package catalog

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// openTest opens a fresh catalog in a temp dir and closes it at cleanup.
func openTest(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sd2nas.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	return d
}

// hexN returns a valid hash: the byte n repeated 32 times, as hex.
func hexN(n byte) string { return strings.Repeat(fmt.Sprintf("%02x", n), 32) }

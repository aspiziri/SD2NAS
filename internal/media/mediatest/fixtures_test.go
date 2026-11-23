package mediatest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the committed fixtures under testdata/")

// TestFixtures builds every fixture in memory. With -update it writes them to
// the repo's testdata/. Without it, it compares each build with the committed
// file: byte-exact when the generator involves no image/jpeg encoding, and,
// for the JPEG, over the EXIF APP1 segment only, so a Go version change to
// the JPEG encoder cannot cause failures.
// Regenerate: go test ./internal/media/mediatest -run TestFixtures -update
func TestFixtures(t *testing.T) {
	for _, f := range Fixtures() {
		data, err := f.Build()
		if err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		path := filepath.Join("..", "..", "..", "testdata", filepath.FromSlash(f.Path))
		if *update {
			if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("fixture %s unreadable: %v (rerun with -update: go test ./internal/media/mediatest -run TestFixtures -update)", f.Path, err)
			continue
		}
		want, got := committed, data
		if strings.EqualFold(filepath.Ext(f.Path), ".jpg") {
			// Compare only the metadata; the encoded pixels may differ across Go versions.
			if want, err = exifSegment(committed); err != nil {
				t.Errorf("fixture %s (committed): %v (rerun with -update)", f.Path, err)
				continue
			}
			if got, err = exifSegment(data); err != nil {
				t.Fatalf("%s (built): %v", f.Path, err)
			}
		}
		if !bytes.Equal(got, want) {
			t.Errorf("fixture %s is stale or corrupt: committed bytes differ from the generator's (rerun with -update: go test ./internal/media/mediatest -run TestFixtures -update)", f.Path)
		}
	}
}

// exifSegment returns the APP1 segment (marker, length and payload) that
// directly follows SOI in a JPEG built by JPEG().
func exifSegment(b []byte) ([]byte, error) {
	if len(b) < 6 || b[0] != 0xFF || b[1] != 0xD8 || b[2] != 0xFF || b[3] != 0xE1 {
		return nil, errors.New("does not start with SOI + APP1")
	}
	end := 4 + int(binary.BigEndian.Uint16(b[4:6]))
	if end > len(b) {
		return nil, errors.New("APP1 segment is truncated")
	}
	return b[2:end], nil
}

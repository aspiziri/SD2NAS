package mediatest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyCard(t *testing.T) {
	testdata := filepath.Join("..", "..", "..", "testdata")
	dst := t.TempDir()
	root, err := CopyCard(testdata, "SONY_A7IV", dst)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dst, "SONY_A7IV"); root != want {
		t.Errorf("root = %q, want %q", root, want)
	}
	for _, name := range []string{"DSC00001.JPG", "DSC00001.ARW"} {
		rel := filepath.Join("DCIM", "100MSDCF", name)
		src := filepath.Join(testdata, "cards", "SONY_A7IV", rel)
		got := filepath.Join(root, rel)
		a, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(got)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s: bytes differ", name)
		}
		si, _ := os.Stat(src)
		gi, _ := os.Stat(got)
		if !si.ModTime().Equal(gi.ModTime()) {
			t.Errorf("%s: ModTime %v, want %v", name, gi.ModTime(), si.ModTime())
		}
	}
}

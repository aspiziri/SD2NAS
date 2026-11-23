package ingest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCleanPartials(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "Staging")
	writeFile(t, staging, "2025-05-30/a.JPG.partial", []byte("x"))
	writeFile(t, staging, "2025-05-30/Capture/b.ARW.partial", []byte("x"))
	keep := writeFile(t, staging, "2025-05-30/c.JPG", []byte("x"))
	dir := filepath.Join(staging, "x.partial")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	got, err := CleanPartials(staging)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2025-05-30/Capture/b.ARW.partial", "2025-05-30/a.JPG.partial"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("removed = %v, want %v", got, want)
	}
	for _, rel := range want {
		if _, err := os.Stat(filepath.Join(staging, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s still exists (err %v)", rel, err)
		}
	}
	for _, p := range []string{keep, dir} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestCleanPartialsMissingRoot(t *testing.T) {
	got, err := CleanPartials(filepath.Join(t.TempDir(), "Staging"))
	if got != nil || err != nil {
		t.Errorf("CleanPartials(missing) = %v, %v; want nil, nil", got, err)
	}
}

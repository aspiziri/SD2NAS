package media

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	mt "github.com/aspiziri/SD2NAS/internal/media/mediatest"
)

// importOne is the M1 equivalent of CameraImport's importFile for one
// image going into destBase/shoot: capture time → name → collision loop →
// CopyHashed.
func importOne(t *testing.T, path, destBase string) {
	t.Helper()
	c, err := CaptureTime(path, KindImage)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(destBase, "shoot", TypeSubdir(KindImage, DefaultRules()))
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	dst, duplicate, err := FreeName(path, dir, DestFileName(c.Time, KindImage, nil, filepath.Ext(path)))
	if err != nil {
		t.Fatalf("FreeName(%s): %v", path, err)
	}
	if duplicate {
		return // identical file already imported
	}
	if _, err := CopyHashed(t.Context(), path, dst); err != nil {
		t.Fatalf("CopyHashed(%s): %v", path, err)
	}
}

func readDestDir(t *testing.T, destBase string) map[string]string {
	t.Helper()
	destDir := filepath.Join(destBase, "shoot")
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatal(err)
	}
	contents := make(map[string]string)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(destDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		contents[e.Name()] = string(data)
	}
	return contents
}

// Burst shots share a capture timestamp; each must land in its own file with
// no overwrites.
func TestImportFileCollisionDoesNotOverwrite(t *testing.T) {
	src := t.TempDir()
	dest := t.TempDir()

	paths := []string{
		writeSourceFile(t, src, "a.jpg", "first"),
		writeSourceFile(t, src, "b.jpg", "second, different content"),
		writeSourceFile(t, src, "c.jpg", "third, also different content here"),
	}
	for _, p := range paths {
		importOne(t, p, dest)
	}

	contents := readDestDir(t, dest)
	if len(contents) != 3 {
		t.Fatalf("expected 3 files at destination, got %d: %v", len(contents), contents)
	}
	seen := make(map[string]bool)
	for _, body := range contents {
		if seen[body] {
			t.Errorf("duplicate content at destination, a file was overwritten: %v", contents)
		}
		seen[body] = true
	}
	for _, name := range []string{"2026-01-02 15.04.05.jpg", "2026-01-02 15.04.05-1.jpg", "2026-01-02 15.04.05-2.jpg"} {
		if _, ok := contents[name]; !ok {
			t.Errorf("missing %q in %v", name, contents)
		}
	}
}

// Re-importing the same file must be skipped, not duplicated.
func TestImportFileSkipsIdenticalDuplicate(t *testing.T) {
	src := t.TempDir()
	dest := t.TempDir()

	path := writeSourceFile(t, src, "a.jpg", "same content")
	importOne(t, path, dest)
	importOne(t, path, dest)

	contents := readDestDir(t, dest)
	if len(contents) != 1 {
		t.Fatalf("expected identical re-import to be skipped, got %d files: %v", len(contents), contents)
	}
}

// The destination folder must come from the same capture time used for the
// filename, so the two always agree.
func TestImportFileFolderMatchesCaptureDate(t *testing.T) {
	path := writeSourceFile(t, t.TempDir(), "a.jpg", "content")
	c, err := CaptureTime(path, KindImage)
	if err != nil {
		t.Fatal(err)
	}
	if got := FolderDate(c.Time, 0); got != "2026-01-02" {
		t.Errorf("FolderDate(0) = %q", got)
	}
	if got := FolderDate(c.Time, DefaultDayStartsAt); got != "2026-01-02" {
		t.Errorf("FolderDate(DefaultDayStartsAt) = %q", got)
	}
	if got := DestFileName(c.Time, KindImage, nil, ".jpg"); got != "2026-01-02 15.04.05.jpg" {
		t.Errorf("DestFileName = %q", got)
	}
}

// The whole M1 pipeline over the committed fixture card.
func TestImportFixtureCard(t *testing.T) {
	local := goproUTC.In(time.Local)
	cases := []struct{ fixture, want string }{
		{mt.SonyJPEG, filepath.Join("2025-05-30", "2025-05-31 01.30.12.JPG")},
		{mt.SonyARW, filepath.Join("2025-05-30", "Capture", "2025-05-31 01.30.12.ARW")},
		{mt.GoProMP4, filepath.Join(FolderDate(local, DefaultDayStartsAt), "Videos", local.Format(nameLayout)+" - 3840w - 29.970 - 12sec.MP4")},
	}
	rules := DefaultRules()
	staging := t.TempDir()
	for _, tc := range cases {
		path := fixture(t, tc.fixture)
		kind, ok := rules.KindOf(path)
		if !ok {
			t.Fatalf("%s: unrecognized kind", path)
		}
		before, err := QuickFingerprint(path)
		if err != nil {
			t.Fatal(err)
		}
		c, err := CaptureTime(path, kind)
		if err != nil {
			t.Fatal(err)
		}
		var meta *VideoMeta
		if kind == KindVideo {
			if meta, err = ReadVideoMeta(path); err != nil {
				t.Fatal(err)
			}
		}
		rel := filepath.Join(FolderDate(c.Time, DefaultDayStartsAt), TypeSubdir(kind, rules), DestFileName(c.Time, kind, meta, filepath.Ext(path)))
		if rel != tc.want {
			t.Errorf("%s: rel = %q, want %q", tc.fixture, rel, tc.want)
		}

		dir := filepath.Join(staging, filepath.Dir(rel))
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		dst, dup, err := FreeName(path, dir, filepath.Base(rel))
		if err != nil || dup {
			t.Fatalf("FreeName: dup=%v err=%v", dup, err)
		}
		got, err := CopyHashed(t.Context(), path, dst)
		if err != nil {
			t.Fatal(err)
		}
		want, err := HashFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: CopyHashed hash %s != HashFile %s", tc.fixture, got, want)
		}
		after, err := QuickFingerprint(path)
		if err != nil {
			t.Fatal(err)
		}
		if before != after {
			t.Errorf("%s: source changed", tc.fixture)
		}
	}
}

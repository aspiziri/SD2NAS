package media

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestCopyHashedCopiesAndHashes(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, "src.jpg", "hello")
	dst := filepath.Join(dir, "out.jpg")

	sum, err := CopyHashed(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"; sum != want {
		t.Errorf("sum = %s, want %s", sum, want)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Errorf("content = %q", data)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(fixedMtime) {
		t.Errorf("mtime = %v, want %v", info.ModTime(), fixedMtime)
	}
	if _, err := os.Stat(dst + PartialSuffix); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("partial left behind: %v", err)
	}
}

func TestCopyHashedLargeFile(t *testing.T) {
	dir := t.TempDir()
	src := writeBytes(t, dir, "big.bin", pattern(200000))
	dst := filepath.Join(dir, "big-out.bin")

	sum, err := CopyHashed(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	const want = "e24bc62381f1224fbbb74688663f8f9743b9680b193edd666835e97b06e730eb"
	if sum != want {
		t.Errorf("sum = %s, want %s", sum, want)
	}
	got, err := HashFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("HashFile(dst) = %s, want %s", got, want)
	}
}

func TestCopyFileRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, "src.jpg", "new content")
	dst := writeSourceFile(t, dir, "dst.jpg", "existing content")

	_, err := CopyHashed(src, dst)
	if err == nil {
		t.Fatal("CopyHashed overwrote an existing file")
	}
	data, err2 := os.ReadFile(dst)
	if err2 != nil {
		t.Fatal(err2)
	}
	if string(data) != "existing content" {
		t.Errorf("existing file was modified: %q", data)
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("error %v does not wrap fs.ErrExist", err)
	}
	if _, err := os.Stat(dst + PartialSuffix); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("partial left behind: %v", err)
	}
}

func TestCopyHashedStalePartial(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, "src.jpg", "new content")
	dst := filepath.Join(dir, "out.jpg")
	partial := writeSourceFile(t, dir, "out.jpg"+PartialSuffix, "stale")

	_, err := CopyHashed(src, dst)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want one wrapping fs.ErrExist", err)
	}
	data, err2 := os.ReadFile(partial)
	if err2 != nil {
		t.Fatal(err2)
	}
	if string(data) != "stale" {
		t.Errorf("stale partial was modified: %q", data)
	}
	if _, err := os.Stat(dst); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dst exists after failed copy: %v", err)
	}
}

func TestCopyHashedMissingSource(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.jpg")

	_, err := CopyHashed(filepath.Join(dir, "missing.jpg"), dst)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want one wrapping fs.ErrNotExist", err)
	}
	for _, p := range []string{dst, dst + PartialSuffix} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s exists: %v", p, err)
		}
	}
}

func TestCopyHashedMissingDestDir(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, "src.jpg", "x")
	dst := filepath.Join(dir, "nope", "out.jpg")

	if _, err := CopyHashed(src, dst); err == nil {
		t.Fatal("expected an error for a missing destination directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "nope")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("destination directory was created: %v", err)
	}
}

func TestCopyHashedSourceIsDirectory(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "sub")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.jpg")

	if _, err := CopyHashed(src, dst); err == nil {
		t.Fatal("expected an error for a directory source")
	}
	if _, err := os.Stat(dst + PartialSuffix); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("partial exists: %v", err)
	}
}

func TestCopyToPartialRemovesPartialOnReadError(t *testing.T) {
	boom := errors.New("card pulled")
	p := filepath.Join(t.TempDir(), "x.jpg"+PartialSuffix)
	r := io.MultiReader(strings.NewReader("abc"), iotest.ErrReader(boom))
	if _, err := copyToPartial(r, p); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("partial left behind after a failed copy: %v", err)
	}
}

func TestFinishPartialRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	partial := writeSourceFile(t, dir, "out.jpg"+PartialSuffix, "partial content")
	dst := writeSourceFile(t, dir, "out.jpg", "existing")

	err := finishPartial(partial, dst, fixedMtime)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want one wrapping fs.ErrExist", err)
	}
	data, err2 := os.ReadFile(dst)
	if err2 != nil {
		t.Fatal(err2)
	}
	if string(data) != "existing" {
		t.Errorf("dst was modified: %q", data)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Errorf("partial should remain for the caller to remove: %v", err)
	}
}

func TestRemovePartialJoinsRemovalFailure(t *testing.T) {
	// A non-empty directory cannot be removed with os.Remove on any OS.
	partial := filepath.Join(t.TempDir(), "x.jpg"+PartialSuffix)
	if err := os.Mkdir(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "child"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := errors.New("original failure")

	err := removePartial(partial, orig)
	if !errors.Is(err, orig) {
		t.Errorf("err %v does not wrap the original error", err)
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Path != partial {
		t.Errorf("err %v does not wrap the removal failure for %s", err, partial)
	}
}

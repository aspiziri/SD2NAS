package media

import (
	"path/filepath"
	"testing"
)

const collisionName = "2026-01-02 15.04.05.jpg"

func checkFreeName(t *testing.T, src, dir, wantBase string, wantDup bool) {
	t.Helper()
	got, dup, err := FreeName(src, dir, collisionName)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, wantBase); got != want || dup != wantDup {
		t.Errorf("FreeName = %q, %v; want %q, %v", got, dup, want, wantDup)
	}
}

func TestFreeNameFreeTarget(t *testing.T) {
	dir, srcDir := t.TempDir(), t.TempDir()
	src := writeSourceFile(t, srcDir, "src.jpg", "x")
	checkFreeName(t, src, dir, collisionName, false)
}

func TestFreeNameSkipsIdentical(t *testing.T) {
	dir, srcDir := t.TempDir(), t.TempDir()
	src := writeSourceFile(t, srcDir, "src.jpg", "same")
	writeSourceFile(t, dir, collisionName, "same")
	checkFreeName(t, src, dir, collisionName, true)
}

func TestFreeNameAddsSuffix(t *testing.T) {
	dir, srcDir := t.TempDir(), t.TempDir()
	src := writeSourceFile(t, srcDir, "src.jpg", "third")
	writeSourceFile(t, dir, collisionName, "first")
	writeSourceFile(t, dir, "2026-01-02 15.04.05-1.jpg", "second")
	checkFreeName(t, src, dir, "2026-01-02 15.04.05-2.jpg", false)
}

func TestFreeNameIdenticalAtSuffix(t *testing.T) {
	dir, srcDir := t.TempDir(), t.TempDir()
	src := writeSourceFile(t, srcDir, "src.jpg", "mine")
	writeSourceFile(t, dir, collisionName, "other")
	writeSourceFile(t, dir, "2026-01-02 15.04.05-1.jpg", "mine")
	checkFreeName(t, src, dir, "2026-01-02 15.04.05-1.jpg", true)
}

func TestFreeNameSameSizeDifferentContent(t *testing.T) {
	dir, srcDir := t.TempDir(), t.TempDir()
	src := writeSourceFile(t, srcDir, "src.jpg", "BBBB")
	writeSourceFile(t, dir, collisionName, "AAAA")
	checkFreeName(t, src, dir, "2026-01-02 15.04.05-1.jpg", false)
}

func TestFreeNameSourceMissing(t *testing.T) {
	dir, srcDir := t.TempDir(), t.TempDir()
	writeSourceFile(t, dir, collisionName, "exists")
	if _, _, err := FreeName(filepath.Join(srcDir, "missing.jpg"), dir, collisionName); err == nil {
		t.Fatal("expected an error when the source is missing")
	}
}

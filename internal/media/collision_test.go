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

// countingHash wraps HashFile and counts calls per path.
func countingHash(calls map[string]int) func(string) (string, error) {
	return func(p string) (string, error) {
		calls[p]++
		return HashFile(p)
	}
}

// A burst of same-size, different-content candidates must read the source
// once, not once per candidate.
func TestFreeNameHashesSourceOnce(t *testing.T) {
	dir, srcDir := t.TempDir(), t.TempDir()
	src := writeSourceFile(t, srcDir, "src.jpg", "SRC0")
	for n, content := range []string{"AAA0", "BBB0", "CCC0"} {
		writeSourceFile(t, dir, SuffixName(collisionName, n), content)
	}
	calls := map[string]int{}
	got, dup, err := freeName(src, dir, collisionName, countingHash(calls))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, SuffixName(collisionName, 3)); got != want || dup {
		t.Errorf("freeName = %q, %v; want %q, false", got, dup, want)
	}
	if calls[src] != 1 {
		t.Errorf("source hashed %d times, want 1", calls[src])
	}
	for n := 0; n < 3; n++ {
		if p := filepath.Join(dir, SuffixName(collisionName, n)); calls[p] != 1 {
			t.Errorf("%s hashed %d times, want 1", p, calls[p])
		}
	}
}

// The source is hashed lazily: never when no candidate has its size.
func TestFreeNameNoHashWithoutSameSize(t *testing.T) {
	dir, srcDir := t.TempDir(), t.TempDir()
	src := writeSourceFile(t, srcDir, "src.jpg", "source")
	writeSourceFile(t, dir, collisionName, "longer content")
	calls := map[string]int{}
	if _, _, err := freeName(src, dir, collisionName, countingHash(calls)); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Errorf("hash calls = %v, want none", calls)
	}
}

package media

import (
	"path/filepath"
	"testing"
)

const (
	sumHello   = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	sumEmpty   = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	sumPat200k = "e24bc62381f1224fbbb74688663f8f9743b9680b193edd666835e97b06e730eb"

	fpEmpty      = "af5570f5a1810b7af78caf4bc70a660f0df51e42baf91d4de5b2328de0e83dfc"
	fpHello      = "6bbb1026316442d9ade2fc3c01e4907ed7b2469a1c3670b3f473b65a89a7e49a"
	fpPat64k     = "66f10c0273c620c85b7ebfcdb03a6b3669e9d8d40ee1863400ab36cb06091fa1" // pattern(65536)
	fpPat200k    = "84e01709c427c78312b2469f33ae47bc3173a5fe9846d068a46e784c66bc9cd0" // pattern(200000)
	fpPat200kEnd = "dcdda296b8da6a058a69f6134681ac5dc358d577c86a263241e30fa0341aff7e" // pattern(200000) with byte 199999 ^= 0xFF
)

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"hello", []byte("hello"), sumHello},
		{"empty", []byte{}, sumEmpty},
		{"pat200k", pattern(200000), sumPat200k},
	}
	for _, tc := range tests {
		p := writeBytes(t, dir, tc.name, tc.data)
		got, err := HashFile(p)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
	if _, err := HashFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file: want error, got nil")
	}
}

func TestQuickFingerprintVectors(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"empty", []byte{}, fpEmpty},
		{"hello", []byte("hello"), fpHello},
		{"pat64k", pattern(65536), fpPat64k},
		{"pat200k", pattern(200000), fpPat200k},
	}
	for _, tc := range tests {
		p := writeBytes(t, dir, tc.name, tc.data)
		got, err := QuickFingerprint(p)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestQuickFingerprintSamplesHeadAndTailOnly(t *testing.T) {
	dir := t.TempDir()

	mid := pattern(200000)
	mid[100000] ^= 0xFF
	got, err := QuickFingerprint(writeBytes(t, dir, "mid", mid))
	if err != nil {
		t.Fatal(err)
	}
	if got != fpPat200k {
		t.Errorf("middle byte changed: got %s, want %s", got, fpPat200k)
	}

	end := pattern(200000)
	end[199999] ^= 0xFF
	got, err = QuickFingerprint(writeBytes(t, dir, "end", end))
	if err != nil {
		t.Fatal(err)
	}
	if got != fpPat200kEnd {
		t.Errorf("last byte changed: got %s, want %s", got, fpPat200kEnd)
	}

	got, err = QuickFingerprint(writeBytes(t, dir, "longer", pattern(200001)))
	if err != nil {
		t.Fatal(err)
	}
	if got == fpPat200k {
		t.Error("size change did not alter the fingerprint")
	}
}

func TestQuickFingerprintMissingFile(t *testing.T) {
	if _, err := QuickFingerprint(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("want error, got nil")
	}
}

// Same size but different bytes must NOT be treated as identical.
func TestFilesAreIdenticalComparesContent(t *testing.T) {
	dir := t.TempDir()
	p1 := writeSourceFile(t, dir, "a.jpg", "AAAA")
	p2 := writeSourceFile(t, dir, "b.jpg", "BBBB")

	identical, err := filesAreIdentical(p1, p2)
	if err != nil {
		t.Fatal(err)
	}
	if identical {
		t.Error("same-size files with different content reported as identical")
	}
}

func TestFilesAreIdenticalSameContent(t *testing.T) {
	dir := t.TempDir()
	a := writeSourceFile(t, dir, "a", "same")
	b := writeSourceFile(t, dir, "b", "same")
	got, err := filesAreIdentical(a, b)
	if err != nil || !got {
		t.Errorf("same content: got %v, %v; want true, nil", got, err)
	}

	c := writeSourceFile(t, dir, "c", "a")
	d := writeSourceFile(t, dir, "d", "bb")
	got, err = filesAreIdentical(c, d)
	if err != nil || got {
		t.Errorf("different size: got %v, %v; want false, nil", got, err)
	}

	if _, err := filesAreIdentical(a, filepath.Join(dir, "missing")); err == nil {
		t.Error("missing second path: want error, got nil")
	}
}

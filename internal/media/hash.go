package media

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

const fingerprintChunk = 64 << 10

// HashFile returns the lowercase hex SHA-256 of the whole file.
func HashFile(path string) (sum string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// filesAreIdentical reports whether two files have the same content. Files of
// different sizes are not identical; otherwise their full hashes are compared.
func filesAreIdentical(path1, path2 string) (bool, error) {
	s1, err := os.Stat(path1)
	if err != nil {
		return false, err
	}
	s2, err := os.Stat(path2)
	if err != nil {
		return false, err
	}
	if s1.Size() != s2.Size() {
		return false, nil
	}
	h1, err := HashFile(path1)
	if err != nil {
		return false, err
	}
	h2, err := HashFile(path2)
	if err != nil {
		return false, err
	}
	return h1 == h2, nil
}

// QuickFingerprint returns the hex SHA-256 of the file's size as 8 bytes
// big-endian, then its first min(64 KiB, size) bytes, then its last
// min(64 KiB, size) bytes (§6). For files under 128 KiB the two ranges
// overlap and both are hashed. It reads at most 128 KiB.
func QuickFingerprint(path string) (fp string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	h := sha256.New()
	var sz [8]byte
	binary.BigEndian.PutUint64(sz[:], uint64(size))
	h.Write(sz[:])
	head := min(size, fingerprintChunk)
	if _, err := io.CopyN(h, io.NewSectionReader(f, 0, head), head); err != nil {
		return "", fmt.Errorf("fingerprint %s: %w", path, err)
	}
	tailOff := max(0, size-fingerprintChunk)
	if _, err := io.CopyN(h, io.NewSectionReader(f, tailOff, size-tailOff), size-tailOff); err != nil {
		return "", fmt.Errorf("fingerprint %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

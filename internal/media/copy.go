package media

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"
)

// PartialSuffix is appended to a destination while it is being written.
// M2's startup cleanup deletes stray *PartialSuffix files.
const PartialSuffix = ".partial"

// CopyHashed copies src to dst and returns the hex SHA-256 of the bytes
// copied. It writes dst+PartialSuffix (created with O_EXCL), hashes while
// copying, fsyncs, sets the source's mtime, then renames into place. It
// never overwrites an existing dst and removes its partial on any failure.
// The parent directory must exist.
//
// Two failures wrap fs.ErrExist, so errors.Is(err, fs.ErrExist) holds for
// both:
//   - dst exists (checked before copying and again before the rename). The
//     error is not a *fs.PathError.
//   - a stale dst+PartialSuffix exists, left by an earlier interrupted copy.
//     It is not CopyHashed's, so it is left in place. The error is the
//     *fs.PathError from creating it, whose Path ends in PartialSuffix.
//
// To tell them apart, use errors.As with a *fs.PathError and check
// strings.HasSuffix(pathErr.Path, PartialSuffix).
func CopyHashed(src, dst string) (_ string, err error) {
	if err := mustNotExist(dst); err != nil {
		return "", err
	}
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := in.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("copy %s: not a regular file", src)
	}
	partial := dst + PartialSuffix
	sum, err := copyToPartial(in, partial)
	if err != nil {
		return "", err
	}
	// The partial exists and is ours: remove it on any later failure.
	if err := finishPartial(partial, dst, info.ModTime()); err != nil {
		return "", removePartial(partial, err)
	}
	return sum, nil
}

// removePartial removes the partial file CopyHashed created, after err
// occurred. A partial that is already gone (fs.ErrNotExist) is fine. Any
// other removal failure is joined onto err, so neither error is lost.
func removePartial(partial string, err error) error {
	if rmErr := os.Remove(partial); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return errors.Join(err, rmErr)
	}
	return err
}

// copyToPartial creates partial with O_EXCL (a stale partial is an
// fs.ErrExist error and is left alone, since it isn't ours), copies r into
// it while hashing, fsyncs and closes it. On failure it removes the partial.
func copyToPartial(r io.Reader, partial string) (string, error) {
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666) // UMASK decides
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); cerr != nil {
		err = errors.Join(err, cerr)
	}
	if err != nil {
		return "", removePartial(partial, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// finishPartial keeps the source's modified time, then renames into place
// unless something appeared at dst meanwhile (folder locks, §8, make that
// rare; this check keeps it from ever overwriting).
func finishPartial(partial, dst string, mtime time.Time) error {
	if err := os.Chtimes(partial, time.Now(), mtime); err != nil {
		return err
	}
	if err := mustNotExist(dst); err != nil {
		return err
	}
	return os.Rename(partial, dst)
}

func mustNotExist(path string) error {
	_, err := os.Lstat(path)
	if err == nil {
		return fmt.Errorf("%s: %w", path, fs.ErrExist)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

package media

import (
	"context"
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
// ctx is checked before starting and before every read, so a cancelled copy
// stops within one read; the error then wraps ctx.Err(). The source is
// closed before the partial is renamed into place, so whenever CopyHashed
// returns an error, dst was not created.
//
// Two failures wrap fs.ErrExist, so errors.Is(err, fs.ErrExist) holds for
// both:
//   - dst exists (checked before copying and again before the rename). The
//     error is not a *fs.PathError.
//   - a stale dst+PartialSuffix exists, left by an earlier interrupted copy.
//     It is not CopyHashed's, so it is left in place. The error is the
//     *fs.PathError from creating it, whose Path ends in PartialSuffix.
//
// ClassifyCopyError tells these apart, and also a missing source from a
// missing destination folder (both wrap fs.ErrNotExist).
func CopyHashed(ctx context.Context, src, dst string) (string, error) {
	return CopyHashedProgress(ctx, src, dst, nil)
}

// CopyHashedProgress is CopyHashed that calls progress (when not nil) with
// the number of bytes read from src so far, after every read that returned
// data. It is called on the calling goroutine, with strictly increasing
// values ending at the source's size on success, and not at all once ctx is
// done.
func CopyHashedProgress(ctx context.Context, src, dst string, progress func(done int64)) (string, error) {
	return copyHashedProgress(ctx, src, dst, openSource, progress)
}

// sourceFile is the part of *os.File CopyHashed uses; tests substitute it.
type sourceFile interface {
	io.Reader
	Stat() (fs.FileInfo, error)
	Close() error
}

// openSource must not return a typed-nil *os.File inside the interface.
func openSource(name string) (sourceFile, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func copyHashed(ctx context.Context, src, dst string, open func(string) (sourceFile, error)) (string, error) {
	return copyHashedProgress(ctx, src, dst, open, nil)
}

func copyHashedProgress(ctx context.Context, src, dst string, open func(string) (sourceFile, error), progress func(int64)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := mustNotExist(dst); err != nil {
		return "", err
	}
	in, err := open(src)
	if err != nil {
		return "", err
	}
	info, err := in.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("copy %s: not a regular file", src)
	}
	if err != nil {
		return "", errors.Join(err, in.Close())
	}
	partial := dst + PartialSuffix
	sum, err := copyToPartial(&ctxReader{ctx: ctx, r: in, progress: progress}, partial)
	// Close the card file before the partial becomes dst: nothing may fail
	// after the rename, so that any error means no dst.
	if cerr := in.Close(); cerr != nil {
		if err == nil { // the partial is complete and ours: remove it
			return "", removePartial(partial, cerr)
		}
		err = errors.Join(err, cerr)
	}
	if err != nil {
		return "", err // copyToPartial already removed its own partial
	}
	if err := finishPartial(partial, dst, info.ModTime()); err != nil {
		return "", removePartial(partial, err)
	}
	return sum, nil
}

// ctxReader stops a copy at the next read once ctx is done, and reports the
// bytes read so far to progress (when not nil). It has no WriteTo, so
// io.Copy reads through it chunk by chunk. It is used by pointer, so done
// keeps counting across reads.
type ctxReader struct {
	ctx      context.Context
	r        io.Reader
	progress func(int64)
	done     int64
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	if n > 0 {
		c.done += int64(n)
		if c.progress != nil {
			c.progress(c.done)
		}
	}
	return n, err
}

// CopyFailure says why CopyHashed failed, so a caller can choose to retry.
type CopyFailure int

const (
	CopyFailOther          CopyFailure = iota // anything else (I/O error, source not a regular file, ...)
	CopyFailCanceled                          // ctx was cancelled or timed out
	CopyFailSourceMissing                     // src does not exist (card pulled, file gone)
	CopyFailDestDirMissing                    // dst's directory does not exist
	CopyFailDestTaken                         // dst already exists
	CopyFailStalePartial                      // dst+PartialSuffix already exists, left by an earlier copy
)

// ClassifyCopyError classifies a non-nil error returned by CopyHashed(ctx, src, dst).
func ClassifyCopyError(err error, src, dst string) CopyFailure {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return CopyFailCanceled
	}
	var pathErr *fs.PathError
	hasPath := errors.As(err, &pathErr)
	switch {
	case errors.Is(err, fs.ErrExist):
		if hasPath && pathErr.Path == dst+PartialSuffix {
			return CopyFailStalePartial
		}
		return CopyFailDestTaken
	case errors.Is(err, fs.ErrNotExist) && hasPath:
		switch pathErr.Path {
		case src:
			return CopyFailSourceMissing
		case dst + PartialSuffix:
			return CopyFailDestDirMissing
		}
	}
	return CopyFailOther
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

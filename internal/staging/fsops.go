package staging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aspiziri/SD2NAS/internal/media"
)

// maxKept is how many leftover paths an answer names.
const maxKept = 20

// isJunk reports files that never keep a folder alive: what Windows and
// macOS leave behind over SMB, and stale partial copies (an import only
// writes partials into folders whose lock it holds).
func isJunk(name string) bool {
	switch strings.ToLower(name) {
	case ".ds_store", "thumbs.db", "desktop.ini":
		return true
	}
	return strings.HasPrefix(name, "._") || strings.HasSuffix(name, media.PartialSuffix)
}

// folderFiles lists the regular files below dir as sorted slash paths
// relative to dir, without junk. Symlinks and other special files are not
// listed and stay where they are. A missing dir has no files.
func folderFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if !d.Type().IsRegular() || isJunk(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	slices.Sort(out)
	return out, nil
}

// removeEmptyTree deletes dir when it holds nothing but folders and junk,
// deepest first; empty subfolders go even when dir stays. It reports
// whether dir is gone (a missing dir counts) and, when it isn't, up to
// maxKept of the paths (relative to dir) that kept it.
func removeEmptyTree(dir string) (gone bool, kept []string, err error) {
	var walk func(abs, rel string) (bool, error)
	walk = func(abs, rel string) (bool, error) {
		entries, err := os.ReadDir(abs)
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		empty := true
		var junk []string
		for _, e := range entries {
			child, childRel := filepath.Join(abs, e.Name()), path.Join(rel, e.Name())
			switch {
			case e.IsDir():
				childGone, err := walk(child, childRel)
				if err != nil {
					return false, err
				}
				empty = empty && childGone
			case e.Type().IsRegular() && isJunk(e.Name()):
				junk = append(junk, child)
			default:
				empty = false
				if len(kept) < maxKept {
					kept = append(kept, childRel)
				}
			}
		}
		if !empty {
			return false, nil
		}
		for _, j := range junk {
			if err := os.Remove(j); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return false, err
			}
		}
		if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		return true, nil
	}
	gone, err = walk(dir, "")
	if err != nil {
		return false, kept, fmt.Errorf("clean up %s: %w", dir, err)
	}
	return gone, kept, nil
}

// errRetryName: the name chosen for a copy was taken meanwhile; pick again.
var errRetryName = errors.New("destination taken during the copy")

// moveFile moves the regular file src into dir as name, or name-1, name-2…
// when that is taken by other content (CameraImport's collision loop,
// media.FreeName). When identical content is already there, src is removed
// and that path returned. A rename across filesystems falls back to copy,
// verify, delete (copyVerify). want is the hash src must have, "" when it
// isn't catalogued. It returns the new path.
func (s *Service) moveFile(ctx context.Context, src, dir, name, want string) (string, error) {
	// Checked before MkdirAll: a cancelled promote must not leave empty
	// folders in Archive/ (pre-flight D1).
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	for range 3 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		dst, dup, err := media.FreeName(src, dir, name)
		if err != nil {
			return "", err
		}
		if dup {
			// dst may be src itself (a case-only alias on a case-insensitive
			// disk, a link to it): the file is already in place, delete nothing.
			if same, err := sameFile(src, dst); err != nil {
				return "", err
			} else if same {
				return dst, nil
			}
			if err := os.Remove(src); err != nil {
				return "", fmt.Errorf("identical copy already at %s, but removing %s failed: %w", dst, src, err)
			}
			return dst, nil
		}
		err = s.rename(src, dst)
		if err == nil {
			return dst, nil
		}
		if !crossDevice(err) {
			return "", err
		}
		dst, err = s.copyVerify(ctx, src, dst, want)
		if errors.Is(err, errRetryName) {
			continue
		}
		return dst, err
	}
	return "", fmt.Errorf("move %s: destination names in %s kept being taken", src, dir)
}

// sameFile reports whether a and b are the same file on disk.
func sameFile(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(ai, bi), nil
}

// copyVerify copies src to dst (media.CopyHashed: a partial, fsync, rename,
// never overwriting), hashes dst again, and removes src only when the copy
// matches what was read and, if want isn't "", the catalog's hash.
func (s *Service) copyVerify(ctx context.Context, src, dst, want string) (string, error) {
	sum, err := media.CopyHashed(ctx, src, dst)
	if err != nil {
		switch media.ClassifyCopyError(err, src, dst) {
		case media.CopyFailDestTaken:
			return "", errRetryName
		case media.CopyFailStalePartial:
			// We hold the folder's lock (in Archive/: the only promote job),
			// so this partial is left over from an interrupted copy.
			if rmErr := os.Remove(dst + media.PartialSuffix); rmErr != nil {
				return "", errors.Join(err, rmErr)
			}
			s.log.Warn("removed a stale partial", "path", dst+media.PartialSuffix)
			return "", errRetryName
		}
		return "", err
	}
	got, err := hashFile(ctx, dst)
	if err == nil && (got != sum || (want != "" && got != want)) {
		err = fmt.Errorf("the copy of %s does not match (read %s, copy %s, catalog %s)", src, sum, got, want)
	}
	if err != nil {
		if rmErr := os.Remove(dst); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
		return "", err
	}
	if err := os.Remove(src); err != nil {
		return dst, fmt.Errorf("copied and verified %s, but removing the original failed: %w", dst, err)
	}
	return dst, nil
}

// hashFile is media.HashFile that stops when ctx ends, so shutdown never
// waits for a multi-gigabyte video to be read.
func hashFile(ctx context.Context, p string) (sum string, err error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx, f}); err != nil {
		return "", fmt.Errorf("hash %s: %w", p, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// syncDirs makes the renames and removals in dirs durable (Decision 16).
// Folders that no longer exist are skipped.
func syncDirs(dirs map[string]bool) error {
	var errs []error
	for d := range dirs {
		if _, err := os.Stat(d); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := media.SyncDir(d); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

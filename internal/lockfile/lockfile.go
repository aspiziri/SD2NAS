// Package lockfile takes an exclusive, non-blocking lock on a file, so a second SD2NAS on the same config folder stops at startup (§16 M6). The operating system drops the lock when the process ends, however it ends.
package lockfile

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrHeld is matched by every *HeldError.
var ErrHeld = errors.New("lockfile: held by another process")

// ErrUnsupported means the file system or operating system has no file locks.
var ErrUnsupported = errors.New("lockfile: locking is not supported here")

// HeldError says another process holds the lock.
type HeldError struct {
	Path   string
	Holder string // the holder's info line; "" if unreadable
}

func (e *HeldError) Error() string {
	holder := e.Holder
	if holder == "" {
		holder = "unknown"
	}
	return fmt.Sprintf("%s is locked by another process (%s)", e.Path, holder)
}

// Is reports whether target is ErrHeld.
func (e *HeldError) Is(target error) bool { return target == ErrHeld }

// Lock is a held lock.
type Lock struct {
	f *os.File
}

// Acquire locks path, creating it if needed, and writes info as its first
// line. It never waits: a lock held elsewhere is a *HeldError.
func Acquire(path, info string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lockfile: %w", err)
	}
	if err := lockFile(f); err != nil {
		closeErr := f.Close()
		if errors.Is(err, ErrHeld) {
			return nil, errors.Join(&HeldError{Path: path, Holder: holderOf(path)}, closeErr)
		}
		return nil, errors.Join(fmt.Errorf("lockfile: lock %s: %w", path, err), closeErr)
	}
	err = f.Truncate(0)
	if err == nil {
		_, err = f.WriteAt([]byte(info+"\n"), 0)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("lockfile: write %s: %w", path, err), unlockFile(f), f.Close())
	}
	return &Lock{f: f}, nil
}

// Release unlocks and closes the file. The file itself stays.
func (l *Lock) Release() error {
	return errors.Join(unlockFile(l.f), l.f.Close())
}

// holderOf returns the first line of the lock file, "" on any error.
func holderOf(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	first, _, _ := strings.Cut(string(buf[:n]), "\n")
	return strings.TrimSpace(first)
}

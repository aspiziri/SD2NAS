package lockfile

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately               = 0x1
	lockfileExclusiveLock                 = 0x2
	errorLockViolation      syscall.Errno = 33 // ERROR_LOCK_VIOLATION
)

// lockRange is the one byte locked: far past the end of the file, so the
// holder line at the start stays readable by a refused process (Decision 14).
func lockRange() *syscall.Overlapped { return &syscall.Overlapped{OffsetHigh: 1} }

func lockFile(f *os.File) error {
	r, _, err := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0,
		uintptr(unsafe.Pointer(lockRange())))
	if r != 0 {
		return nil
	}
	if errors.Is(err, errorLockViolation) {
		return ErrHeld
	}
	return err
}

func unlockFile(f *os.File) error {
	r, _, err := procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(lockRange())))
	if r == 0 {
		return err
	}
	return nil
}

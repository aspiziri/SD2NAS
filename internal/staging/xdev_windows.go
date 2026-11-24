//go:build windows

package staging

import (
	"errors"
	"syscall"
)

// errNotSameDevice is ERROR_NOT_SAME_DEVICE: MoveFileEx can't move a file
// to another volume.
const errNotSameDevice = syscall.Errno(17)

// crossDevice reports a rename that would cross filesystems (§13).
func crossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, errNotSameDevice)
}

//go:build !windows

package staging

import (
	"errors"
	"syscall"
)

// crossDevice reports a rename that would cross filesystems (§13).
func crossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }

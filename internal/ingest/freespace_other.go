//go:build !linux && !darwin && !windows

package ingest

import (
	"errors"
	"fmt"
)

// FreeSpace is not supported on this platform.
func FreeSpace(path string) (uint64, error) {
	return 0, fmt.Errorf("free space of %s: %w", path, errors.ErrUnsupported)
}

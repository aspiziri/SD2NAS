//go:build !linux && !darwin && !windows

package lockfile

import "os"

func lockFile(*os.File) error { return ErrUnsupported }

func unlockFile(*os.File) error { return nil }

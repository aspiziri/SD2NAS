//go:build linux || darwin

package ingest

import (
	"io/fs"
	"syscall"
)

// FreeSpace returns the bytes available to this user on path's filesystem.
func FreeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil // Bsize is int64 on linux, uint32 on darwin
}

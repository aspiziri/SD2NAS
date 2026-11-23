//go:build windows

package ingest

import (
	"io/fs"
	"syscall"
	"unsafe"
)

var procGetDiskFreeSpaceExW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// FreeSpace returns the bytes available to this user on path's filesystem.
func FreeSpace(path string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var avail uint64
	r, _, callErr := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&avail)), 0, 0)
	if r == 0 {
		return 0, &fs.PathError{Op: "GetDiskFreeSpaceEx", Path: path, Err: callErr}
	}
	return avail, nil
}

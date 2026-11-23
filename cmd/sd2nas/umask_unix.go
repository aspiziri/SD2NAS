//go:build unix

package main

import "syscall"

// setUmask applies the UMASK env value so new files and folders get the
// permissions Unraid expects (000 keeps them editable over SMB).
func setUmask(mask int) error {
	syscall.Umask(mask)
	return nil
}

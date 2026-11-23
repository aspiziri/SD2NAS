//go:build !unix

package media

import (
	"fmt"
	"os"
)

// SyncDir makes a create or rename inside dir durable: on Unix it fsyncs
// the directory; elsewhere it only checks that dir is a directory. Both
// return an error wrapping fs.ErrNotExist for a missing dir and an error
// for a path that isn't a directory.
//
// NTFS journals metadata itself, and Windows cannot fsync a directory
// handle without backup semantics, so there is nothing to flush here.
func SyncDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("sync %s: not a directory", dir)
	}
	return nil
}

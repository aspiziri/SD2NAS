//go:build unix

package media

import (
	"errors"
	"fmt"
	"os"
)

// SyncDir makes a create or rename inside dir durable: on Unix it fsyncs
// the directory; elsewhere it only checks that dir is a directory. Both
// return an error wrapping fs.ErrNotExist for a missing dir and an error
// for a path that isn't a directory.
func SyncDir(dir string) (err error) {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("sync %s: not a directory", dir)
	}
	return f.Sync()
}

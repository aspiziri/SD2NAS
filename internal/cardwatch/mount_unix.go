//go:build linux || darwin

package cardwatch

import (
	"fmt"
	"os"
	"syscall"
)

// isMount reports whether dir is on a different device than root, which is
// what a mounted card looks like under the cards folder.
func isMount(dir, root string) (bool, error) {
	d, err := os.Stat(dir)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", dir, err)
	}
	r, err := os.Stat(root)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", root, err)
	}
	ds, ok1 := d.Sys().(*syscall.Stat_t)
	rs, ok2 := r.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false, fmt.Errorf("stat %s: no device information", dir)
	}
	return uint64(ds.Dev) != uint64(rs.Dev), nil // Dev is int32 on darwin, uint64 on linux
}

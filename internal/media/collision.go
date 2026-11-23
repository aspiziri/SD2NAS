package media

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// FreeName is CameraImport's collision loop. It tries name, then
// SuffixName(name, 1), SuffixName(name, 2), … in dir and returns the first
// path that doesn't exist (duplicate == false), or the first existing path
// whose content is identical to src (duplicate == true: skip the copy).
func FreeName(src, dir, name string) (dst string, duplicate bool, err error) {
	for n := 0; ; n++ {
		path := filepath.Join(dir, SuffixName(name, n))
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return path, false, nil
			}
			return "", false, err
		}
		same, err := filesAreIdentical(src, path)
		if err != nil {
			return "", false, err
		}
		if same {
			return path, true, nil
		}
	}
}

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
	return freeName(src, dir, name, HashFile)
}

// freeName is FreeName with the file hasher passed in, so tests can count
// how often each file is read. The source is stat'ed when the first
// candidate exists and hashed at most once, when the first candidate of the
// same size is found; each same-size candidate is hashed once.
func freeName(src, dir, name string, hash func(string) (string, error)) (string, bool, error) {
	var srcInfo fs.FileInfo
	var srcSum string
	for n := 0; ; n++ {
		path := filepath.Join(dir, SuffixName(name, n))
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return path, false, nil
			}
			return "", false, err
		}
		if srcInfo == nil {
			if srcInfo, err = os.Stat(src); err != nil {
				return "", false, err
			}
		}
		if info.Size() != srcInfo.Size() {
			continue
		}
		if srcSum == "" {
			if srcSum, err = hash(src); err != nil {
				return "", false, err
			}
		}
		sum, err := hash(path)
		if err != nil {
			return "", false, err
		}
		if sum == srcSum {
			return path, true, nil
		}
	}
}

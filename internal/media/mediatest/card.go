package mediatest

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// CopyCard copies the committed card testdata/cards/<label> into
// dst/<label>, keeping modified times, and returns the new card root.
// testdata is the repo's testdata directory (tests at internal/X pass
// filepath.Join("..", "..", "testdata")).
func CopyCard(testdata, label, dst string) (string, error) {
	src := filepath.Join(testdata, "cards", label)
	root := filepath.Join(dst, label)
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o777)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := copyFile(path, target); err != nil {
			return err
		}
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	})
	if err != nil {
		return "", err
	}
	return root, nil
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	_, err = io.Copy(out, in)
	return err
}
